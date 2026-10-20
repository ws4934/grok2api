package relational

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	dashboarddomain "github.com/chenyme/grok2api/backend/internal/domain/dashboard"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

type DashboardRepository struct{ db *Database }

func NewDashboardRepository(db *Database) *DashboardRepository { return &DashboardRepository{db: db} }

const dashboardUsageAggregateSelect = `
	COUNT(*) AS requests,
	` + auditSuccessAggregate + ` AS successful_requests,
	COALESCE(SUM(input_tokens), 0) AS input_tokens,
	COALESCE(SUM(cached_input_tokens), 0) AS cached_input_tokens,
	COALESCE(SUM(output_tokens), 0) AS output_tokens,
	COALESCE(SUM(reasoning_tokens), 0) AS reasoning_tokens,
	COALESCE(SUM(total_tokens), 0) AS tokens,
	COALESCE(SUM(` + auditBilledCostExpression + `), 0) AS billed_cost_usd_ticks,
	COUNT(first_token_ms) AS first_token_samples,
	COALESCE(SUM(first_token_ms), 0) AS first_token_total_ms,
	COALESCE(SUM(CASE WHEN first_token_ms IS NOT NULL AND output_tokens > 0 AND duration_ms > first_token_ms THEN 1 ELSE 0 END), 0) AS throughput_samples,
	COALESCE(SUM(CASE WHEN first_token_ms IS NOT NULL AND output_tokens > 0 AND duration_ms > first_token_ms THEN output_tokens ELSE 0 END), 0) AS throughput_tokens,
	COALESCE(SUM(CASE WHEN first_token_ms IS NOT NULL AND output_tokens > 0 AND duration_ms > first_token_ms THEN CASE WHEN reasoning_tokens > 0 AND duration_ms - first_token_ms < first_token_ms AND duration_ms - first_token_ms < 1000 THEN duration_ms ELSE duration_ms - first_token_ms END ELSE 0 END), 0) AS generation_total_ms`

const dashboardTopModelsLimit = 10

// reportTxOptions 让报表在只读事务里读：SQLite 驱动对只读事务发普通 BEGIN，不像 _txlock=immediate
// 那样占住写锁；PostgreSQL 用可重复读，让多条聚合看到同一快照。
func reportTxOptions(dialect string) *sql.TxOptions {
	if dialect == "postgres" {
		return &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead}
	}
	return &sql.TxOptions{ReadOnly: true}
}

// Snapshot 在同一只读事务内读取资源计数和指定区间的审计聚合。
func (r *DashboardRepository) Snapshot(ctx context.Context, window repository.DashboardSnapshotWindow, snapshotAt time.Time) (dashboarddomain.Aggregate, error) {
	if err := validateDashboardBoundaries(window.BucketBoundaries); err != nil {
		return dashboarddomain.Aggregate{}, err
	}
	if err := validateDashboardBoundaries(window.ActivityBoundaries); err != nil {
		return dashboarddomain.Aggregate{}, err
	}
	bucketBoundaries := window.BucketBoundaries
	start := bucketBoundaries[0]
	end := bucketBoundaries[len(bucketBoundaries)-1]
	result := dashboarddomain.Aggregate{}
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var accounts struct {
			Total  int64
			Active int64
		}
		if err := tx.Model(&accountModel{}).
			Select("COUNT(*) AS total, COALESCE(SUM(CASE WHEN enabled = ? AND auth_status = ? AND (cooldown_until IS NULL OR cooldown_until <= ?) AND NOT "+accountRecoveryPredicate+" AND NOT "+providerQuotaExhaustedPredicate+" THEN 1 ELSE 0 END), 0) AS active", true, "active", snapshotAt).
			Scan(&accounts).Error; err != nil {
			return err
		}
		var providerCounts []struct {
			Provider string
			Total    int64
		}
		if err := tx.Model(&accountModel{}).Select("provider, COUNT(*) AS total").Group("provider").Scan(&providerCounts).Error; err != nil {
			return err
		}
		result.Resources.AccountsByProvider = make(map[string]int64, len(providerCounts))
		for _, item := range providerCounts {
			result.Resources.AccountsByProvider[item.Provider] = item.Total
		}

		var models struct {
			Total   int64
			Enabled int64
		}
		if err := tx.Model(&modelRouteModel{}).
			Select("COUNT(*) AS total, COALESCE(SUM(CASE WHEN enabled = ? AND "+availableRoutePredicate+" THEN 1 ELSE 0 END), 0) AS enabled", true, true, "active").
			Scan(&models).Error; err != nil {
			return err
		}

		var clientKeys struct {
			Total  int64
			Active int64
		}
		if err := tx.Model(&clientKeyModel{}).Where("internal_kind IS NULL").
			Select("COUNT(*) AS total, COALESCE(SUM(CASE WHEN enabled = ? AND (expires_at IS NULL OR expires_at > ?) THEN 1 ELSE 0 END), 0) AS active", true, snapshotAt).
			Scan(&clientKeys).Error; err != nil {
			return err
		}

		result.Resources.ActiveAccounts = accounts.Active
		result.Resources.TotalAccounts = accounts.Total
		result.Resources.EnabledModels = models.Enabled
		result.Resources.TotalModels = models.Total
		result.Resources.ActiveClientKeys = clientKeys.Active
		result.Resources.TotalClientKeys = clientKeys.Total

		// 总量和趋势桶共用一次扫描：先按 UTC 时间片分组，再在 Go 里折叠到本地时间桶。
		usageSlot, usageArgs, usageGroup, usageKeys := r.dashboardSlots(bucketBoundaries)
		var usageSlots []struct {
			Slot string
			dashboarddomain.Usage
		}
		if err := tx.Model(&requestAuditModel{}).
			Select(usageSlot+" AS slot, "+dashboardUsageAggregateSelect, usageArgs...).
			Where("created_at >= ? AND created_at < ?", start, end).
			Group(usageGroup).
			Scan(&usageSlots).Error; err != nil {
			return err
		}
		bucketUsage := make([]dashboarddomain.Usage, len(bucketBoundaries)-1)
		for _, slot := range usageSlots {
			addDashboardUsage(&result.Usage, slot.Usage)
			if index := dashboardSlotIndex(usageKeys, slot.Slot); index >= 0 && index < len(bucketUsage) {
				addDashboardUsage(&bucketUsage[index], slot.Usage)
			}
		}
		result.Usage.FailedRequests = result.Usage.Requests - result.Usage.SuccessfulRequests
		result.Buckets = make([]dashboarddomain.Bucket, 0, len(bucketUsage))
		for index, bucket := range bucketUsage {
			if bucket.Requests > 0 {
				result.Buckets = append(result.Buckets, dashboarddomain.Bucket{Index: index, Requests: bucket.Requests, InputTokens: bucket.InputTokens, CachedInputTokens: bucket.CachedInputTokens, OutputTokens: bucket.OutputTokens, ReasoningTokens: bucket.ReasoningTokens, Tokens: bucket.Tokens, BilledCostUSDTicks: bucket.BilledCostUSDTicks})
			}
		}

		activitySlot, activityArgs, activityGroup, activityKeys := r.dashboardSlots(window.ActivityBoundaries)
		var activitySlots []struct {
			Slot     string
			Requests int64
		}
		if err := tx.Model(&requestAuditModel{}).
			Select(activitySlot+" AS slot, COUNT(*) AS requests", activityArgs...).
			Where("created_at >= ? AND created_at < ?", window.ActivityBoundaries[0], window.ActivityBoundaries[len(window.ActivityBoundaries)-1]).
			Group(activityGroup).
			Scan(&activitySlots).Error; err != nil {
			return err
		}
		activityRequests := make([]int64, len(window.ActivityBoundaries)-1)
		for _, slot := range activitySlots {
			if index := dashboardSlotIndex(activityKeys, slot.Slot); index >= 0 && index < len(activityRequests) {
				activityRequests[index] += slot.Requests
			}
		}
		result.ActivityBuckets = make([]dashboarddomain.ActivityBucket, 0, len(activityRequests))
		for index, requests := range activityRequests {
			if requests > 0 {
				result.ActivityBuckets = append(result.ActivityBuckets, dashboarddomain.ActivityBucket{Index: index, Requests: requests})
			}
		}

		var providers []struct {
			Provider           string
			Requests           int64
			SuccessfulRequests int64
			Tokens             int64
		}
		if err := tx.Model(&requestAuditModel{}).
			Select("provider, COUNT(*) AS requests, "+auditSuccessAggregate+" AS successful_requests, COALESCE(SUM(total_tokens), 0) AS tokens").
			Where("created_at >= ? AND created_at < ?", start, end).
			Group("provider").
			Order("requests DESC, provider ASC").
			Scan(&providers).Error; err != nil {
			return err
		}
		result.Providers = make([]dashboarddomain.ProviderUsage, 0, len(providers))
		for _, item := range providers {
			result.Providers = append(result.Providers, dashboarddomain.ProviderUsage{Provider: item.Provider, Requests: item.Requests, SuccessfulRequests: item.SuccessfulRequests, Tokens: item.Tokens})
		}

		// One small query per provider media quota kind; there are only a few.
		// A window past its reset counts as full, unless the upstream reports no
		// total (ChatGPT), where the stored remaining is all we know.
		for _, source := range window.MediaQuota {
			var quota struct {
				Remaining int64
				Total     int64
				Accounts  int64
			}
			if err := tx.Table("account_quota_windows AS w").
				Joins("JOIN provider_accounts AS a ON a.id = w.account_id").
				Select("COALESCE(SUM(CASE WHEN w.reset_at IS NOT NULL AND w.reset_at <= ? AND w.total > 0 THEN w.total ELSE w.remaining END), 0) AS remaining, "+
					"COALESCE(SUM(w.total), 0) AS total, COUNT(DISTINCT w.account_id) AS accounts", snapshotAt).
				Where("a.provider = ? AND a.enabled = ? AND a.auth_status = ? AND w.mode IN ?", source.Provider, true, "active", source.Modes).
				Scan(&quota).Error; err != nil {
				return err
			}
			if quota.Accounts > 0 {
				result.MediaQuota = append(result.MediaQuota, dashboarddomain.MediaQuota{
					Provider: source.Provider, Kind: source.Kind, Remaining: quota.Remaining, Total: quota.Total, Accounts: quota.Accounts,
				})
			}
		}

		modelExpression := "CASE WHEN TRIM(model_public_id) <> '' THEN model_public_id WHEN TRIM(model_upstream_model) <> '' THEN model_upstream_model ELSE 'unknown' END"
		var topModels []struct {
			Model              string
			Requests           int64
			InputTokens        int64
			CachedInputTokens  int64
			OutputTokens       int64
			ReasoningTokens    int64
			Tokens             int64
			BilledCostUSDTicks int64
		}
		if err := tx.Model(&requestAuditModel{}).
			Select(modelExpression+" AS model, COUNT(*) AS requests, COALESCE(SUM(input_tokens), 0) AS input_tokens, COALESCE(SUM(cached_input_tokens), 0) AS cached_input_tokens, COALESCE(SUM(output_tokens), 0) AS output_tokens, COALESCE(SUM(reasoning_tokens), 0) AS reasoning_tokens, COALESCE(SUM(total_tokens), 0) AS tokens, COALESCE(SUM("+auditBilledCostExpression+"), 0) AS billed_cost_usd_ticks").
			Where("created_at >= ? AND created_at < ?", start, end).
			Group(modelExpression).
			Order("billed_cost_usd_ticks DESC, requests DESC, tokens DESC, model ASC").
			Limit(dashboardTopModelsLimit).
			Scan(&topModels).Error; err != nil {
			return err
		}
		result.TopModels = make([]dashboarddomain.ModelUsage, 0, dashboardTopModelsLimit)
		listedModels := make(map[string]struct{}, dashboardTopModelsLimit)
		for _, item := range topModels {
			result.TopModels = append(result.TopModels, dashboarddomain.ModelUsage{Model: item.Model, Requests: item.Requests, InputTokens: item.InputTokens, CachedInputTokens: item.CachedInputTokens, OutputTokens: item.OutputTokens, ReasoningTokens: item.ReasoningTokens, Tokens: item.Tokens, BilledCostUSDTicks: item.BilledCostUSDTicks})
			listedModels[item.Model] = struct{}{}
		}
		if len(result.TopModels) < dashboardTopModelsLimit {
			var enabledModels []struct {
				PublicID string
				Provider string
			}
			if err := tx.Model(&modelRouteModel{}).
				Select("public_id, provider").
				Where("enabled = ?", true).
				Order("public_id ASC").
				Limit(dashboardTopModelsLimit * len(accountdomain.Providers())).
				Scan(&enabledModels).Error; err != nil {
				return err
			}
			for _, route := range enabledModels {
				publicID := modeldomain.ExternalPublicID(accountdomain.Provider(route.Provider), route.PublicID)
				if publicID == "" {
					continue
				}
				if _, exists := listedModels[publicID]; exists {
					continue
				}
				result.TopModels = append(result.TopModels, dashboarddomain.ModelUsage{Model: publicID})
				listedModels[publicID] = struct{}{}
				if len(result.TopModels) == dashboardTopModelsLimit {
					break
				}
			}
		}
		return nil
	}, reportTxOptions(r.db.dialect))
	return result, err
}

func validateDashboardBoundaries(boundaries []time.Time) error {
	if len(boundaries) < 2 {
		return fmt.Errorf("Dashboard 聚合范围无效")
	}
	for index := 1; index < len(boundaries); index++ {
		if !boundaries[index-1].Before(boundaries[index]) {
			return fmt.Errorf("Dashboard 时间桶无效")
		}
	}
	return nil
}

// dashboardSlots 返回取时间片键的表达式及其参数、分组表达式和各边界对应的键。边界都是 UTC 整点时按小时、
// 都在 UTC 刻钟上（+05:30、+05:45 这类时区）时按刻钟分组，Go 侧再把时间片折叠进桶，不必逐行求值
// N 分支的 CASE。SQLite 的 created_at 是 "2006-01-02 15:04:05..." 形式的 UTC 文本，取前缀分组与逐行
// 比较文本的结果一致；键按字典序就是时间顺序。其他边界退回逐行 CASE，直接取所在桶的键。
func (r *DashboardRepository) dashboardSlots(boundaries []time.Time) (string, []any, string, []string) {
	hourly, quarterly := true, true
	for _, boundary := range boundaries {
		quarterly = quarterly && boundary.Location() == time.UTC && boundary.Truncate(15*time.Minute).Equal(boundary)
		hourly = hourly && quarterly && boundary.Truncate(time.Hour).Equal(boundary)
	}
	group, format, layout := "slot", "", "2006-01-02 15:04"
	switch {
	case !quarterly:
		layout = "2006-01-02 15:04:05.000000000"
	case r.db.dialect == "postgres" && hourly:
		group, format, layout = "date_trunc('hour', created_at AT TIME ZONE 'UTC')", "YYYY-MM-DD HH24", "2006-01-02 15"
	case r.db.dialect == "postgres":
		group, format = "date_trunc('hour', created_at AT TIME ZONE 'UTC') + (date_part('minute', created_at AT TIME ZONE 'UTC')::int / 15) * INTERVAL '15 minutes'", "YYYY-MM-DD HH24:MI"
	case hourly:
		group, layout = "substr(created_at, 1, 13)", "2006-01-02 15"
	default:
		group = "substr(created_at, 1, 14) || CASE WHEN substr(created_at, 15, 2) < '15' THEN '00' WHEN substr(created_at, 15, 2) < '30' THEN '15' WHEN substr(created_at, 15, 2) < '45' THEN '30' ELSE '45' END"
	}
	keys := make([]string, len(boundaries))
	for index, boundary := range boundaries {
		keys[index] = boundary.UTC().Format(layout)
	}
	if !quarterly {
		var expression strings.Builder
		var args []any
		expression.WriteString("CASE")
		for index := 0; index < len(boundaries)-1; index++ {
			expression.WriteString(" WHEN created_at >= ? AND created_at < ? THEN ?")
			args = append(args, boundaries[index], boundaries[index+1], keys[index])
		}
		expression.WriteString(" ELSE '' END")
		return expression.String(), args, group, keys
	}
	if format != "" {
		// PostgreSQL 按时间值分组，每组只格式化一次键。
		return "to_char(" + group + ", '" + format + "')", nil, group, keys
	}
	return group, nil, group, keys
}

// dashboardSlotIndex 返回时间片所在桶的下标；keys 是 dashboardSlots 返回的边界键。
func dashboardSlotIndex(keys []string, slot string) int {
	return sort.Search(len(keys), func(index int) bool { return keys[index] > slot }) - 1
}

func addDashboardUsage(target *dashboarddomain.Usage, value dashboarddomain.Usage) {
	target.Requests += value.Requests
	target.SuccessfulRequests += value.SuccessfulRequests
	target.InputTokens += value.InputTokens
	target.CachedInputTokens += value.CachedInputTokens
	target.OutputTokens += value.OutputTokens
	target.ReasoningTokens += value.ReasoningTokens
	target.Tokens += value.Tokens
	target.BilledCostUSDTicks += value.BilledCostUSDTicks
	target.FirstTokenSamples += value.FirstTokenSamples
	target.FirstTokenTotalMS += value.FirstTokenTotalMS
	target.ThroughputSamples += value.ThroughputSamples
	target.ThroughputTokens += value.ThroughputTokens
	target.GenerationTotalMS += value.GenerationTotalMS
}
