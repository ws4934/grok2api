package relational

import (
	"context"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// ListRoutingCandidates 批量加载账号、额度、恢复状态和目标模型能力，避免推理热路径按账号逐条查询。
func (r *AccountRepository) ListRoutingCandidates(ctx context.Context, provider account.Provider, modelRouteID uint64, upstreamModel, quotaMode string) ([]account.RoutingCandidate, error) {
	values, err := r.listRoutingCredentials(ctx, provider)
	if err != nil {
		return nil, err
	}
	bound := make(map[uint64]bool)
	if strings.TrimSpace(upstreamModel) != "" {
		boundIDs, loadErr := r.listRoutingBoundAccountIDs(ctx, provider, modelRouteID, upstreamModel)
		if loadErr != nil {
			return nil, loadErr
		}
		if len(boundIDs) > 0 {
			for _, id := range boundIDs {
				bound[id] = true
			}
			filtered := values[:0]
			for _, value := range values {
				if bound[value.ID] {
					filtered = append(filtered, value)
				}
			}
			values = filtered
		}
	}
	billings, err := r.getRoutingBillings(ctx, provider)
	if err != nil {
		return nil, err
	}
	recoveries, err := r.getRoutingQuotaRecoveries(ctx, provider)
	if err != nil {
		return nil, err
	}
	quotaWindows, err := r.getRoutingQuotaWindows(ctx, provider, quotaMode, values)
	if err != nil {
		return nil, err
	}
	egressLeaseBlocks, err := r.getRoutingEgressLeaseBlocks(ctx, provider, values, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	known := make(map[uint64]bool, len(values))
	supported := make(map[uint64]bool, len(values))
	modelQuotaBlocks := make(map[uint64]account.ModelQuotaBlock, len(values))
	if strings.TrimSpace(upstreamModel) != "" && len(values) > 0 {
		var states []accountModelSyncStateModel
		if err := r.db.db.WithContext(ctx).
			Table("account_model_sync_states AS state").
			Select("state.*").
			Joins("JOIN provider_accounts AS account ON account.id = state.account_id").
			Where("account.provider = ? AND account.enabled = ? AND account.auth_status = ? AND state.last_success_at IS NOT NULL", provider, true, account.AuthStatusActive).
			Find(&states).Error; err != nil {
			return nil, err
		}
		for _, state := range states {
			known[state.AccountID] = true
		}
		var capabilities []accountModelCapabilityModel
		if err := r.db.db.WithContext(ctx).
			Table("account_model_capabilities AS capability").
			Select("capability.*").
			Joins("JOIN provider_accounts AS account ON account.id = capability.account_id").
			Where("account.provider = ? AND account.enabled = ? AND account.auth_status = ? AND capability.upstream_model = ?", provider, true, account.AuthStatusActive, upstreamModel).
			Find(&capabilities).Error; err != nil {
			return nil, err
		}
		for _, capability := range capabilities {
			supported[capability.AccountID] = true
		}
		var blockRows []accountModelQuotaBlockModel
		if err := r.db.db.WithContext(ctx).
			Table("account_model_quota_blocks AS block").
			Select("block.*").
			Joins("JOIN provider_accounts AS account ON account.id = block.account_id").
			Where("account.provider = ? AND account.enabled = ? AND account.auth_status = ? AND block.upstream_model = ? AND block.cooldown_until > ?", provider, true, account.AuthStatusActive, upstreamModel, time.Now().UTC()).
			Find(&blockRows).Error; err != nil {
			return nil, err
		}
		for _, row := range blockRows {
			modelQuotaBlocks[row.AccountID] = account.ModelQuotaBlock{AccountID: row.AccountID, UpstreamModel: row.UpstreamModel, Reason: row.Reason, CooldownUntil: row.CooldownUntil.UTC(), UpdatedAt: row.UpdatedAt.UTC()}
		}
	}
	sharedSuperBuildModel := false
	if provider == account.ProviderBuild && len(bound) == 0 {
		for _, value := range values {
			if !supported[value.ID] {
				continue
			}
			var billing *account.Billing
			if snapshot, exists := billings[value.ID]; exists {
				billing = &snapshot
			}
			if account.IsBuildSuper(value, billing) {
				sharedSuperBuildModel = true
				break
			}
		}
	}
	result := make([]account.RoutingCandidate, 0, len(values))
	staticProviderModel := (provider == account.ProviderConsole && strings.TrimSpace(quotaMode) != "") ||
		(provider == account.ProviderWeb && account.IsWebImagineQuotaMode(quotaMode))
	for _, value := range values {
		capabilityKnown, supportsModel := known[value.ID], supported[value.ID]
		if staticProviderModel {
			// Console and Web Imagine expose provider-wide static catalogs.
			// Historical account snapshots may predate newly shipped catalog
			// entries, but must not make those routes unroutable. A recognized
			// quota mode proves the adapter knows the model; unknown/manual models
			// keep snapshot-based gating.
			capabilityKnown, supportsModel = true, true
		} else if len(bound) > 0 {
			capabilityKnown, supportsModel = true, true
		} else if sharedSuperBuildModel {
			var billing *account.Billing
			if snapshot, exists := billings[value.ID]; exists {
				billing = &snapshot
			}
			if account.IsBuildSuper(value, billing) {
				capabilityKnown, supportsModel = true, true
			}
		}
		candidate := account.RoutingCandidate{Credential: value, ModelCapabilityKnown: capabilityKnown, SupportsModel: supportsModel}
		if billing, ok := billings[value.ID]; ok {
			candidate.Billing = &billing
		}
		if recovery, ok := recoveries[value.ID]; ok {
			candidate.QuotaRecovery = &recovery
		}
		if window, ok := quotaWindows[value.ID]; ok {
			candidate.QuotaWindow = &window
		}
		if block, ok := modelQuotaBlocks[value.ID]; ok {
			candidate.ModelQuotaBlock = &block
		}
		if block, ok := egressLeaseBlocks[value.ID]; ok {
			candidate.EgressLeaseBlock = &block
		}
		result = append(result, candidate)
	}
	return result, nil
}

func (r *AccountRepository) ListRoutingAccountBases(ctx context.Context, provider account.Provider, quotaMode string) ([]account.RoutingAccountBase, error) {
	values, err := r.listRoutingCredentials(ctx, provider)
	if err != nil {
		return nil, err
	}
	billings, err := r.getRoutingBillings(ctx, provider)
	if err != nil {
		return nil, err
	}
	recoveries, err := r.getRoutingQuotaRecoveries(ctx, provider)
	if err != nil {
		return nil, err
	}
	quotaWindows, err := r.getRoutingQuotaWindows(ctx, provider, quotaMode, values)
	if err != nil {
		return nil, err
	}
	egressLeaseBlocks, err := r.getRoutingEgressLeaseBlocks(ctx, provider, values, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	result := make([]account.RoutingAccountBase, 0, len(values))
	for _, value := range values {
		base := account.RoutingAccountBase{Credential: value}
		if billing, ok := billings[value.ID]; ok {
			base.Billing = &billing
		}
		if recovery, ok := recoveries[value.ID]; ok {
			base.QuotaRecovery = &recovery
		}
		if window, ok := quotaWindows[value.ID]; ok {
			base.QuotaWindow = &window
		}
		if block, ok := egressLeaseBlocks[value.ID]; ok {
			base.EgressLeaseBlock = &block
		}
		result = append(result, base)
	}
	return result, nil
}

func (r *AccountRepository) getRoutingEgressLeaseBlocks(ctx context.Context, provider account.Provider, values []account.Credential, now time.Time) (map[uint64]account.EgressLeaseBlock, error) {
	result := make(map[uint64]account.EgressLeaseBlock)
	if len(values) == 0 {
		return result, nil
	}
	var rows []accountEgressLeaseBlockModel
	if err := r.db.db.WithContext(ctx).
		Table("account_egress_lease_blocks AS block").
		Select("block.*").
		Joins("JOIN provider_accounts AS account ON account.id = block.account_id").
		Where("account.provider = ? AND account.enabled = ? AND account.auth_status = ? AND (account.egress_node_id IS NULL OR account.egress_node_id = block.node_id) AND block.cooldown_until > ?", provider, true, account.AuthStatusActive, now.UTC()).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		block := egressLeaseBlockFromModel(row)
		current, exists := result[row.AccountID]
		if !exists || block.CooldownUntil.After(current.CooldownUntil) {
			result[row.AccountID] = block
		}
	}
	return result, nil
}

// listRoutingCredentials loads only the account state required to decide which
// account to use. Provider secrets deliberately stay in account_credentials
// until a selected account is hydrated for the upstream call.
func (r *AccountRepository) listRoutingCredentials(ctx context.Context, provider account.Provider) ([]account.Credential, error) {
	rows, err := r.listActiveProviderAccountRows(ctx, provider, routingCredentialMetadataColumns)
	if err != nil {
		return nil, err
	}
	values := make([]account.Credential, 0, len(rows))
	for _, row := range rows {
		values = append(values, toAccountDomain(row))
	}
	if err := r.attachRoutingEgressIdentities(ctx, provider, values); err != nil {
		return nil, err
	}
	return values, nil
}

// listActiveProviderAccountRows avoids GORM association preloads for complete
// provider pools. Preload expands every parent key into an IN list and exceeds
// SQLite's variable limit for large pools. The fixed-shape JOIN queries below
// remain valid for both SQLite and PostgreSQL regardless of pool size.
func (r *AccountRepository) listActiveProviderAccountRows(ctx context.Context, provider account.Provider, credentialColumns []string) ([]accountModel, error) {
	var rows []accountModel
	if err := r.db.db.WithContext(ctx).
		Where("provider = ? AND enabled = ? AND auth_status = ?", provider, true, account.AuthStatusActive).
		Order("priority DESC, id ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return rows, nil
	}
	positions := make(map[uint64]int, len(rows))
	for index := range rows {
		positions[rows[index].ID] = index
	}

	credentialSelect := "credential.*"
	if len(credentialColumns) > 0 {
		credentialSelect = qualifiedColumnList("credential", credentialColumns)
	}
	var credentials []accountCredentialModel
	if err := r.db.db.WithContext(ctx).
		Table("account_credentials AS credential").
		Select(credentialSelect).
		Joins("JOIN provider_accounts AS account ON account.id = credential.account_id").
		Where("account.provider = ? AND account.enabled = ? AND account.auth_status = ?", provider, true, account.AuthStatusActive).
		Find(&credentials).Error; err != nil {
		return nil, err
	}
	for index := range credentials {
		if position, ok := positions[credentials[index].AccountID]; ok {
			rows[position].Credential = &credentials[index]
		}
	}

	if provider == account.ProviderWeb {
		var profiles []webAccountProfileModel
		if err := r.db.db.WithContext(ctx).
			Table("web_account_profiles AS profile").
			Select("profile.*").
			Joins("JOIN provider_accounts AS account ON account.id = profile.account_id").
			Where("account.provider = ? AND account.enabled = ? AND account.auth_status = ?", provider, true, account.AuthStatusActive).
			Find(&profiles).Error; err != nil {
			return nil, err
		}
		for index := range profiles {
			if position, ok := positions[profiles[index].AccountID]; ok {
				rows[position].WebProfile = &profiles[index]
			}
		}
	}
	return rows, nil
}

func qualifiedColumnList(alias string, columns []string) string {
	qualified := make([]string, 0, len(columns))
	for _, column := range columns {
		qualified = append(qualified, alias+"."+column)
	}
	return strings.Join(qualified, ", ")
}

// routingCredentialMetadataColumns contains all credential fields used for
// routing and execution decisions, but deliberately excludes the encrypted
// access token, refresh token, and Cloudflare cookie.
var routingCredentialMetadataColumns = []string{
	"account_id", "auth_type", "client_id", "expires_at", "refresh_due_at", "last_refresh_at",
	"refresh_failures", "last_refresh_error", "refresh_permanent", "build_bot_flag_source", "updated_at",
}

var routingBillingColumns = []string{
	"account_id", "plan_code", "plan_name", "monthly_limit", "used", "on_demand_cap", "on_demand_used", "prepaid_balance",
	"credit_usage_percent", "is_unified_billing_user", "on_demand_enabled", "top_up_method", "usage_period_type",
	"usage_period_start", "usage_period_end", "billing_period_start", "billing_period_end", "synced_at",
}

func (r *AccountRepository) getRoutingBillings(ctx context.Context, provider account.Provider) (map[uint64]account.Billing, error) {
	result := make(map[uint64]account.Billing)
	var rows []billingModel
	if err := r.db.db.WithContext(ctx).
		Table("account_billing_snapshots AS billing").
		Select(qualifiedColumnList("billing", routingBillingColumns)).
		Joins("JOIN provider_accounts AS account ON account.id = billing.account_id").
		Where("account.provider = ? AND account.enabled = ? AND account.auth_status = ?", provider, true, account.AuthStatusActive).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.AccountID] = toRoutingBillingDomain(row)
	}
	return result, nil
}

func (r *AccountRepository) getRoutingQuotaRecoveries(ctx context.Context, provider account.Provider) (map[uint64]account.QuotaRecovery, error) {
	result := make(map[uint64]account.QuotaRecovery)
	var rows []quotaRecoveryModel
	if err := r.db.db.WithContext(ctx).
		Table("account_quota_recovery AS recovery").
		Select("recovery.*").
		Joins("JOIN provider_accounts AS account ON account.id = recovery.account_id").
		Where("account.provider = ? AND account.enabled = ? AND account.auth_status = ?", provider, true, account.AuthStatusActive).
		Find(&rows).Error; err != nil {
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

var routingQuotaWindowColumns = []string{
	"account_id", "mode", "remaining", "total", "usage_percent", "window_seconds", "reset_at", "synced_at", "source", "updated_at",
}

func (r *AccountRepository) getRoutingQuotaWindows(ctx context.Context, provider account.Provider, quotaMode string, credentials []account.Credential) (map[uint64]account.QuotaWindow, error) {
	result := make(map[uint64]account.QuotaWindow)
	if provider != account.ProviderWeb && quotaMode == "" {
		return result, nil
	}
	modes := make([]string, 0, 3)
	webImagineMode := provider == account.ProviderWeb && account.IsWebImagineQuotaMode(quotaMode)
	// Paid Web routes use the shared weekly pool. Imagine may additionally
	// expose a product-specific remainingQueries window (notably for Basic and
	// older response shapes); load both and prefer the exact product window
	// below, falling back to weekly only for confirmed Super/Heavy accounts.
	if provider == account.ProviderWeb {
		modes = append(modes, "weekly")
	}
	if provider == account.ProviderWeb && quotaMode == account.QuotaModeWebImageEdit {
		// Basic Web accounts use image_pro for editing, while Super/Heavy
		// accounts have the dedicated image_edit product. Load both once and
		// select the authoritative window per account below.
		modes = append(modes, account.QuotaModeWebImagePro)
	}
	if quotaMode != "" {
		modes = append(modes, quotaMode)
	}
	var rows []quotaWindowModel
	if err := r.db.db.WithContext(ctx).
		Table("account_quota_windows AS quota").
		Select(qualifiedColumnList("quota", routingQuotaWindowColumns)).
		Joins("JOIN provider_accounts AS account ON account.id = quota.account_id").
		Where("account.provider = ? AND account.enabled = ? AND account.auth_status = ? AND quota.mode IN ?", provider, true, account.AuthStatusActive, modes).
		Order("CASE WHEN quota.mode = 'weekly' THEN 0 ELSE 1 END").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	webTiers := make(map[uint64]account.WebTier, len(credentials))
	for _, credential := range credentials {
		webTiers[credential.ID] = credential.WebTier
	}
	for _, row := range rows {
		if webImagineMode {
			tier := webTiers[row.AccountID]
			productMode := quotaMode
			if quotaMode == account.QuotaModeWebImageEdit {
				productMode = webImageEditRoutingQuotaMode(tier)
			}
			switch {
			case row.Mode == productMode:
				// An explicit product window is more precise than the shared pool,
				// regardless of query order.
				result[row.AccountID] = toRoutingQuotaWindowDomain(row)
			case row.Mode == "weekly" && (tier == account.WebTierSuper || tier == account.WebTierHeavy):
				if existing, exists := result[row.AccountID]; !exists || existing.Mode != productMode {
					result[row.AccountID] = toRoutingQuotaWindowDomain(row)
				}
			}
			continue
		}
		if _, exists := result[row.AccountID]; !exists {
			result[row.AccountID] = toRoutingQuotaWindowDomain(row)
		}
	}
	return result, nil
}

func webImageEditRoutingQuotaMode(tier account.WebTier) string {
	switch tier {
	case account.WebTierSuper, account.WebTierHeavy:
		return account.QuotaModeWebImageEdit
	default:
		// Empty and auto tiers are deliberately treated as Basic, matching the
		// Web adapter's conservative capability normalization.
		return account.QuotaModeWebImagePro
	}
}

func (r *AccountRepository) ListRoutingAccountOverlays(ctx context.Context, provider account.Provider, modelRouteID uint64, upstreamModel string) (account.RoutingOverlaySnapshot, error) {
	upstreamModel = strings.TrimSpace(upstreamModel)
	if upstreamModel == "" {
		return account.RoutingOverlaySnapshot{}, nil
	}
	boundIDs, err := r.listRoutingBoundAccountIDs(ctx, provider, modelRouteID, upstreamModel)
	if err != nil {
		return account.RoutingOverlaySnapshot{}, err
	}
	values := make(map[uint64]account.RoutingAccountOverlay)
	for _, id := range boundIDs {
		values[id] = account.RoutingAccountOverlay{AccountID: id, Bound: true, ModelCapabilityKnown: true, SupportsModel: true}
	}
	var states []accountModelSyncStateModel
	if err := r.db.db.WithContext(ctx).
		Table("account_model_sync_states AS state").
		Select("state.account_id").
		Joins("JOIN provider_accounts AS account ON account.id = state.account_id").
		Where("account.provider = ? AND account.enabled = TRUE AND state.last_success_at IS NOT NULL", provider).
		Find(&states).Error; err != nil {
		return account.RoutingOverlaySnapshot{}, err
	}
	for _, state := range states {
		overlay := values[state.AccountID]
		overlay.AccountID = state.AccountID
		overlay.ModelCapabilityKnown = true
		values[state.AccountID] = overlay
	}
	var capabilities []accountModelCapabilityModel
	if err := r.db.db.WithContext(ctx).
		Table("account_model_capabilities AS capability").
		Select("capability.account_id").
		Joins("JOIN provider_accounts AS account ON account.id = capability.account_id").
		Where("account.provider = ? AND account.enabled = TRUE AND capability.upstream_model = ?", provider, upstreamModel).
		Find(&capabilities).Error; err != nil {
		return account.RoutingOverlaySnapshot{}, err
	}
	for _, capability := range capabilities {
		overlay := values[capability.AccountID]
		overlay.AccountID = capability.AccountID
		overlay.SupportsModel = true
		values[capability.AccountID] = overlay
	}
	var blockRows []accountModelQuotaBlockModel
	if err := r.db.db.WithContext(ctx).
		Table("account_model_quota_blocks AS block").
		Select("block.account_id", "block.upstream_model", "block.reason", "block.cooldown_until", "block.updated_at").
		Joins("JOIN provider_accounts AS account ON account.id = block.account_id").
		Where("account.provider = ? AND account.enabled = TRUE AND block.upstream_model = ? AND block.cooldown_until > ?", provider, upstreamModel, time.Now().UTC()).
		Find(&blockRows).Error; err != nil {
		return account.RoutingOverlaySnapshot{}, err
	}
	for _, row := range blockRows {
		overlay := values[row.AccountID]
		overlay.AccountID = row.AccountID
		overlay.ModelQuotaBlock = &account.ModelQuotaBlock{AccountID: row.AccountID, UpstreamModel: row.UpstreamModel, Reason: row.Reason, CooldownUntil: row.CooldownUntil.UTC(), UpdatedAt: row.UpdatedAt.UTC()}
		values[row.AccountID] = overlay
	}
	result := account.RoutingOverlaySnapshot{HasBindings: len(boundIDs) > 0, Values: make([]account.RoutingAccountOverlay, 0, len(values))}
	for _, value := range values {
		result.Values = append(result.Values, value)
	}
	return result, nil
}

func (r *AccountRepository) listRoutingBoundAccountIDs(ctx context.Context, provider account.Provider, modelRouteID uint64, upstreamModel string) ([]uint64, error) {
	query := r.db.db.WithContext(ctx).
		Table("model_route_accounts AS binding").
		Select("binding.account_id").
		Joins("JOIN model_routes AS route ON route.id = binding.model_route_id")
	if modelRouteID > 0 {
		query = query.Where("route.id = ? AND route.provider = ? AND route.upstream_model = ?", modelRouteID, provider, upstreamModel)
	} else {
		query = query.Where("route.provider = ? AND route.upstream_model = ?", provider, upstreamModel)
	}
	var accountIDs []uint64
	if err := query.Scan(&accountIDs).Error; err != nil {
		return nil, err
	}
	return accountIDs, nil
}

func (r *AccountRepository) ListEnabled(ctx context.Context, provider account.Provider) ([]account.Credential, error) {
	rows, err := r.listActiveProviderAccountRows(ctx, provider, nil)
	if err != nil {
		return nil, err
	}
	out := make([]account.Credential, 0, len(rows))
	for _, row := range rows {
		out = append(out, toAccountDomain(row))
	}
	if err := r.attachRoutingEgressIdentities(ctx, provider, out); err != nil {
		return nil, err
	}
	return out, nil
}

// attachRoutingEgressIdentities 只补充推理路由需要的稳定出口身份。
// 管理端展示所需的账号名称和 linkedAccounts 仍由 attachAccountLinks 加载，
// 避免路由候选缓存刷新时额外查询两类完整关系。
func (r *AccountRepository) attachRoutingEgressIdentities(ctx context.Context, provider account.Provider, values []account.Credential) error {
	if len(values) == 0 || provider == account.ProviderWeb {
		return nil
	}
	positions := make(map[uint64]int, len(values))
	for index := range values {
		positions[values[index].ID] = index
	}
	type identityRow struct {
		AccountID      uint64
		WebSourceKey   string
		EgressIdentity string
	}
	var rows []identityRow
	query := r.db.db.WithContext(ctx)
	switch provider {
	case account.ProviderBuild:
		query = query.Table("account_provider_links AS link").
			Select("link.build_account_id AS account_id, web.source_key AS web_source_key, profile.egress_identity").
			Joins("JOIN provider_accounts AS target ON target.id = link.build_account_id").
			Joins("JOIN provider_accounts AS web ON web.id = link.web_account_id").
			Joins("LEFT JOIN web_account_profiles AS profile ON profile.account_id = web.id").
			Where("target.provider = ? AND target.enabled = ? AND target.auth_status = ?", provider, true, account.AuthStatusActive)
	case account.ProviderConsole:
		query = query.Table("web_console_account_links AS link").
			Select("link.console_account_id AS account_id, web.source_key AS web_source_key, profile.egress_identity").
			Joins("JOIN provider_accounts AS target ON target.id = link.console_account_id").
			Joins("JOIN provider_accounts AS web ON web.id = link.web_account_id").
			Joins("LEFT JOIN web_account_profiles AS profile ON profile.account_id = web.id").
			Where("target.provider = ? AND target.enabled = ? AND target.auth_status = ?", provider, true, account.AuthStatusActive)
	default:
		return nil
	}
	if err := query.Scan(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		if index, ok := positions[row.AccountID]; ok {
			values[index].EgressIdentity = linkedWebEgressIdentity(row.EgressIdentity, row.WebSourceKey)
		}
	}
	return nil
}

func toRoutingQuotaWindowDomain(row quotaWindowModel) account.QuotaWindow {
	return account.QuotaWindow{
		AccountID: row.AccountID, Mode: row.Mode, Remaining: row.Remaining, Total: row.Total,
		UsagePercent: row.UsagePercent, WindowSeconds: row.WindowSeconds,
		ResetAt: row.ResetAt, SyncedAt: row.SyncedAt, Source: account.QuotaSource(row.Source), UpdatedAt: row.UpdatedAt,
	}
}
