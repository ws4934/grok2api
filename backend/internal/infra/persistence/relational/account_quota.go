package relational

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type quotaBreakdownJSON struct {
	ProductCode  int     `json:"productCode"`
	UsagePercent float64 `json:"usagePercent"`
}

func (r *AccountRepository) UpsertModelQuotaBlock(ctx context.Context, value account.ModelQuotaBlock) error {
	value.UpstreamModel = strings.TrimSpace(value.UpstreamModel)
	value.Reason = strings.TrimSpace(value.Reason)
	if value.AccountID == 0 || value.UpstreamModel == "" || value.Reason == "" || value.CooldownUntil.IsZero() {
		return repository.ErrConflict
	}
	now := time.Now().UTC()
	row := accountModelQuotaBlockModel{
		AccountID: value.AccountID, UpstreamModel: truncate(value.UpstreamModel, 255), Reason: truncate(value.Reason, 100),
		CooldownUntil: value.CooldownUntil.UTC(), UpdatedAt: now,
	}
	err := r.db.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "account_id"}, {Name: "upstream_model"}},
		DoUpdates: clause.Assignments(map[string]any{
			"reason":         gorm.Expr("CASE WHEN cooldown_until > ? THEN reason ELSE ? END", row.CooldownUntil, row.Reason),
			"cooldown_until": gorm.Expr("CASE WHEN cooldown_until > ? THEN cooldown_until ELSE ? END", row.CooldownUntil, row.CooldownUntil), "updated_at": now,
		}),
	}).Create(&row).Error
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountModelQuotaChanged, AccountID: value.AccountID, UpstreamModel: value.UpstreamModel})
	}
	return err
}

func (r *AccountRepository) PruneExpiredModelQuotaBlocks(ctx context.Context, now time.Time, limit int) (int64, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var rows []accountModelQuotaBlockModel
	if err := r.db.db.WithContext(ctx).Select("account_id", "upstream_model").Where("cooldown_until <= ?", now.UTC()).Order("cooldown_until ASC").Limit(limit).Find(&rows).Error; err != nil || len(rows) == 0 {
		return 0, err
	}
	var deleted int64
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, row := range rows {
			result := tx.Where("account_id = ? AND upstream_model = ? AND cooldown_until <= ?", row.AccountID, row.UpstreamModel, now.UTC()).Delete(&accountModelQuotaBlockModel{})
			if result.Error != nil {
				return result.Error
			}
			deleted += result.RowsAffected
		}
		return nil
	})
	if err == nil && deleted > 0 {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountModelQuotaChanged})
	}
	return deleted, err
}

func (r *AccountRepository) SaveBilling(ctx context.Context, value account.Billing) error {
	history, err := json.Marshal(value.History)
	if err != nil {
		return err
	}
	row := billingModel{AccountID: value.AccountID, PlanCode: truncate(value.PlanCode, 100), PlanName: truncate(value.PlanName, 160), MonthlyLimit: value.MonthlyLimit, Used: value.Used, OnDemandCap: value.OnDemandCap, OnDemandUsed: value.OnDemandUsed, PrepaidBalance: value.PrepaidBalance, CreditUsagePercent: value.CreditUsagePercent, IsUnifiedBillingUser: value.IsUnifiedBillingUser, OnDemandEnabled: value.OnDemandEnabled, TopUpMethod: truncate(value.TopUpMethod, 100), UsagePeriodType: truncate(value.UsagePeriodType, 100), UsagePeriodStart: truncate(value.UsagePeriodStart, 64), UsagePeriodEnd: truncate(value.UsagePeriodEnd, 64), BillingPeriodStart: truncate(value.BillingPeriodStart, 64), BillingPeriodEnd: truncate(value.BillingPeriodEnd, 64), HistoryJSON: string(history), SyncedAt: value.SyncedAt}
	err = r.db.db.WithContext(ctx).Save(&row).Error
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountBillingChanged, AccountID: value.AccountID})
	}
	return err
}

func (r *AccountRepository) GetBilling(ctx context.Context, accountID uint64) (account.Billing, error) {
	var row billingModel
	if err := r.db.db.WithContext(ctx).First(&row, "account_id = ?", accountID).Error; err != nil {
		return account.Billing{}, mapError(err)
	}
	return toBillingDomain(row), nil
}

func (r *AccountRepository) GetBillings(ctx context.Context, accountIDs []uint64) (map[uint64]account.Billing, error) {
	result := make(map[uint64]account.Billing, len(accountIDs))
	if len(accountIDs) == 0 {
		return result, nil
	}
	var rows []billingModel
	if err := r.db.db.WithContext(ctx).Where("account_id IN ?", accountIDs).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.AccountID] = toBillingDomain(row)
	}
	return result, nil
}

func (r *AccountRepository) GetQuotaRecovery(ctx context.Context, accountID uint64) (account.QuotaRecovery, error) {
	var row quotaRecoveryModel
	if err := r.db.db.WithContext(ctx).First(&row, "account_id = ?", accountID).Error; err != nil {
		return account.QuotaRecovery{}, mapError(err)
	}
	return account.QuotaRecovery{
		AccountID: row.AccountID, Kind: account.QuotaRecoveryKind(row.Kind), Status: account.QuotaRecoveryStatus(row.Status), ConfirmedUsed: row.ConfirmedUsed,
		ConfirmedLimit: row.ConfirmedLimit, ExhaustedAt: row.ExhaustedAt, NextProbeAt: row.NextProbeAt,
		LastConfirmedAt: row.LastConfirmedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

func (r *AccountRepository) GetQuotaRecoveries(ctx context.Context, accountIDs []uint64) (map[uint64]account.QuotaRecovery, error) {
	result := make(map[uint64]account.QuotaRecovery, len(accountIDs))
	if len(accountIDs) == 0 {
		return result, nil
	}
	var rows []quotaRecoveryModel
	if err := r.db.db.WithContext(ctx).Where("account_id IN ?", accountIDs).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.AccountID] = account.QuotaRecovery{
			AccountID: row.AccountID, Kind: account.QuotaRecoveryKind(row.Kind), Status: account.QuotaRecoveryStatus(row.Status), ConfirmedUsed: row.ConfirmedUsed,
			ConfirmedLimit: row.ConfirmedLimit, ExhaustedAt: row.ExhaustedAt, NextProbeAt: row.NextProbeAt,
			LastConfirmedAt: row.LastConfirmedAt, UpdatedAt: row.UpdatedAt,
		}
	}
	return result, nil
}

func (r *AccountRepository) SaveQuotaRecovery(ctx context.Context, value account.QuotaRecovery) error {
	row := quotaRecoveryModel{
		AccountID: value.AccountID, Kind: string(value.Kind), Status: string(value.Status), ConfirmedUsed: value.ConfirmedUsed,
		ConfirmedLimit: value.ConfirmedLimit, ExhaustedAt: value.ExhaustedAt, NextProbeAt: value.NextProbeAt,
		LastConfirmedAt: value.LastConfirmedAt, UpdatedAt: value.UpdatedAt,
	}
	err := r.db.db.WithContext(ctx).Save(&row).Error
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountRecoveryChanged, AccountID: value.AccountID})
	}
	return err
}

func (r *AccountRepository) ClaimQuotaProbe(ctx context.Context, accountID uint64, now, leaseUntil time.Time) (bool, error) {
	result := r.db.db.WithContext(ctx).Model(&quotaRecoveryModel{}).
		Where("account_id = ? AND status IN ? AND next_probe_at IS NOT NULL AND next_probe_at <= ?", accountID, []string{string(account.QuotaRecoveryStatusExhausted), string(account.QuotaRecoveryStatusProbing)}, now).
		Updates(map[string]any{"status": string(account.QuotaRecoveryStatusProbing), "next_probe_at": leaseUntil, "updated_at": now})
	return result.RowsAffected == 1, result.Error
}

func (r *AccountRepository) ClearQuotaRecovery(ctx context.Context, accountID uint64) error {
	result := r.db.db.WithContext(ctx).Delete(&quotaRecoveryModel{}, "account_id = ?", accountID)
	if result.Error == nil && result.RowsAffected > 0 {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountRecoveryChanged, AccountID: accountID})
	}
	return result.Error
}

func (r *AccountRepository) ResetQuotaState(ctx context.Context, provider account.Provider, accountIDs []uint64) error {
	if len(accountIDs) == 0 {
		return nil
	}
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("account_id IN ?", accountIDs).Delete(&quotaRecoveryModel{}).Error; err != nil {
			return err
		}
		return tx.Where("account_id IN ? AND reason = ?", accountIDs, "model_quota_depleted").Delete(&accountModelQuotaBlockModel{}).Error
	})
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountRecoveryChanged, Provider: provider})
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountModelQuotaChanged, Provider: provider})
	}
	return err
}

func (r *AccountRepository) ResetProviderQuotaState(ctx context.Context, provider account.Provider, activeOnly bool) (int64, error) {
	var accountCount int64
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		accountQuery := func() *gorm.DB {
			query := tx.Model(&accountModel{}).Where("provider = ?", provider)
			if activeOnly {
				query = query.Where("enabled = ? AND auth_status = ?", true, account.AuthStatusActive)
			}
			return query
		}
		if err := accountQuery().Count(&accountCount).Error; err != nil {
			return err
		}
		if err := tx.Where("account_id IN (?)", accountQuery().Select("id")).Delete(&quotaRecoveryModel{}).Error; err != nil {
			return err
		}
		return tx.Where("account_id IN (?) AND reason = ?", accountQuery().Select("id"), "model_quota_depleted").Delete(&accountModelQuotaBlockModel{}).Error
	})
	if err == nil && accountCount > 0 {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountRecoveryChanged, Provider: provider})
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountModelQuotaChanged, Provider: provider})
	}
	return accountCount, err
}

func (r *AccountRepository) HasQuotaWindows(ctx context.Context, accountID uint64) (bool, error) {
	var providerRow struct {
		Provider string
	}
	if err := r.db.db.WithContext(ctx).Model(&accountModel{}).Select("provider").Where("id = ?", accountID).Take(&providerRow).Error; err != nil {
		return false, err
	}
	var count int64
	query := r.db.db.WithContext(ctx).Model(&quotaWindowModel{}).Where("account_id = ? AND synced_at IS NOT NULL", accountID)
	if account.Provider(providerRow.Provider) == account.ProviderConsole {
		// Pre-usage Console releases stored one synthetic local chat window.
		// Only the complete authoritative /usage snapshot counts as initialized,
		// so re-import and startup migration replace that legacy state.
		query = query.Where("source = ? AND mode IN ?", account.QuotaSourceUpstream, []string{"console", "console_image", "console_video"}).Distinct("mode")
		if err := query.Count(&count).Error; err != nil {
			return false, err
		}
		return count == 3, nil
	}
	if err := query.Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

func (r *AccountRepository) GetQuotaWindows(ctx context.Context, accountIDs []uint64) (map[uint64][]account.QuotaWindow, error) {
	result := make(map[uint64][]account.QuotaWindow, len(accountIDs))
	if len(accountIDs) == 0 {
		return result, nil
	}
	var rows []quotaWindowModel
	if err := r.db.db.WithContext(ctx).Where("account_id IN ?", accountIDs).Order("account_id ASC, mode ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.AccountID] = append(result[row.AccountID], toQuotaWindowDomain(row))
	}
	return result, nil
}

func (r *AccountRepository) SaveQuotaWindows(ctx context.Context, accountID uint64, tier account.WebTier, syncedAt time.Time, values []account.QuotaWindow) error {
	err := r.saveQuotaWindows(ctx, accountID, tier, syncedAt, values, false, nil)
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountQuotaChanged, AccountID: accountID})
	}
	return err
}

func (r *AccountRepository) ReplaceQuotaWindows(ctx context.Context, accountID uint64, tier account.WebTier, syncedAt time.Time, values []account.QuotaWindow) error {
	err := r.saveQuotaWindows(ctx, accountID, tier, syncedAt, values, true, nil)
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountQuotaChanged, AccountID: accountID})
	}
	return err
}

func (r *AccountRepository) ReplaceQuotaWindowGroup(ctx context.Context, accountID uint64, syncedAt time.Time, modes []string, values []account.QuotaWindow) error {
	allowed := make(map[string]struct{}, len(modes))
	cleanModes := make([]string, 0, len(modes))
	for _, mode := range modes {
		mode = strings.TrimSpace(mode)
		if mode == "" {
			return repository.ErrConflict
		}
		if _, exists := allowed[mode]; !exists {
			allowed[mode] = struct{}{}
			cleanModes = append(cleanModes, mode)
		}
	}
	if accountID == 0 || len(cleanModes) == 0 {
		return repository.ErrConflict
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		mode := strings.TrimSpace(value.Mode)
		if _, ok := allowed[mode]; !ok {
			return repository.ErrConflict
		}
		if _, duplicate := seen[mode]; duplicate {
			return repository.ErrConflict
		}
		seen[mode] = struct{}{}
	}
	err := r.saveQuotaWindows(ctx, accountID, "", syncedAt, values, false, cleanModes)
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountQuotaChanged, AccountID: accountID})
	}
	return err
}

func (r *AccountRepository) saveQuotaWindows(ctx context.Context, accountID uint64, tier account.WebTier, syncedAt time.Time, values []account.QuotaWindow, replace bool, replaceModes []string) error {
	return r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if tier != "" {
			profile := webAccountProfileModel{AccountID: accountID, Tier: string(tier), SyncedAt: &syncedAt}
			if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "account_id"}}, DoUpdates: clause.AssignmentColumns([]string{"tier", "synced_at"})}).Create(&profile).Error; err != nil {
				return err
			}
		}
		// The marker also records a sync that returned no windows (no limits).
		if err := tx.Exec("UPDATE provider_accounts SET quota_synced_at = ? WHERE id = ?", syncedAt.UTC(), accountID).Error; err != nil {
			return err
		}
		if replace {
			if err := tx.Where("account_id = ?", accountID).Delete(&quotaWindowModel{}).Error; err != nil {
				return err
			}
		} else if len(replaceModes) > 0 {
			if err := tx.Where("account_id = ? AND mode IN ?", accountID, replaceModes).Delete(&quotaWindowModel{}).Error; err != nil {
				return err
			}
		}
		for _, value := range values {
			serializedBreakdown := make([]quotaBreakdownJSON, 0, len(value.Breakdown))
			for _, item := range value.Breakdown {
				serializedBreakdown = append(serializedBreakdown, quotaBreakdownJSON{ProductCode: item.ProductCode, UsagePercent: item.UsagePercent})
			}
			breakdownJSON, err := json.Marshal(serializedBreakdown)
			if err != nil {
				return err
			}
			row := quotaWindowModel{
				AccountID: accountID, Mode: truncate(strings.TrimSpace(value.Mode), 64), Remaining: max(0, value.Remaining), Total: max(0, value.Total),
				UsagePercent: min(100, max(0, value.UsagePercent)), BreakdownJSON: string(breakdownJSON),
				WindowSeconds: max(0, value.WindowSeconds), ResetAt: value.ResetAt, SyncedAt: value.SyncedAt, Source: string(value.Source), UpdatedAt: syncedAt,
			}
			if row.Source == "" {
				row.Source = string(account.QuotaSourceUpstream)
			}
			if err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "account_id"}, {Name: "mode"}},
				DoUpdates: clause.AssignmentColumns([]string{"remaining", "total", "usage_percent", "breakdown_json", "window_seconds", "reset_at", "synced_at", "source", "updated_at"}),
			}).Create(&row).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (r *AccountRepository) DecrementQuotaWindow(ctx context.Context, accountID uint64, mode string, now time.Time) (bool, error) {
	result := r.db.db.WithContext(ctx).Model(&quotaWindowModel{}).
		Where("account_id = ? AND mode = ? AND remaining > 0", accountID, mode).
		Updates(map[string]any{"remaining": gorm.Expr("remaining - 1"), "updated_at": now})
	return result.RowsAffected == 1, result.Error
}

func (r *AccountRepository) DecrementQuotaWindowBy(ctx context.Context, accountID uint64, mode string, amount int, now time.Time) (bool, error) {
	if amount <= 0 {
		amount = 1
	}
	result := r.db.db.WithContext(ctx).Model(&quotaWindowModel{}).
		Where("account_id = ? AND mode = ? AND remaining > 0", accountID, mode).
		Updates(map[string]any{
			"remaining":  gorm.Expr("CASE WHEN remaining <= ? THEN 0 ELSE remaining - ? END", amount, amount),
			"updated_at": now,
		})
	return result.RowsAffected == 1, result.Error
}

func (r *AccountRepository) ExhaustQuotaWindow(ctx context.Context, accountID uint64, mode string, resetAt *time.Time, now time.Time) error {
	err := r.db.db.WithContext(ctx).Model(&quotaWindowModel{}).Where("account_id = ? AND mode = ?", accountID, mode).
		Updates(map[string]any{"remaining": 0, "reset_at": resetAt, "updated_at": now}).Error
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountQuotaChanged, AccountID: accountID})
	}
	return err
}

func (r *AccountRepository) ListDueQuotaWindows(ctx context.Context, now time.Time, limit int) ([]account.QuotaWindow, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var rows []quotaWindowModel
	if err := r.db.db.WithContext(ctx).Where("remaining = 0 AND reset_at IS NOT NULL AND reset_at <= ?", now).Order("reset_at ASC, account_id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	values := make([]account.QuotaWindow, 0, len(rows))
	for _, row := range rows {
		values = append(values, toQuotaWindowDomain(row))
	}
	return values, nil
}

func (r *AccountRepository) ListQuotaRecoveryWindows(ctx context.Context, limit int) ([]account.QuotaWindow, error) {
	if limit <= 0 || limit > 100000 {
		limit = 100000
	}
	var rows []quotaWindowModel
	if err := r.db.db.WithContext(ctx).Where("remaining = 0 AND reset_at IS NOT NULL").Order("reset_at ASC, account_id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	values := make([]account.QuotaWindow, 0, len(rows))
	for _, row := range rows {
		values = append(values, toQuotaWindowDomain(row))
	}
	return values, nil
}

// ListStaleQuotaAccountIDs 返回指定 Provider 中缺失或长期未同步额度的账号，供低优先级追赶任务使用。
// An account is stale when its last quota sync is missing or older than before,
// or when any window was flagged for resync (synced_at NULL). Never-synced and
// flagged accounts come first on every dialect.
func (r *AccountRepository) ListStaleQuotaAccountIDs(ctx context.Context, providers []account.Provider, before time.Time, limit int) ([]uint64, error) {
	if len(providers) == 0 {
		return []uint64{}, nil
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var ids []uint64
	err := r.db.db.WithContext(ctx).
		Table("provider_accounts AS account").
		Where("account.provider IN ? AND account.enabled = ? AND account.auth_status = ?", providers, true, account.AuthStatusActive).
		Where("account.quota_synced_at IS NULL OR account.quota_synced_at < ? OR EXISTS (SELECT 1 FROM account_quota_windows AS quota WHERE quota.account_id = account.id AND quota.synced_at IS NULL)", before.UTC()).
		Order("CASE WHEN account.quota_synced_at IS NULL THEN 0 ELSE 1 END, account.quota_synced_at ASC, account.id ASC").
		Limit(limit).
		Pluck("account.id", &ids).Error
	return ids, err
}

func toQuotaWindowDomain(row quotaWindowModel) account.QuotaWindow {
	var serializedBreakdown []quotaBreakdownJSON
	_ = json.Unmarshal([]byte(row.BreakdownJSON), &serializedBreakdown)
	result := toRoutingQuotaWindowDomain(row)
	breakdown := make([]account.QuotaBreakdown, 0, len(serializedBreakdown))
	for _, item := range serializedBreakdown {
		breakdown = append(breakdown, account.QuotaBreakdown{ProductCode: item.ProductCode, UsagePercent: item.UsagePercent})
	}
	result.Breakdown = breakdown
	return result
}
