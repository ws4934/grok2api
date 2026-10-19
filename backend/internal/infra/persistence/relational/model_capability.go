package relational

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Console static support is anchored to the reconciled catalog routes instead
// of the provider name alone. Manual aliases remain supported while an
// equivalent catalog route exists, and stale aliases stop advertising support
// as soon as that catalog entry is removed.
const modelConsoleStaticSupportExpression = `(model_routes.provider = 'grok_console' AND (
	model_routes.origin = 'catalog'
	OR EXISTS (
		SELECT 1 FROM model_routes console_catalog_route
		WHERE console_catalog_route.provider = model_routes.provider
			AND console_catalog_route.upstream_model = model_routes.upstream_model
			AND console_catalog_route.capability = model_routes.capability
			AND console_catalog_route.origin = 'catalog'
	)
))`

const modelConsoleStaticSupportAvailabilityExpression = `(route.provider = 'grok_console' AND (
	route.origin = 'catalog'
	OR EXISTS (
		SELECT 1 FROM model_routes console_catalog_route
		WHERE console_catalog_route.provider = route.provider
			AND console_catalog_route.upstream_model = route.upstream_model
			AND console_catalog_route.capability = route.capability
			AND console_catalog_route.origin = 'catalog'
	)
))`

// These Web media catalog capabilities are available to Basic accounts for
// their confirmed quota products. Older capability snapshots may predate that
// support, so catalog routes (and their aliases) remain discoverable while the
// runtime selector enforces the adapter's parameter-specific tier order.
const modelWebBasicMediaStaticSupportExpression = `(model_routes.provider = 'grok_web'
	AND model_routes.upstream_model IN ('grok-imagine-image-quality', 'grok-imagine-image-2.0', 'imagine-image-edit', 'grok-imagine-video')
	AND (
		model_routes.origin = 'catalog'
		OR EXISTS (
			SELECT 1 FROM model_routes web_catalog_route
			WHERE web_catalog_route.provider = model_routes.provider
				AND web_catalog_route.upstream_model = model_routes.upstream_model
				AND web_catalog_route.capability = model_routes.capability
				AND web_catalog_route.origin = 'catalog'
		)
	))`

const modelWebBasicMediaStaticSupportAvailabilityExpression = `(route.provider = 'grok_web'
	AND route.upstream_model IN ('grok-imagine-image-quality', 'grok-imagine-image-2.0', 'imagine-image-edit', 'grok-imagine-video')
	AND (
		route.origin = 'catalog'
		OR EXISTS (
			SELECT 1 FROM model_routes web_catalog_route
			WHERE web_catalog_route.provider = route.provider
				AND web_catalog_route.upstream_model = route.upstream_model
				AND web_catalog_route.capability = route.capability
				AND web_catalog_route.origin = 'catalog'
		)
	))`

const availableRoutePredicate = `
	EXISTS (
		SELECT 1 FROM provider_accounts account
		WHERE account.provider = model_routes.provider
			AND account.enabled = ?
			AND account.auth_status = ?
			AND (
				EXISTS (
					SELECT 1 FROM model_route_accounts binding
					WHERE binding.model_route_id = model_routes.id
						AND binding.account_id = account.id
				)
				OR (
					NOT EXISTS (SELECT 1 FROM model_route_accounts binding WHERE binding.model_route_id = model_routes.id)
					AND (
						` + modelConsoleStaticSupportExpression + `
						OR ` + modelWebBasicMediaStaticSupportExpression + `
						OR EXISTS (
							SELECT 1 FROM account_model_capabilities capability
							WHERE capability.account_id = account.id
								AND capability.upstream_model = model_routes.upstream_model
						)
					)
				)
			)
	)
`

// Build Super 共享能力：Billing paid 或 build_super_entitled；仅 grok_build。
const modelAccountBuildSuperPredicate = `(EXISTS (SELECT 1 FROM account_billing_snapshots billing WHERE billing.account_id = account.id AND ` + accountPaidBillingSignals + `) OR (account.provider = 'grok_build' AND account.build_super_entitled = TRUE))`
const modelPeerBuildSuperPredicate = `(EXISTS (SELECT 1 FROM account_billing_snapshots billing WHERE billing.account_id = peer.id AND ` + accountPaidBillingSignals + `) OR (peer.provider = 'grok_build' AND peer.build_super_entitled = TRUE))`

const modelSharedPaidBuildSupportSortExpression = `(model_routes.provider = 'grok_build'
	AND ` + modelAccountBuildSuperPredicate + `
	AND EXISTS (
		SELECT 1
		FROM provider_accounts peer
		JOIN account_model_capabilities peer_capability ON peer_capability.account_id = peer.id AND peer_capability.upstream_model = model_routes.upstream_model
		WHERE peer.provider = model_routes.provider
			AND peer.enabled = TRUE
			AND peer.auth_status = 'active'
			AND ` + modelPeerBuildSuperPredicate + `
	))`

const modelSharedPaidBuildSupportAvailabilityExpression = `(route.provider = 'grok_build'
	AND ` + modelAccountBuildSuperPredicate + `
	AND EXISTS (
		SELECT 1
		FROM provider_accounts peer
		JOIN account_model_capabilities peer_capability ON peer_capability.account_id = peer.id AND peer_capability.upstream_model = route.upstream_model
		WHERE peer.provider = route.provider
			AND peer.enabled = TRUE
			AND peer.auth_status = 'active'
			AND ` + modelPeerBuildSuperPredicate + `
	))`

// These predicates mirror the gateway's client-key scope classification. They
// are used only by the admin model picker to avoid presenting routes that the
// selected account scope can never serve.
const modelAccountBuildFreePredicate = `(account.provider = 'grok_build'
	AND NOT ` + modelAccountBuildSuperPredicate + `
	AND (
		EXISTS (SELECT 1 FROM account_quota_recovery recovery WHERE recovery.account_id = account.id AND recovery.kind = 'free')
		OR LOWER(TRIM(account.observed_model)) LIKE '%-build-free'
		OR EXISTS (SELECT 1 FROM account_billing_snapshots billing WHERE billing.account_id = account.id AND ` + accountFreeBillingSignal + `)
	))`

const modelAccountBuildSuperTierPredicate = `(account.provider = 'grok_build' AND ` + modelAccountBuildSuperPredicate + `)`

const modelAccountWebFreePredicate = `(account.provider = 'grok_web'
	AND EXISTS (SELECT 1 FROM web_account_profiles profile WHERE profile.account_id = account.id AND profile.tier = 'basic'))`

const modelAccountWebSuperPredicate = `(account.provider = 'grok_web'
	AND EXISTS (SELECT 1 FROM web_account_profiles profile WHERE profile.account_id = account.id AND profile.tier IN ('super', 'heavy')))`

const modelSharedPaidBuildScopeExpression = `(model_routes.provider = 'grok_build'
	AND ` + modelAccountBuildSuperPredicate + `
	AND EXISTS (
		SELECT 1
		FROM provider_accounts peer
		JOIN account_model_capabilities peer_capability ON peer_capability.account_id = peer.id AND peer_capability.upstream_model = model_routes.upstream_model
		WHERE peer.provider = model_routes.provider
			AND ` + modelPeerBuildSuperPredicate + `
	))`

const modelRouteAccountCapabilityPredicate = `(
	EXISTS (
		SELECT 1 FROM model_route_accounts binding
		WHERE binding.model_route_id = model_routes.id
			AND binding.account_id = account.id
	)
	OR (
		NOT EXISTS (SELECT 1 FROM model_route_accounts binding WHERE binding.model_route_id = model_routes.id)
		AND (
			` + modelConsoleStaticSupportExpression + `
			OR ` + modelWebBasicMediaStaticSupportExpression + `
			OR EXISTS (
				SELECT 1 FROM account_model_capabilities capability
				WHERE capability.account_id = account.id
					AND capability.upstream_model = model_routes.upstream_model
			)
			OR ` + modelSharedPaidBuildScopeExpression + `
		)
	)
)`

const modelAvailableRouteAccountCapabilityPredicate = `(
	EXISTS (
		SELECT 1 FROM model_route_accounts binding
		WHERE binding.model_route_id = model_routes.id
			AND binding.account_id = account.id
	)
	OR (
		NOT EXISTS (SELECT 1 FROM model_route_accounts binding WHERE binding.model_route_id = model_routes.id)
		AND (
			` + modelConsoleStaticSupportExpression + `
			OR ` + modelWebBasicMediaStaticSupportExpression + `
			OR EXISTS (
				SELECT 1 FROM account_model_capabilities capability
				WHERE capability.account_id = account.id
					AND capability.upstream_model = model_routes.upstream_model
			)
			OR ` + modelSharedPaidBuildSupportSortExpression + `
		)
	)
)`

func modelTierAvailabilityPredicate(tiers, exempt []string) string {
	return modelTierAvailabilityPredicateWithAvailability(tiers, exempt, false)
}

func modelTierAvailabilityPredicateWithAvailability(tiers, exempt []string, activeOnly bool) string {
	parts := make([]string, 0, len(tiers))
	for _, tier := range tiers {
		switch tier {
		case "free":
			parts = append(parts, modelAccountBuildFreePredicate, modelAccountWebFreePredicate)
		case "super":
			parts = append(parts, modelAccountBuildSuperTierPredicate, modelAccountWebSuperPredicate)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	accountPredicate := ""
	capabilityPredicate := modelRouteAccountCapabilityPredicate
	if activeOnly {
		accountPredicate = " AND account.enabled = TRUE AND account.auth_status = 'active'"
		capabilityPredicate = modelAvailableRouteAccountCapabilityPredicate
	}
	exemptPredicate := ""
	for _, provider := range exempt {
		if account.Provider(provider).IsValid() { // ids are syntax-checked, so quoting is safe
			exemptPredicate += "model_routes.provider = '" + provider + "' OR "
		}
	}
	return `(` + exemptPredicate + `EXISTS (
		SELECT 1 FROM provider_accounts account
		WHERE account.provider = model_routes.provider
		` + accountPredicate + `
			AND (` + strings.Join(parts, " OR ") + `)
			AND ` + capabilityPredicate + `
	))`
}

func (r *ModelRepository) ReplaceAccountCapabilities(ctx context.Context, accountID uint64, upstreamModels []string, syncedAt time.Time) error {
	unique := make(map[string]struct{}, len(upstreamModels))
	rows := make([]accountModelCapabilityModel, 0, len(upstreamModels))
	for _, value := range upstreamModels {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := unique[value]; ok {
			continue
		}
		unique[value] = struct{}{}
		rows = append(rows, accountModelCapabilityModel{AccountID: accountID, UpstreamModel: value})
	}
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("account_id = ?", accountID).Delete(&accountModelCapabilityModel{}).Error; err != nil {
			return err
		}
		if len(rows) > 0 {
			if err := tx.CreateInBatches(rows, 200).Error; err != nil {
				return err
			}
		}
		state := accountModelSyncStateModel{AccountID: accountID, LastAttemptAt: syncedAt, LastSuccessAt: &syncedAt}
		return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "account_id"}}, DoUpdates: clause.AssignmentColumns([]string{"last_attempt_at", "last_success_at", "last_error"})}).Create(&state).Error
	})
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountCapabilityChanged, AccountID: accountID})
	}
	return err
}

func (r *ModelRepository) MarkAccountCapabilitySyncFailed(ctx context.Context, accountID uint64, attemptedAt time.Time, message string) error {
	state := accountModelSyncStateModel{AccountID: accountID, LastAttemptAt: attemptedAt, LastError: truncate(message, 512)}
	return r.db.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "account_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"last_attempt_at", "last_error"}),
	}).Create(&state).Error
}

func (r *ModelRepository) HasSuccessfulAccountSync(ctx context.Context, accountID uint64) (bool, error) {
	var row struct{ AccountID uint64 }
	err := r.db.db.WithContext(ctx).Model(&accountModelSyncStateModel{}).
		Select("account_id").
		Where("account_id = ? AND last_success_at IS NOT NULL", accountID).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	return row.AccountID > 0, err
}

// ListStaleAccountSyncIDs 返回模型能力快照缺失或过期的启用账号，不扫描已禁用账号。
func (r *ModelRepository) ListStaleAccountSyncIDs(ctx context.Context, before time.Time, limit int) ([]uint64, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var ids []uint64
	err := r.db.db.WithContext(ctx).
		Table("provider_accounts AS account").
		Select("account.id").
		Joins("LEFT JOIN account_model_sync_states AS sync ON sync.account_id = account.id").
		Where("account.enabled = ? AND account.auth_status = ?", true, account.AuthStatusActive).
		Where("sync.last_success_at IS NULL OR sync.last_success_at < ?", before.UTC()).
		Order("sync.last_success_at ASC, account.id ASC").
		Limit(limit).
		Scan(&ids).Error
	return ids, err
}

func (r *ModelRepository) availableRoutes(query *gorm.DB) *gorm.DB {
	return query.Where(availableRoutePredicate, true, account.AuthStatusActive)
}

func (r *ModelRepository) annotateAvailability(ctx context.Context, values []model.Route) error {
	if len(values) == 0 {
		return nil
	}
	ids := make([]uint64, 0, len(values))
	for _, value := range values {
		ids = append(ids, value.ID)
	}
	type availabilityRow struct {
		RouteID           uint64
		SupportedAccounts int
		SyncedAccounts    int
		TotalAccounts     int
		LastSyncedUnix    sql.NullInt64
	}
	var rows []availabilityRow
	lastSyncedExpression := "MAX(unixepoch(sync.last_success_at))"
	if r.db.dialect == "postgres" {
		lastSyncedExpression = "CAST(MAX(EXTRACT(EPOCH FROM sync.last_success_at)) AS BIGINT)"
	}
	err := r.db.db.WithContext(ctx).Raw(fmt.Sprintf(`
		SELECT route.id AS route_id,
			CASE WHEN COUNT(DISTINCT binding.account_id) > 0
				THEN COUNT(DISTINCT CASE WHEN account.enabled = TRUE AND account.auth_status = ? AND binding.account_id IS NOT NULL THEN account.id END)
				ELSE COUNT(DISTINCT CASE WHEN account.enabled = TRUE AND account.auth_status = ? AND (`+modelConsoleStaticSupportAvailabilityExpression+` OR `+modelWebBasicMediaStaticSupportAvailabilityExpression+` OR capability.account_id IS NOT NULL OR `+modelSharedPaidBuildSupportAvailabilityExpression+`) THEN account.id END)
			END AS supported_accounts,
			CASE WHEN COUNT(DISTINCT binding.account_id) > 0
				THEN COUNT(DISTINCT CASE WHEN account.enabled = TRUE AND account.auth_status = ? AND binding.account_id IS NOT NULL AND sync.last_success_at IS NOT NULL THEN account.id END)
				ELSE COUNT(DISTINCT CASE WHEN account.enabled = TRUE AND account.auth_status = ? AND sync.last_success_at IS NOT NULL THEN account.id END)
			END AS synced_accounts,
			CASE WHEN COUNT(DISTINCT binding.account_id) > 0
				THEN COUNT(DISTINCT binding.account_id)
				ELSE COUNT(DISTINCT CASE WHEN account.enabled = TRUE AND account.auth_status = ? THEN account.id END)
			END AS total_accounts,
			%s AS last_synced_unix
		FROM model_routes route
		LEFT JOIN provider_accounts account ON account.provider = route.provider
		LEFT JOIN model_route_accounts binding ON binding.model_route_id = route.id AND binding.account_id = account.id
		LEFT JOIN account_model_sync_states sync ON sync.account_id = account.id
		LEFT JOIN account_model_capabilities capability ON capability.account_id = account.id AND capability.upstream_model = route.upstream_model
		WHERE route.id IN ?
		GROUP BY route.id
	`, lastSyncedExpression), account.AuthStatusActive, account.AuthStatusActive, account.AuthStatusActive, account.AuthStatusActive, account.AuthStatusActive, ids).Scan(&rows).Error
	if err != nil {
		return err
	}
	var bindings []modelRouteAccountModel
	if err := r.db.db.WithContext(ctx).Where("model_route_id IN ?", ids).Order("model_route_id ASC, account_id ASC").Find(&bindings).Error; err != nil {
		return err
	}
	boundByRoute := make(map[uint64][]uint64, len(values))
	for _, binding := range bindings {
		boundByRoute[binding.ModelRouteID] = append(boundByRoute[binding.ModelRouteID], binding.AccountID)
	}
	byID := make(map[uint64]availabilityRow, len(rows))
	for _, row := range rows {
		byID[row.RouteID] = row
	}
	for index := range values {
		row := byID[values[index].ID]
		values[index].SupportedAccounts = row.SupportedAccounts
		values[index].SyncedAccounts = row.SyncedAccounts
		values[index].TotalAccounts = row.TotalAccounts
		values[index].BoundAccountIDs = boundByRoute[values[index].ID]
		if row.LastSyncedUnix.Valid {
			lastSyncedAt := time.Unix(row.LastSyncedUnix.Int64, 0).UTC()
			values[index].LastSyncedAt = &lastSyncedAt
		}
	}
	return nil
}
