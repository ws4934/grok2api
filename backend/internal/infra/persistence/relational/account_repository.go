package relational

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

type AccountRepository struct {
	db       *Database
	observer repository.InvalidationObserver
}

func NewAccountRepository(db *Database) *AccountRepository { return &AccountRepository{db: db} }

func (r *AccountRepository) SetInvalidationObserver(observer repository.InvalidationObserver) {
	r.observer = observer
}

func (r *AccountRepository) notifyInvalidation(ctx context.Context, event repository.InvalidationEvent) {
	if r.observer != nil {
		r.observer(ctx, event)
	}
}

const (
	accountUpdateBatchSize      = 500
	accountNormalizedPlanCode   = `LOWER(REPLACE(REPLACE(REPLACE(REPLACE(TRIM(billing.plan_code), ' ', ''), '_', ''), '-', ''), '+', 'plus'))`
	accountNormalizedPlanName   = `LOWER(REPLACE(REPLACE(REPLACE(REPLACE(TRIM(billing.plan_name), ' ', ''), '_', ''), '-', ''), '+', 'plus'))`
	accountPaidPlanNames        = `'super', 'supergrok', 'supergrokpro', 'supergrokheavy', 'supergroklite', 'supergrokplus', 'grokpro', 'xpremium', 'xpremiumplus', 'apikey'`
	accountPaidPlanSignal       = `(` + accountNormalizedPlanCode + ` IN (` + accountPaidPlanNames + `) OR ` + accountNormalizedPlanName + ` IN (` + accountPaidPlanNames + `) OR substr(` + accountNormalizedPlanCode + `, 1, 9) = 'supergrok' OR substr(` + accountNormalizedPlanName + `, 1, 9) = 'supergrok')`
	accountFreePlanSignal       = `(LOWER(REPLACE(REPLACE(REPLACE(REPLACE(TRIM(billing.plan_code), ' ', ''), '_', ''), '-', ''), '+', 'plus')) IN ('free', 'grokfree', 'freetier', 'basic', 'grokbasic', 'xbasic') OR LOWER(REPLACE(REPLACE(REPLACE(REPLACE(TRIM(billing.plan_name), ' ', ''), '_', ''), '-', ''), '+', 'plus')) IN ('free', 'grokfree', 'freetier', 'basic', 'grokbasic', 'xbasic'))`
	accountPaidBillingSignals   = `(` + accountPaidPlanSignal + ` OR billing.monthly_limit > 0 OR billing.on_demand_cap > 0 OR billing.on_demand_used > 0 OR billing.prepaid_balance > 0)`
	accountPaidBillingPredicate = `EXISTS (SELECT 1 FROM account_billing_snapshots billing WHERE billing.account_id = provider_accounts.id AND ` + accountPaidBillingSignals + `)`
	// 仅 grok_build 的管理员确认 Super entitlement；与 domain.IsBuildSuper 对齐。
	accountBuildSuperEntitledPredicate        = `(provider_accounts.provider = 'grok_build' AND provider_accounts.build_super_entitled = TRUE)`
	accountBuildSuperPredicate                = `(` + accountPaidBillingPredicate + ` OR ` + accountBuildSuperEntitledPredicate + `)`
	accountInferredFreeBillingSignal          = `(TRIM(billing.plan_code) = '' AND TRIM(billing.plan_name) = '' AND billing.synced_at IS NOT NULL AND billing.monthly_limit = 0 AND billing.used = 0 AND billing.on_demand_cap = 0 AND billing.on_demand_used = 0 AND billing.prepaid_balance = 0 AND billing.credit_usage_percent = 0)`
	accountFreeBillingSignal                  = `(` + accountFreePlanSignal + ` OR ` + accountInferredFreeBillingSignal + `)`
	accountFreeSignalPredicate                = `(provider_accounts.provider = 'grok_build' AND (LOWER(TRIM(provider_accounts.observed_model)) LIKE '%-build-free' OR EXISTS (SELECT 1 FROM account_billing_snapshots billing WHERE billing.account_id = provider_accounts.id AND ` + accountFreeBillingSignal + `)))`
	accountRecoveryPredicate                  = `EXISTS (SELECT 1 FROM account_quota_recovery recovery WHERE recovery.account_id = provider_accounts.id AND recovery.status IN ('exhausted', 'probing'))`
	providerQuotaExhaustedPredicate           = `((provider_accounts.provider = 'grok_web' AND ((EXISTS (SELECT 1 FROM account_quota_windows quota WHERE quota.account_id = provider_accounts.id AND quota.mode = 'weekly') AND NOT EXISTS (SELECT 1 FROM account_quota_windows quota WHERE quota.account_id = provider_accounts.id AND quota.mode = 'weekly' AND quota.remaining > 0)) OR (NOT EXISTS (SELECT 1 FROM account_quota_windows quota WHERE quota.account_id = provider_accounts.id AND quota.mode = 'weekly') AND EXISTS (SELECT 1 FROM account_quota_windows quota WHERE quota.account_id = provider_accounts.id) AND NOT EXISTS (SELECT 1 FROM account_quota_windows quota WHERE quota.account_id = provider_accounts.id AND quota.remaining > 0)))) OR (provider_accounts.provider = 'grok_console' AND EXISTS (SELECT 1 FROM account_quota_windows quota WHERE quota.account_id = provider_accounts.id AND quota.mode = 'console') AND NOT EXISTS (SELECT 1 FROM account_quota_windows quota WHERE quota.account_id = provider_accounts.id AND quota.mode = 'console' AND quota.remaining > 0)))`
	accountTypeSortExpression                 = `CASE WHEN provider_accounts.provider = 'grok_web' THEN COALESCE((SELECT profile.tier FROM web_account_profiles profile WHERE profile.account_id = provider_accounts.id), 'auto') WHEN ` + accountBuildSuperPredicate + ` THEN 'paid' WHEN ` + accountFreeSignalPredicate + ` THEN 'free' ELSE 'unknown' END`
	accountStatusSortExpression               = `CASE WHEN provider_accounts.enabled = FALSE THEN 4 WHEN provider_accounts.auth_status = 'reauthRequired' THEN 5 WHEN EXISTS (SELECT 1 FROM account_quota_recovery recovery WHERE recovery.account_id = provider_accounts.id AND recovery.status = 'probing') THEN 3 WHEN EXISTS (SELECT 1 FROM account_quota_recovery recovery WHERE recovery.account_id = provider_accounts.id AND recovery.status = 'exhausted') OR ` + providerQuotaExhaustedPredicate + ` THEN 2 WHEN provider_accounts.cooldown_until > CURRENT_TIMESTAMP THEN 1 ELSE 0 END`
	accountUnclassifiedRefreshReauthPredicate = `NOT EXISTS (SELECT 1 FROM account_credentials credential WHERE credential.account_id = provider_accounts.id AND credential.refresh_permanent = FALSE AND credential.refresh_unclassified_auth_failures > 0)`
	missingConsoleAccountPredicate            = `NOT EXISTS (SELECT 1 FROM provider_accounts AS console_account WHERE console_account.provider = ? AND console_account.source_key = ('console-' || provider_accounts.source_key))`
)

func (r *AccountRepository) List(ctx context.Context, input repository.AccountListQuery) ([]account.Credential, int64, error) {
	var total int64
	query := r.db.db.WithContext(ctx).Model(&accountModel{})
	if input.Filter.Provider != "" {
		query = query.Where("provider = ?", input.Filter.Provider)
	}
	if search := strings.TrimSpace(input.Page.Search); search != "" {
		if id, err := strconv.ParseUint(strings.TrimPrefix(search, "#"), 10, 64); strings.HasPrefix(search, "#") && err == nil && id > 0 {
			// #ID 是管理端名单使用的内部精确查询形式，走主键索引且不改变
			// 原有纯数字名称的模糊搜索语义。
			query = query.Where("provider_accounts.id = ?", id)
		} else {
			pattern := "%" + strings.ToLower(search) + "%"
			query = query.Where("LOWER(name) LIKE ? OR LOWER(email) LIKE ? OR LOWER(user_id) LIKE ? OR LOWER(team_id) LIKE ?", pattern, pattern, pattern, pattern)
		}
	}
	switch input.Filter.QuotaType {
	case "free":
		// Super（Billing paid 或 BuildSuperEntitled）不得落入 free；与 IsKnownFreeBuild / QuotaView 一致。
		query = query.Where("NOT " + accountBuildSuperPredicate + " AND (EXISTS (SELECT 1 FROM account_quota_recovery recovery WHERE recovery.account_id = provider_accounts.id AND recovery.kind = 'free') OR " + accountFreeSignalPredicate + ")")
	case "paid":
		query = query.Where(accountBuildSuperPredicate)
	case "unknown":
		query = query.Where("NOT " + accountRecoveryPredicate + " AND NOT " + accountBuildSuperPredicate + " AND NOT " + accountFreeSignalPredicate)
	case "auto", "basic", "super", "heavy":
		query = query.Where("EXISTS (SELECT 1 FROM web_account_profiles profile WHERE profile.account_id = provider_accounts.id AND profile.tier = ?)", input.Filter.QuotaType)
	}
	query = applyAccountStatusFilter(query, input.Filter.Status, input.Filter.Now)
	switch input.Filter.Egress {
	case "bound":
		query = query.Where("egress_node_id IS NOT NULL")
		if nodeID := input.Filter.EgressNodeID; nodeID > 0 {
			query = query.Where("egress_node_id = ?", nodeID)
		}
		if sourceID := input.Filter.EgressSourceID; sourceID > 0 {
			query = query.Where("EXISTS (SELECT 1 FROM egress_nodes node WHERE node.id = provider_accounts.egress_node_id AND node.source_id = ?)", sourceID)
		}
	case "unbound":
		query = query.Where("egress_node_id IS NULL")
	}
	if input.Filter.Refreshable != nil {
		if *input.Filter.Refreshable {
			query = query.Where("EXISTS (SELECT 1 FROM account_credentials credential WHERE credential.account_id = provider_accounts.id AND credential.encrypted_refresh <> '')")
		} else {
			query = query.Where("NOT EXISTS (SELECT 1 FROM account_credentials credential WHERE credential.account_id = provider_accounts.id AND credential.encrypted_refresh <> '')")
		}
	}
	switch input.Filter.Risk {
	case "flagged":
		query = query.Where("EXISTS (SELECT 1 FROM account_credentials credential WHERE credential.account_id = provider_accounts.id AND credential.build_bot_flag_source IN (1,2))")
	case "normal":
		query = query.Where("NOT EXISTS (SELECT 1 FROM account_credentials credential WHERE credential.account_id = provider_accounts.id AND credential.build_bot_flag_source IN (1,2))")
	}
	query = applyWebAgreementFilter(query, input.Filter.Agreement)
	query = applyAssociationFilter(query, input.Filter.Provider, input.Filter.Association)
	if input.Filter.RestrictIDs {
		if len(input.Filter.AccountIDs) == 0 {
			query = query.Where("1 = 0")
		} else {
			query = query.Where("provider_accounts.id IN ?", input.Filter.AccountIDs)
		}
	}
	if len(input.Filter.ExcludeIDs) > 0 {
		query = query.Where("provider_accounts.id NOT IN ?", input.Filter.ExcludeIDs)
	}
	if input.Filter.AfterID > 0 {
		query = query.Where("provider_accounts.id > ?", input.Filter.AfterID)
	}
	if input.Filter.ThroughID > 0 {
		query = query.Where("provider_accounts.id <= ?", input.Filter.ThroughID)
	}
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []accountModel
	query = applyStableSort(query, input.Page.Sort, map[string]sortSpec{
		"id":        {expression: "provider_accounts.id"},
		"name":      {expression: "LOWER(provider_accounts.name)"},
		"type":      {expression: accountTypeSortExpression},
		"status":    {expression: accountStatusSortExpression},
		"createdAt": {expression: "provider_accounts.created_at", defaultDirection: repository.SortDescending},
	}, sortSpec{expression: "provider_accounts.created_at", defaultDirection: repository.SortDescending}, "provider_accounts.id")
	if err := query.Preload("Credential").Preload("WebProfile").Offset(input.Page.Offset).Limit(input.Page.Limit).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	out := make([]account.Credential, 0, len(rows))
	for _, row := range rows {
		out = append(out, toAccountDomain(row))
	}
	if err := r.attachAccountLinks(ctx, out); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

func (r *AccountRepository) ListProviderAccountBatch(ctx context.Context, providerValue account.Provider, afterID uint64, limit int) ([]account.Credential, int64, error) {
	if limit < 1 {
		return []account.Credential{}, 0, nil
	}
	var total int64
	if afterID == 0 {
		if err := r.db.db.WithContext(ctx).Model(&accountModel{}).Where("provider = ?", providerValue).Count(&total).Error; err != nil {
			return nil, 0, err
		}
	}
	var rows []accountModel
	if err := r.db.db.WithContext(ctx).
		Preload("Credential").Preload("WebProfile").
		Where("provider = ? AND id > ?", providerValue, afterID).
		Order("id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	out := make([]account.Credential, 0, len(rows))
	for _, row := range rows {
		out = append(out, toAccountDomain(row))
	}
	if err := r.attachAccountLinks(ctx, out); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// CountProviderAccountsByIDs 只校验账号主表归属，不加载额度、关联或审计数据。
func (r *AccountRepository) CountProviderAccountsByIDs(ctx context.Context, providerValue account.Provider, ids []uint64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var count int64
	err := r.db.db.WithContext(ctx).Model(&accountModel{}).
		Where("provider = ? AND id IN ?", providerValue, ids).
		Count(&count).Error
	return count, err
}

// CountAvailableAmong counts IDs that currently match Summarize's available predicate.
func (r *AccountRepository) CountAvailableAmong(ctx context.Context, providerValue account.Provider, ids []uint64, now time.Time) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	const batchSize = 500
	var total int64
	for start := 0; start < len(ids); start += batchSize {
		end := min(start+batchSize, len(ids))
		var count int64
		query := r.db.db.WithContext(ctx).Model(&accountModel{}).
			Where("provider = ? AND id IN ?", providerValue, ids[start:end])
		query = applyAccountStatusFilter(query, "active", now)
		if err := query.Count(&count).Error; err != nil {
			return 0, err
		}
		total += count
	}
	return total, nil
}

// UpdateConsoleAPIKeyID stores the Console team apiKeyId cache. Routing
// re-reads credential material per request, so no invalidation is needed.
func (r *AccountRepository) UpdateConsoleAPIKeyID(ctx context.Context, accountID uint64, apiKeyID string) error {
	return r.updateConsoleCredentialCache(ctx, accountID, "console_api_key_id", strings.TrimSpace(apiKeyID), 64)
}

// UpdateConsoleWorkOSSession stores the encrypted WorkOS cookie triple.
func (r *AccountRepository) UpdateConsoleWorkOSSession(ctx context.Context, accountID uint64, encryptedSession string) error {
	return r.updateConsoleCredentialCache(ctx, accountID, "encrypted_console_workos_session", encryptedSession, 65536)
}

func (r *AccountRepository) updateConsoleCredentialCache(ctx context.Context, accountID uint64, column, value string, limit int) error {
	if accountID == 0 || len(value) > limit {
		return repository.ErrInvalidRecord
	}
	return mapError(r.db.db.WithContext(ctx).Model(&accountCredentialModel{}).
		Where("account_id = ? AND account_id IN (?)", accountID,
			r.db.db.Model(&accountModel{}).Select("id").Where("provider = ?", account.ProviderConsole)).
		Update(column, value).Error)
}

func (r *AccountRepository) Summarize(ctx context.Context, now time.Time) ([]repository.AccountSummary, error) {
	var rows []repository.AccountSummary
	selectFields := `
		provider,
		COUNT(*) AS total,
		SUM(CASE WHEN enabled = ? AND auth_status = ? AND NOT ` + accountRecoveryPredicate + ` AND NOT ` + providerQuotaExhaustedPredicate + ` AND (cooldown_until IS NULL OR cooldown_until <= ?) THEN 1 ELSE 0 END) AS available,
		SUM(CASE WHEN enabled = ? AND auth_status = ? AND NOT ` + accountRecoveryPredicate + ` AND NOT ` + providerQuotaExhaustedPredicate + ` AND cooldown_until > ? THEN 1 ELSE 0 END) AS cooldown,
		SUM(CASE WHEN enabled = ? AND auth_status = ? AND (EXISTS (SELECT 1 FROM account_quota_recovery recovery WHERE recovery.account_id = provider_accounts.id AND recovery.status = 'exhausted') OR ` + providerQuotaExhaustedPredicate + `) THEN 1 ELSE 0 END) AS waiting_reset,
		SUM(CASE WHEN enabled = ? AND auth_status = ? AND EXISTS (SELECT 1 FROM account_quota_recovery recovery WHERE recovery.account_id = provider_accounts.id AND recovery.status = 'probing') THEN 1 ELSE 0 END) AS probing,
		SUM(CASE WHEN enabled = ? THEN 1 ELSE 0 END) AS disabled,
		SUM(CASE WHEN enabled = ? AND auth_status = ? THEN 1 ELSE 0 END) AS reauth_required`
	err := r.db.db.WithContext(ctx).Model(&accountModel{}).Select(
		selectFields,
		true, account.AuthStatusActive, now,
		true, account.AuthStatusActive, now,
		true, account.AuthStatusActive,
		true, account.AuthStatusActive,
		false,
		true, account.AuthStatusReauthRequired,
	).Group("provider").Scan(&rows).Error
	return rows, err
}

func (r *AccountRepository) ListEnabledAccountIDs(ctx context.Context, provider account.Provider, refreshableOnly bool) ([]uint64, error) {
	query := r.db.db.WithContext(ctx).
		Table("provider_accounts AS account").
		Select("account.id").
		Where("account.provider = ? AND account.enabled = ? AND account.auth_status = ?", provider, true, account.AuthStatusActive)
	if refreshableOnly {
		query = query.
			Joins("JOIN account_credentials AS credential ON credential.account_id = account.id").
			Where("credential.encrypted_refresh <> ''")
	}
	var ids []uint64
	err := query.Order("account.priority DESC, account.id ASC").Scan(&ids).Error
	return ids, err
}

func (r *AccountRepository) HasActive(ctx context.Context, provider account.Provider) (bool, error) {
	var row struct{ ID uint64 }
	err := r.db.db.WithContext(ctx).Model(&accountModel{}).Select("id").Where("provider = ? AND enabled = ? AND auth_status = ?", provider, true, account.AuthStatusActive).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	return row.ID > 0, err
}

func (r *AccountRepository) Get(ctx context.Context, id uint64) (account.Credential, error) {
	var row accountModel
	if err := r.db.db.WithContext(ctx).Preload("Credential").Preload("WebProfile").First(&row, id).Error; err != nil {
		return account.Credential{}, mapError(err)
	}
	value := toAccountDomain(row)
	values := []account.Credential{value}
	if err := r.attachAccountLinks(ctx, values); err != nil {
		return account.Credential{}, err
	}
	return values[0], nil
}

// FindBySourceKey returns the provider account imported from sourceKey.
func (r *AccountRepository) FindBySourceKey(ctx context.Context, provider account.Provider, sourceKey string) (account.Credential, error) {
	var row accountModel
	if err := r.db.db.WithContext(ctx).Preload("Credential").Preload("WebProfile").Where("provider = ? AND source_key = ?", provider, sourceKey).First(&row).Error; err != nil {
		return account.Credential{}, mapError(err)
	}
	return toAccountDomain(row), nil
}

func (r *AccountRepository) attachAccountLinks(ctx context.Context, values []account.Credential) error {
	if len(values) == 0 {
		return nil
	}
	ids := make([]uint64, 0, len(values))
	positions := make(map[uint64]int, len(values))
	for index := range values {
		ids = append(ids, values[index].ID)
		positions[values[index].ID] = index
	}
	var buildRows []struct {
		WebAccountID            uint64
		BuildAccountID          uint64
		WebName                 string
		BuildName               string
		WebEmail                string
		BuildEmail              string
		WebUserID               string
		BuildUserID             string
		WebSourceKey            string
		EgressIdentity          string
		WebNSFWEnabledAt        *time.Time
		WebTermsAcceptedAt      *time.Time
		WebTermsAcceptedVersion int
	}
	err := r.db.db.WithContext(ctx).Table("account_provider_links AS link").
		Select("link.web_account_id, link.build_account_id, web.name AS web_name, build.name AS build_name, web.email AS web_email, build.email AS build_email, web.user_id AS web_user_id, build.user_id AS build_user_id, web.source_key AS web_source_key, profile.egress_identity, profile.nsfw_enabled_at AS web_nsfw_enabled_at, profile.terms_accepted_at AS web_terms_accepted_at, profile.terms_accepted_version AS web_terms_accepted_version").
		Joins("JOIN provider_accounts AS web ON web.id = link.web_account_id").
		Joins("JOIN provider_accounts AS build ON build.id = link.build_account_id").
		Joins("LEFT JOIN web_account_profiles AS profile ON profile.account_id = web.id").
		Where("link.web_account_id IN ? OR link.build_account_id IN ?", ids, ids).
		Scan(&buildRows).Error
	if err != nil {
		return err
	}
	for _, row := range buildRows {
		egressIdentity := linkedWebEgressIdentity(row.EgressIdentity, row.WebSourceKey)
		if index, ok := positions[row.WebAccountID]; ok {
			values[index].LinkedAccountID = row.BuildAccountID
			values[index].LinkedAccountName = row.BuildName
			values[index].LinkedProvider = account.ProviderBuild
			values[index].LinkedAccounts = append(values[index].LinkedAccounts, account.LinkedAccount{ID: row.BuildAccountID, Provider: account.ProviderBuild, Name: row.BuildName, Email: row.BuildEmail, UserID: row.BuildUserID})
			if values[index].EgressIdentity == "" {
				values[index].EgressIdentity = egressIdentity
			}
			values[index].WebNSFWEnabledAt = row.WebNSFWEnabledAt
			values[index].WebTermsAcceptedVersion = row.WebTermsAcceptedVersion
			values[index].WebTermsAcceptedAt = currentWebTermsAcceptedAt(row.WebTermsAcceptedAt, row.WebTermsAcceptedVersion)
		}
		if index, ok := positions[row.BuildAccountID]; ok {
			values[index].LinkedAccountID = row.WebAccountID
			values[index].LinkedAccountName = row.WebName
			values[index].LinkedProvider = account.ProviderWeb
			values[index].LinkedAccounts = append(values[index].LinkedAccounts, account.LinkedAccount{ID: row.WebAccountID, Provider: account.ProviderWeb, Name: row.WebName, Email: row.WebEmail, UserID: row.WebUserID})
			values[index].EgressIdentity = egressIdentity
			values[index].WebNSFWEnabledAt = row.WebNSFWEnabledAt
			values[index].WebTermsAcceptedVersion = row.WebTermsAcceptedVersion
			values[index].WebTermsAcceptedAt = currentWebTermsAcceptedAt(row.WebTermsAcceptedAt, row.WebTermsAcceptedVersion)
		}
	}
	var consoleRows []struct {
		WebAccountID            uint64
		ConsoleAccountID        uint64
		WebName                 string
		ConsoleName             string
		WebEmail                string
		ConsoleEmail            string
		WebUserID               string
		ConsoleUserID           string
		WebSourceKey            string
		EgressIdentity          string
		WebNSFWEnabledAt        *time.Time
		WebTermsAcceptedAt      *time.Time
		WebTermsAcceptedVersion int
	}
	if err := r.db.db.WithContext(ctx).Table("web_console_account_links AS link").
		Select("link.web_account_id, link.console_account_id, web.name AS web_name, console.name AS console_name, web.email AS web_email, console.email AS console_email, web.user_id AS web_user_id, console.user_id AS console_user_id, web.source_key AS web_source_key, profile.egress_identity, profile.nsfw_enabled_at AS web_nsfw_enabled_at, profile.terms_accepted_at AS web_terms_accepted_at, profile.terms_accepted_version AS web_terms_accepted_version").
		Joins("JOIN provider_accounts AS web ON web.id = link.web_account_id").
		Joins("JOIN provider_accounts AS console ON console.id = link.console_account_id").
		Joins("LEFT JOIN web_account_profiles AS profile ON profile.account_id = web.id").
		Where("link.web_account_id IN ? OR link.console_account_id IN ?", ids, ids).
		Scan(&consoleRows).Error; err != nil {
		return err
	}
	for _, row := range consoleRows {
		egressIdentity := linkedWebEgressIdentity(row.EgressIdentity, row.WebSourceKey)
		if index, ok := positions[row.WebAccountID]; ok {
			values[index].LinkedAccounts = append(values[index].LinkedAccounts, account.LinkedAccount{ID: row.ConsoleAccountID, Provider: account.ProviderConsole, Name: row.ConsoleName, Email: row.ConsoleEmail, UserID: row.ConsoleUserID})
			if values[index].EgressIdentity == "" {
				values[index].EgressIdentity = egressIdentity
			}
			values[index].WebNSFWEnabledAt = row.WebNSFWEnabledAt
			values[index].WebTermsAcceptedVersion = row.WebTermsAcceptedVersion
			values[index].WebTermsAcceptedAt = currentWebTermsAcceptedAt(row.WebTermsAcceptedAt, row.WebTermsAcceptedVersion)
		}
		if index, ok := positions[row.ConsoleAccountID]; ok {
			values[index].LinkedAccounts = append(values[index].LinkedAccounts, account.LinkedAccount{ID: row.WebAccountID, Provider: account.ProviderWeb, Name: row.WebName, Email: row.WebEmail, UserID: row.WebUserID})
			values[index].EgressIdentity = egressIdentity
			values[index].WebNSFWEnabledAt = row.WebNSFWEnabledAt
			values[index].WebTermsAcceptedVersion = row.WebTermsAcceptedVersion
			values[index].WebTermsAcceptedAt = currentWebTermsAcceptedAt(row.WebTermsAcceptedAt, row.WebTermsAcceptedVersion)
		}
	}
	return nil
}

func currentWebTermsAcceptedAt(value *time.Time, version int) *time.Time {
	if version < account.CurrentWebTermsVersion {
		return nil
	}
	return value
}

func linkedWebEgressIdentity(stored, sourceKey string) string {
	if value := strings.TrimSpace(stored); value != "" {
		return value
	}
	value, _ := egressIdentityFromWebSourceKey(sourceKey)
	return value
}

func applyAccountStatusFilter(query *gorm.DB, status string, now time.Time) *gorm.DB {
	switch status {
	case "active":
		return query.Where("enabled = ? AND auth_status = ? AND NOT "+accountRecoveryPredicate+" AND NOT "+providerQuotaExhaustedPredicate+" AND (cooldown_until IS NULL OR cooldown_until <= ?)", true, account.AuthStatusActive, now)
	case "disabled":
		return query.Where("enabled = ?", false)
	case "reauthRequired":
		return query.Where("enabled = ? AND auth_status = ?", true, account.AuthStatusReauthRequired)
	case "cooldown":
		return query.Where("enabled = ? AND auth_status = ? AND NOT "+accountRecoveryPredicate+" AND NOT "+providerQuotaExhaustedPredicate+" AND cooldown_until > ?", true, account.AuthStatusActive, now)
	case "waitingReset":
		return query.Where("enabled = ? AND auth_status = ? AND (EXISTS (SELECT 1 FROM account_quota_recovery recovery WHERE recovery.account_id = provider_accounts.id AND recovery.status = 'exhausted') OR "+providerQuotaExhaustedPredicate+")", true, account.AuthStatusActive)
	case "probing":
		return query.Where("enabled = ? AND auth_status = ? AND EXISTS (SELECT 1 FROM account_quota_recovery recovery WHERE recovery.account_id = provider_accounts.id AND recovery.status = 'probing')", true, account.AuthStatusActive)
	default:
		return query
	}
}

// Web agreement predicates match the effective state exposed by the admin API.
// Terms are current only when the recorded version reaches CurrentWebTermsVersion.
const (
	webNSFWEnabledPredicate   = "EXISTS (SELECT 1 FROM web_account_profiles profile WHERE profile.account_id = provider_accounts.id AND profile.nsfw_enabled_at IS NOT NULL)"
	webTermsAcceptedPredicate = "EXISTS (SELECT 1 FROM web_account_profiles profile WHERE profile.account_id = provider_accounts.id AND profile.terms_accepted_at IS NOT NULL AND profile.terms_accepted_version >= ?)"
	webBuildLinkedPredicate   = "EXISTS (SELECT 1 FROM account_provider_links link WHERE link.web_account_id = provider_accounts.id)"
	webConsoleLinkedPredicate = "EXISTS (SELECT 1 FROM web_console_account_links link WHERE link.web_account_id = provider_accounts.id)"
	// Build and Console filter by whether a Web link exists.
	buildWebLinkedPredicate   = "EXISTS (SELECT 1 FROM account_provider_links link WHERE link.build_account_id = provider_accounts.id)"
	consoleWebLinkedPredicate = "EXISTS (SELECT 1 FROM web_console_account_links link WHERE link.console_account_id = provider_accounts.id)"
)

func applyWebAgreementFilter(query *gorm.DB, agreement string) *gorm.DB {
	switch agreement {
	case "nsfwEnabled":
		return query.Where(webNSFWEnabledPredicate)
	case "nsfwDisabled":
		return query.Where("NOT " + webNSFWEnabledPredicate)
	case "termsAccepted":
		return query.Where(webTermsAcceptedPredicate, account.CurrentWebTermsVersion)
	case "termsNotAccepted":
		return query.Where("NOT "+webTermsAcceptedPredicate, account.CurrentWebTermsVersion)
	case "allAccepted":
		return query.Where(webNSFWEnabledPredicate).Where(webTermsAcceptedPredicate, account.CurrentWebTermsVersion)
	case "allNotAccepted":
		return query.Where("NOT "+webNSFWEnabledPredicate).Where("NOT "+webTermsAcceptedPredicate, account.CurrentWebTermsVersion)
	default:
		return query
	}
}

// applyAssociationFilter applies provider-specific association predicates.
// Web supports Build, Console, and combined filters; Build and Console use
// provider-specific foreign keys for webLinked and webUnlinked.
func applyAssociationFilter(query *gorm.DB, providerValue, association string) *gorm.DB {
	switch association {
	case "buildLinked":
		return query.Where(webBuildLinkedPredicate)
	case "buildUnlinked":
		return query.Where("NOT " + webBuildLinkedPredicate)
	case "consoleLinked":
		return query.Where(webConsoleLinkedPredicate)
	case "consoleUnlinked":
		return query.Where("NOT " + webConsoleLinkedPredicate)
	case "allLinked":
		return query.Where(webBuildLinkedPredicate).Where(webConsoleLinkedPredicate)
	case "allUnlinked":
		return query.Where("NOT " + webBuildLinkedPredicate).Where("NOT " + webConsoleLinkedPredicate)
	case "webLinked":
		if providerValue == string(account.ProviderConsole) {
			return query.Where(consoleWebLinkedPredicate)
		}
		return query.Where(buildWebLinkedPredicate)
	case "webUnlinked":
		if providerValue == string(account.ProviderConsole) {
			return query.Where("NOT " + consoleWebLinkedPredicate)
		}
		return query.Where("NOT " + buildWebLinkedPredicate)
	default:
		return query
	}
}
