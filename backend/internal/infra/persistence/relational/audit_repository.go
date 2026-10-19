package relational

import (
	"context"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

type AuditRepository struct{ db *Database }

func NewAuditRepository(db *Database) *AuditRepository { return &AuditRepository{db: db} }

const (
	auditInsertBatchSize   = 20
	auditLookupBatchSize   = 500
	attemptInsertBatchSize = 40
	auditPurgeBatchSize    = 1000
	auditSuccessPredicate  = "status_code >= 200 AND status_code < 300 AND (error_code IS NULL OR error_code = '')"
	auditSuccessAggregate  = "COALESCE(SUM(CASE WHEN " + auditSuccessPredicate + " THEN 1 ELSE 0 END), 0)"
)

func (r *AuditRepository) SumTokensByAccountsSince(ctx context.Context, accountIDs []uint64, since time.Time) (map[uint64]int64, error) {
	result := make(map[uint64]int64, len(accountIDs))
	if len(accountIDs) == 0 {
		return result, nil
	}
	var rows []struct {
		AccountID   uint64
		TotalTokens int64
	}
	err := r.db.db.WithContext(ctx).
		Model(&requestAuditModel{}).
		Select("account_id, COALESCE(SUM(total_tokens), 0) AS total_tokens").
		Where("account_id IN ? AND created_at >= ? AND total_tokens > 0", accountIDs, since).
		Group("account_id").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.AccountID] = row.TotalTokens
	}
	return result, nil
}

func (r *AuditRepository) List(ctx context.Context, offset, limit int) ([]audit.Record, int64, error) {
	var total int64
	query := r.db.db.WithContext(ctx).Model(&requestAuditModel{})
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []requestAuditModel
	if err := query.Omit("request_headers_json").Order("created_at DESC, id DESC").Offset(offset).Limit(limit).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	out := make([]audit.Record, 0, len(rows))
	for _, row := range rows {
		out = append(out, toAuditDomain(row))
	}
	return out, total, nil
}

func (r *AuditRepository) Get(ctx context.Context, id uint64) (audit.Record, error) {
	var row requestAuditModel
	if err := r.db.db.WithContext(ctx).First(&row, id).Error; err != nil {
		return audit.Record{}, mapError(err)
	}
	var attemptRows []requestAuditAttemptModel
	if err := r.db.db.WithContext(ctx).Where("audit_id = ?", id).Order("number ASC").Find(&attemptRows).Error; err != nil {
		return audit.Record{}, err
	}
	value := toAuditDomain(row)
	value.Attempts = make([]audit.Attempt, 0, len(attemptRows))
	for _, attemptRow := range attemptRows {
		attempt, err := toAuditAttemptDomain(attemptRow)
		if err != nil {
			return audit.Record{}, err
		}
		value.Attempts = append(value.Attempts, attempt)
	}
	return value, nil
}

// ListCursor 使用“排序值 + ID”复合游标读取审计，避免深分页和同值记录漏读。
func (r *AuditRepository) ListCursor(ctx context.Context, input repository.AuditCursorQuery) ([]audit.Record, bool, error) {
	query := r.db.db.WithContext(ctx).Model(&requestAuditModel{})
	query = applyAuditQuery(query, input.Search, input.Start, input.End, input.Filter)
	fields := map[string]sortSpec{
		"request":   {expression: "request_audits.request_id"},
		"model":     {expression: "LOWER(request_audits.model_public_id)"},
		"billing":   {expression: "CASE WHEN request_audits.price_id = 0 AND request_audits.cost_in_usd_ticks > 0 THEN request_audits.cost_in_usd_ticks ELSE request_audits.estimated_cost_in_usd_ticks END", defaultDirection: repository.SortDescending},
		"tokens":    {expression: "request_audits.total_tokens", defaultDirection: repository.SortDescending},
		"status":    {expression: "request_audits.status_code"},
		"mode":      {expression: "CASE WHEN request_audits.streaming = TRUE THEN 1 ELSE 0 END"},
		"duration":  {expression: "request_audits.duration_ms", defaultDirection: repository.SortDescending},
		"createdAt": {expression: "request_audits.created_at", defaultDirection: repository.SortDescending},
	}
	fallback := sortSpec{expression: "request_audits.created_at", defaultDirection: repository.SortDescending}
	spec, direction := stableSortSpec(input.Sort, fields, fallback)
	if input.Cursor != nil {
		comparison := ">"
		if direction == "DESC" {
			comparison = "<"
		}
		query = query.Where("("+spec.expression+" "+comparison+" ? OR ("+spec.expression+" = ? AND request_audits.id "+comparison+" ?))", input.Cursor.Value, input.Cursor.Value, input.Cursor.ID)
	}
	var rows []requestAuditModel
	query = applyStableSort(query, input.Sort, fields, fallback, "request_audits.id")
	if err := query.Omit("request_headers_json").Limit(input.Limit + 1).Find(&rows).Error; err != nil {
		return nil, false, err
	}
	hasMore := len(rows) > input.Limit
	if hasMore {
		rows = rows[:input.Limit]
	}
	out := make([]audit.Record, 0, len(rows))
	for _, row := range rows {
		out = append(out, toAuditDomain(row))
	}
	return out, hasMore, nil
}

func (r *AuditRepository) Summarize(ctx context.Context, input repository.AuditSummaryQuery) (audit.Summary, error) {
	var aggregate struct {
		Requests                int64
		SuccessfulRequests      int64
		InputTokens             int64
		CachedInputTokens       int64
		OutputTokens            int64
		ReasoningTokens         int64
		TotalTokens             int64
		DurationMS              int64
		EstimatedCostInUSDTicks int64
		PricedRequests          int64
		UnpricedRequests        int64
		PricedTokens            int64
		UnpricedTokens          int64
		CustomPricedRequests    int64
	}
	query := applyAuditQuery(r.db.db.WithContext(ctx).Model(&requestAuditModel{}), input.Search, input.Start, input.End, input.Filter)
	if err := query.Select(`
		COUNT(*) AS requests,
		` + auditSuccessAggregate + ` AS successful_requests,
		COALESCE(SUM(input_tokens), 0) AS input_tokens,
		COALESCE(SUM(cached_input_tokens), 0) AS cached_input_tokens,
		COALESCE(SUM(output_tokens), 0) AS output_tokens,
		COALESCE(SUM(reasoning_tokens), 0) AS reasoning_tokens,
		COALESCE(SUM(total_tokens), 0) AS total_tokens,
		COALESCE(SUM(duration_ms), 0) AS duration_ms,
		COALESCE(SUM(estimated_cost_in_usd_ticks), 0) AS estimated_cost_in_usd_ticks,
		COALESCE(SUM(CASE WHEN COALESCE(pricing_model, '') <> '' THEN 1 ELSE 0 END), 0) AS priced_requests,
		COALESCE(SUM(CASE WHEN COALESCE(pricing_model, '') = '' THEN 1 ELSE 0 END), 0) AS unpriced_requests,
		COALESCE(SUM(CASE WHEN COALESCE(pricing_model, '') <> '' THEN total_tokens ELSE 0 END), 0) AS priced_tokens,
		COALESCE(SUM(CASE WHEN COALESCE(pricing_model, '') = '' THEN total_tokens ELSE 0 END), 0) AS unpriced_tokens,
		COALESCE(SUM(CASE WHEN COALESCE(pricing_model, '') <> '' AND price_id <> 0 THEN 1 ELSE 0 END), 0) AS custom_priced_requests`).Scan(&aggregate).Error; err != nil {
		return audit.Summary{}, err
	}
	result := audit.Summary{
		Requests: aggregate.Requests, SuccessfulRequests: aggregate.SuccessfulRequests, FailedRequests: aggregate.Requests - aggregate.SuccessfulRequests,
		InputTokens: aggregate.InputTokens, CachedInputTokens: aggregate.CachedInputTokens, OutputTokens: aggregate.OutputTokens,
		ReasoningTokens: aggregate.ReasoningTokens, TotalTokens: aggregate.TotalTokens, DurationMS: aggregate.DurationMS,
		EstimatedCostInUSDTicks: aggregate.EstimatedCostInUSDTicks, PricedRequests: aggregate.PricedRequests,
		UnpricedRequests: aggregate.UnpricedRequests, PricedTokens: aggregate.PricedTokens, UnpricedTokens: aggregate.UnpricedTokens,
		CustomPricedRequests: aggregate.CustomPricedRequests,
	}
	return result, nil
}

func applyAuditQuery(query *gorm.DB, search string, start, end time.Time, filter repository.AuditListFilter) *gorm.DB {
	if value := strings.TrimSpace(search); value != "" {
		pattern := "%" + strings.ToLower(value) + "%"
		query = query.Where("LOWER(request_id) LIKE ? OR LOWER(model_public_id) LIKE ? OR LOWER(model_upstream_model) LIKE ? OR LOWER(client_ip) LIKE ? OR LOWER(egress_node_name) LIKE ?", pattern, pattern, pattern, pattern, pattern)
	}
	if !start.IsZero() {
		query = query.Where("created_at >= ?", start)
	}
	if !end.IsZero() {
		query = query.Where("created_at < ?", end)
	}
	if value := strings.TrimSpace(filter.Model); value != "" {
		query = query.Where("model_public_id = ? OR model_upstream_model = ?", value, value)
	}
	if value := strings.TrimSpace(filter.Key); value != "" {
		pattern := "%" + strings.ToLower(value) + "%"
		query = query.Where("LOWER(client_key_name) LIKE ? OR CAST(client_key_id AS TEXT) LIKE ?", pattern, pattern)
	}
	if value := strings.TrimSpace(filter.Account); value != "" {
		pattern := "%" + strings.ToLower(value) + "%"
		query = query.Where("LOWER(account_name) LIKE ? OR CAST(account_id AS TEXT) LIKE ?", pattern, pattern)
	}
	if value := strings.TrimSpace(filter.Provider); value != "" {
		query = query.Where("provider = ?", value)
	}
	if value := strings.TrimSpace(filter.Operation); value != "" {
		query = query.Where("operation = ?", value)
	}
	switch filter.Status {
	case "success", "2xx":
		query = query.Where(auditSuccessPredicate)
	case "clientError", "4xx":
		query = query.Where("status_code >= 400 AND status_code < 500")
	case "serverError", "5xx":
		query = query.Where("status_code >= 500 AND status_code < 600")
	case "other":
		// 保留真实 HTTP 状态：2xx 响应头之后的流失败通过 error_code 识别；
		// 同时覆盖不属于 2xx/4xx/5xx 的状态段。status_code < 100 兼容
		// 曾运行过早期实现并写入 0 的开发数据库，但新记录仍只允许 100..599。
		query = query.Where("(status_code >= 200 AND status_code < 300 AND error_code IS NOT NULL AND error_code <> '') OR status_code < 200 OR (status_code >= 300 AND status_code < 400) OR status_code >= 600")
	}
	switch filter.Mode {
	case "stream":
		query = query.Where("streaming = ?", true)
	case "nonStream":
		query = query.Where("streaming = ?", false)
	}
	return query
}

func (r *AuditRepository) PurgeOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	var totalDeleted int64
	for {
		var batchDeleted int64
		var batchSelected int
		err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var ids []uint64
			if err := tx.Model(&requestAuditModel{}).
				Select("id").
				Where("created_at < ?", cutoff).
				Order("id ASC").
				Limit(auditPurgeBatchSize).
				Pluck("id", &ids).Error; err != nil {
				return err
			}
			if len(ids) == 0 {
				return nil
			}
			batchSelected = len(ids)
			if err := tx.Where("audit_id IN ?", ids).Delete(&requestAuditAttemptModel{}).Error; err != nil {
				return err
			}
			res := tx.Where("id IN ? AND created_at < ?", ids, cutoff).Delete(&requestAuditModel{})
			if res.Error != nil {
				return res.Error
			}
			batchDeleted = res.RowsAffected
			return nil
		})
		if err != nil {
			return totalDeleted, err
		}
		totalDeleted += batchDeleted
		// Use the number selected rather than RowsAffected to decide whether
		// another batch may exist. In a multi-instance deployment another
		// cleaner can delete part of this batch between selection and deletion.
		if batchSelected < auditPurgeBatchSize {
			return totalDeleted, nil
		}
		if err := ctx.Err(); err != nil {
			return totalDeleted, err
		}
	}
}
