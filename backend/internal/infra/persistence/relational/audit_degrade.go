package relational

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

type degradeTotalsRow struct {
	Hits         int64   `gorm:"column:hits"`
	Accounts     int64   `gorm:"column:accounts"`
	StillEnabled int64   `gorm:"column:still_enabled"`
	Disabled     int64   `gorm:"column:disabled"`
	Deleted      int64   `gorm:"column:deleted"`
	Hard         int64   `gorm:"column:hard"`
	Soft         int64   `gorm:"column:soft"`
	Burst        int64   `gorm:"column:burst"`
	Thinking     int64   `gorm:"column:thinking"`
	MaxTPS       float64 `gorm:"column:max_tps"`
}

type degradeAccountRow struct {
	ID                 uint64           `gorm:"column:id"`
	Name               string           `gorm:"column:name"`
	Email              string           `gorm:"column:email"`
	Hits               int64            `gorm:"column:hits"`
	MaxTPS             float64          `gorm:"column:max_tps"`
	Burst              int64            `gorm:"column:burst"`
	Soft               int64            `gorm:"column:soft"`
	Hard               int64            `gorm:"column:hard"`
	Thinking           int64            `gorm:"column:thinking"`
	Last               degradeTimestamp `gorm:"column:last_seen"`
	Enabled            bool             `gorm:"column:enabled"`
	Found              bool             `gorm:"column:found"`
	BuildBotFlagSource int              `gorm:"column:build_bot_flag_source"`
}

type degradeAccountLeaseRow struct {
	AccountID     uint64           `gorm:"column:account_id"`
	CooldownUntil degradeTimestamp `gorm:"column:cooldown_until"`
}

type degradeTimestamp time.Time

func (value *degradeTimestamp) Scan(input any) error {
	parsed, err := parseDegradeTimestamp(input)
	if err != nil {
		return err
	}
	*value = degradeTimestamp(parsed)
	return nil
}

func parseDegradeTimestamp(input any) (time.Time, error) {
	if parsed, ok := input.(time.Time); ok {
		return parsed, nil
	}
	var raw string
	switch value := input.(type) {
	case string:
		raw = value
	case []byte:
		raw = string(value)
	case nil:
		return time.Time{}, nil
	default:
		return time.Time{}, fmt.Errorf("unsupported degrade timestamp type %T", input)
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05",
	} {
		parsed, err := time.Parse(layout, raw)
		if err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid degrade timestamp")
}

type degradeNodeRow struct {
	Name     string  `gorm:"column:name"`
	Hits     int64   `gorm:"column:hits"`
	Accounts int64   `gorm:"column:accounts"`
	MaxTPS   float64 `gorm:"column:max_tps"`
}

type degradeBucketRow struct {
	Index  int   `gorm:"column:bucket_index"`
	Count  int64 `gorm:"column:count"`
	Severe int64 `gorm:"column:severe"`
}

type degradeAccountNodeRow struct {
	AccountID uint64 `gorm:"column:account_id"`
	Name      string `gorm:"column:name"`
}

type degradeEventRow struct {
	ID             uint64    `gorm:"column:id"`
	RequestID      string    `gorm:"column:request_id"`
	AccountID      *uint64   `gorm:"column:account_id"`
	AccountName    string    `gorm:"column:account_name"`
	EgressNodeID   *uint64   `gorm:"column:egress_node_id"`
	EgressNodeName string    `gorm:"column:egress_node_name"`
	OutputTokens   int64     `gorm:"column:output_tokens"`
	TPS            float64   `gorm:"column:tps"`
	Class          string    `gorm:"column:degrade_class"`
	CreatedAt      time.Time `gorm:"column:created_at"`
	Model          string    `gorm:"column:model_upstream_model"`
}

func (r *AuditRepository) SummarizeDegrade(ctx context.Context, input repository.DegradeSummaryQuery) (repository.DegradeSummaryResult, error) {
	if input.AccountLimit <= 0 || input.AccountLimit > 100 {
		input.AccountLimit = 50
	}
	if input.AccountOffset < 0 {
		input.AccountOffset = 0
	}
	if input.RecentLimit <= 0 || input.RecentLimit > 200 {
		input.RecentLimit = 80
	}
	var result repository.DegradeSummaryResult
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		classified := r.degradeClassifiedQuery(tx, input)
		var totals degradeTotalsRow
		if err := tx.Table("(?) AS d", classified).
			Select(`COUNT(*) AS hits,
				COUNT(DISTINCT d.account_id) AS accounts,
				COUNT(DISTINCT CASE WHEN p.id IS NOT NULL AND p.enabled = TRUE THEN d.account_id END) AS still_enabled,
				COUNT(DISTINCT CASE WHEN p.id IS NOT NULL AND p.enabled = FALSE THEN d.account_id END) AS disabled,
				COUNT(DISTINCT CASE WHEN p.id IS NULL THEN d.account_id END) AS deleted,
				COALESCE(SUM(CASE WHEN d.degrade_class = 'hard_tps' THEN 1 ELSE 0 END), 0) AS hard,
				COALESCE(SUM(CASE WHEN d.degrade_class = 'soft_tps' THEN 1 ELSE 0 END), 0) AS soft,
				COALESCE(SUM(CASE WHEN d.degrade_class = 'buffered_burst' THEN 1 ELSE 0 END), 0) AS burst,
				COALESCE(SUM(CASE WHEN d.degrade_class = 'missing_thinking' THEN 1 ELSE 0 END), 0) AS thinking,
				COALESCE(MAX(d.tps), 0) AS max_tps`).
			Joins("LEFT JOIN provider_accounts p ON p.id = d.account_id").
			Scan(&totals).Error; err != nil {
			return err
		}
		result.Totals = repository.DegradeTotals{
			Hits: totals.Hits, Accounts: totals.Accounts, StillEnabled: totals.StillEnabled,
			Disabled: totals.Disabled, Deleted: totals.Deleted, Hard: totals.Hard, Soft: totals.Soft,
			Burst: totals.Burst, Thinking: totals.Thinking, MaxTPS: totals.MaxTPS,
		}

		accountQuery := r.degradeAccountAggregate(tx, classified, input)
		if err := tx.Table("(?) AS filtered_accounts", accountQuery).Count(&result.AccountTotal).Error; err != nil {
			return err
		}
		accountOffset := input.AccountOffset
		if result.AccountTotal > 0 && int64(accountOffset) >= result.AccountTotal {
			accountOffset = int((result.AccountTotal-1)/int64(input.AccountLimit)) * input.AccountLimit
		}
		result.AccountOffset = accountOffset
		var accountRows []degradeAccountRow
		if err := accountQuery.Order("hits DESC, max_tps DESC, id ASC").
			Offset(accountOffset).Limit(input.AccountLimit).Scan(&accountRows).Error; err != nil {
			return err
		}
		accountIDs := make([]uint64, 0, len(accountRows))
		accountsByID := make(map[uint64]*repository.DegradeAccount, len(accountRows))
		result.Accounts = make([]repository.DegradeAccount, 0, len(accountRows))
		for _, row := range accountRows {
			result.Accounts = append(result.Accounts, repository.DegradeAccount{
				ID: row.ID, Name: row.Name, Email: row.Email, Hits: row.Hits, MaxTPS: row.MaxTPS,
				Burst: row.Burst, Soft: row.Soft, Hard: row.Hard, Thinking: row.Thinking, Last: time.Time(row.Last), Enabled: row.Enabled,
				Found: row.Found, BuildBotFlagSource: row.BuildBotFlagSource,
			})
			accountIDs = append(accountIDs, row.ID)
			accountsByID[row.ID] = &result.Accounts[len(result.Accounts)-1]
		}
		if len(accountIDs) > 0 {
			var leaseRows []degradeAccountLeaseRow
			if err := tx.Table("account_egress_lease_blocks").
				Select("account_id, MAX(cooldown_until) AS cooldown_until").
				Where("account_id IN ? AND cooldown_until > ?", accountIDs, input.End).
				Group("account_id").Scan(&leaseRows).Error; err != nil {
				return err
			}
			for _, row := range leaseRows {
				if account := accountsByID[row.AccountID]; account != nil {
					cooldownUntil := time.Time(row.CooldownUntil).UTC()
					account.LeaseCooldownUntil = &cooldownUntil
				}
			}

			var nodeRows []degradeAccountNodeRow
			if err := tx.Table("(?) AS d", classified).
				Select("d.account_id, COALESCE(NULLIF(d.egress_node_name, ''), '?') AS name").
				Where("d.account_id IN ?", accountIDs).
				Group("d.account_id, COALESCE(NULLIF(d.egress_node_name, ''), '?')").
				Order("d.account_id ASC, name ASC").Scan(&nodeRows).Error; err != nil {
				return err
			}
			for _, row := range nodeRows {
				if account := accountsByID[row.AccountID]; account != nil {
					account.Nodes = append(account.Nodes, row.Name)
				}
			}
		}

		var nodeRows []degradeNodeRow
		if err := tx.Table("(?) AS d", classified).
			Select("COALESCE(NULLIF(d.egress_node_name, ''), '?') AS name, COUNT(*) AS hits, COUNT(DISTINCT d.account_id) AS accounts, COALESCE(MAX(d.tps), 0) AS max_tps").
			Group("COALESCE(NULLIF(d.egress_node_name, ''), '?')").Order("hits DESC, name ASC").Scan(&nodeRows).Error; err != nil {
			return err
		}
		result.Nodes = make([]repository.DegradeNode, 0, len(nodeRows))
		for _, row := range nodeRows {
			result.Nodes = append(result.Nodes, repository.DegradeNode{Name: row.Name, Hits: row.Hits, Accounts: row.Accounts, MaxTPS: row.MaxTPS})
		}

		if len(input.Buckets) > 0 {
			caseSQL, caseArgs := degradeBucketCase(input.Buckets)
			bucketed := tx.Table("(?) AS d", classified).Select(caseSQL+" AS bucket_index, d.degrade_class", caseArgs...)
			var bucketRows []degradeBucketRow
			if err := tx.Table("(?) AS b", bucketed).
				Select("b.bucket_index, COUNT(*) AS count, COALESCE(SUM(CASE WHEN b.degrade_class IN ('hard_tps', 'buffered_burst', 'missing_thinking') THEN 1 ELSE 0 END), 0) AS severe").
				Where("b.bucket_index >= 0").Group("b.bucket_index").Order("b.bucket_index ASC").Scan(&bucketRows).Error; err != nil {
				return err
			}
			result.Buckets = make([]repository.DegradeBucket, 0, len(bucketRows))
			for _, row := range bucketRows {
				result.Buckets = append(result.Buckets, repository.DegradeBucket{Index: row.Index, Count: row.Count, Severe: row.Severe})
			}
		}

		var eventRows []degradeEventRow
		if err := tx.Table("(?) AS d", classified).
			Select("d.id, d.request_id, d.account_id, d.account_name, d.egress_node_id, d.egress_node_name, d.output_tokens, d.tps, d.degrade_class, d.created_at, d.model_upstream_model").
			Order("d.created_at DESC, d.id DESC").Limit(input.RecentLimit).Scan(&eventRows).Error; err != nil {
			return err
		}
		result.Events = make([]repository.DegradeEvent, 0, len(eventRows))
		for _, row := range eventRows {
			result.Events = append(result.Events, repository.DegradeEvent{
				ID: row.ID, RequestID: row.RequestID, AccountID: row.AccountID, AccountName: row.AccountName,
				EgressNodeID: row.EgressNodeID, EgressNodeName: row.EgressNodeName, OutputTokens: row.OutputTokens,
				TPS: row.TPS, Class: row.Class, CreatedAt: row.CreatedAt, Model: row.Model,
			})
		}
		return nil
	})
	return result, err
}

func (r *AuditRepository) degradeClassifiedQuery(tx *gorm.DB, input repository.DegradeSummaryQuery) *gorm.DB {
	castType := "REAL"
	if r.db.dialect == "postgres" {
		castType = "DOUBLE PRECISION"
	}
	generationExpression := fmt.Sprintf("(CASE WHEN a.reasoning_tokens > 0 AND a.duration_ms - a.first_token_ms < a.first_token_ms AND a.duration_ms - a.first_token_ms < %d THEN a.duration_ms ELSE a.duration_ms - a.first_token_ms END)", audit.DefaultDegradeMinGenMS)
	// NULLIF keeps PostgreSQL safe even if its planner evaluates the throughput
	// expression before the duration guard in the WHERE clause.
	tpsExpression := fmt.Sprintf("(CAST(a.output_tokens AS %s) * 1000.0 / NULLIF(%s, 0))", castType, generationExpression)
	selectTPS := fmt.Sprintf("CASE WHEN a.error_code = ? THEN 0 ELSE %s END", tpsExpression)
	classExpression := fmt.Sprintf("CASE WHEN a.error_code = ? THEN ? WHEN ? AND %s < ? THEN ? WHEN %s >= ? THEN ? ELSE ? END", generationExpression, tpsExpression)
	speedPredicate := fmt.Sprintf(
		"a.streaming = ? AND %s AND a.output_tokens >= ? AND a.first_token_ms IS NOT NULL AND a.duration_ms > a.first_token_ms AND %s >= ?",
		auditSuccessPredicate, tpsExpression,
	)
	thinkingPredicate := "a.streaming = ? AND a.error_code = ? AND a.status_code >= 200 AND a.status_code < 300"
	query := tx.Table("request_audits AS a").
		Select("a.id, a.request_id, a.account_id, a.account_name, a.egress_node_id, a.egress_node_name, a.output_tokens, a.created_at, a.model_upstream_model, "+selectTPS+" AS tps, "+classExpression+" AS degrade_class",
			audit.ErrorQualityDegraded,
			audit.ErrorQualityDegraded, audit.DegradeClassThinking, input.FailClosed, input.MinGenerationMS, audit.DegradeClassBurst, input.HardTPS, audit.DegradeClassHard, audit.DegradeClassSoft).
		Where("a.provider = ?", "grok_build").
		Where("a.request_id NOT LIKE ?", "quality_%").
		Where("(("+speedPredicate+") OR ("+thinkingPredicate+"))",
			true, input.MinOutputTokens, input.SoftTPS,
			true, audit.ErrorQualityDegraded)
	if !input.Start.IsZero() {
		query = query.Where("a.created_at >= ?", input.Start)
	}
	if !input.End.IsZero() {
		query = query.Where("a.created_at < ?", input.End)
	}
	return query
}

func (r *AuditRepository) degradeAccountAggregate(tx *gorm.DB, classified *gorm.DB, input repository.DegradeSummaryQuery) *gorm.DB {
	query := tx.Table("(?) AS d", classified).
		Select(`d.account_id AS id,
			COALESCE(NULLIF(p.name, ''), MAX(d.account_name), '') AS name,
			COALESCE(p.email, '') AS email,
			COUNT(*) AS hits,
			COALESCE(MAX(d.tps), 0) AS max_tps,
			COALESCE(SUM(CASE WHEN d.degrade_class = 'buffered_burst' THEN 1 ELSE 0 END), 0) AS burst,
			COALESCE(SUM(CASE WHEN d.degrade_class = 'soft_tps' THEN 1 ELSE 0 END), 0) AS soft,
			COALESCE(SUM(CASE WHEN d.degrade_class = 'hard_tps' THEN 1 ELSE 0 END), 0) AS hard,
			COALESCE(SUM(CASE WHEN d.degrade_class = 'missing_thinking' THEN 1 ELSE 0 END), 0) AS thinking,
			MAX(d.created_at) AS last_seen,
			COALESCE(p.enabled, FALSE) AS enabled,
			CASE WHEN p.id IS NULL THEN FALSE ELSE TRUE END AS found,
			COALESCE(c.build_bot_flag_source, 0) AS build_bot_flag_source`).
		Joins("LEFT JOIN provider_accounts p ON p.id = d.account_id").
		Joins("LEFT JOIN account_credentials c ON c.account_id = d.account_id").
		Where("d.account_id IS NOT NULL").
		Group("d.account_id, p.id, p.name, p.email, p.enabled, c.build_bot_flag_source")
	if search := strings.TrimSpace(input.AccountSearch); search != "" {
		pattern := "%" + strings.ToLower(search) + "%"
		query = query.Where(
			"LOWER(COALESCE(p.email, '')) LIKE ? OR LOWER(COALESCE(p.name, '')) LIKE ? OR LOWER(COALESCE(d.account_name, '')) LIKE ? OR CAST(d.account_id AS TEXT) LIKE ?",
			pattern, pattern, pattern, pattern,
		)
	}
	switch input.AccountStatus {
	case "enabled":
		query = query.Where("p.id IS NOT NULL AND p.enabled = TRUE")
	case "disabled":
		query = query.Where("p.id IS NOT NULL AND p.enabled = FALSE")
	case "deleted":
		query = query.Where("p.id IS NULL")
	}
	if input.MinHits > 1 {
		query = query.Having("COUNT(*) >= ?", input.MinHits)
	}
	if input.AccountClass != "" {
		query = query.Having("SUM(CASE WHEN d.degrade_class = ? THEN 1 ELSE 0 END) > 0", input.AccountClass)
	}
	return query
}

func degradeBucketCase(buckets []repository.DegradeBucketRange) (string, []any) {
	var builder strings.Builder
	args := make([]any, 0, len(buckets)*2)
	builder.WriteString("CASE")
	for index, bucket := range buckets {
		builder.WriteString(" WHEN d.created_at >= ? AND d.created_at < ? THEN ")
		builder.WriteString(strconv.Itoa(index))
		args = append(args, bucket.Start, bucket.End)
	}
	builder.WriteString(" ELSE -1 END")
	return builder.String(), args
}
