package relational

import (
	"context"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"gorm.io/gorm"
)

// Remove only the historical business enums. Identity, FK and value constraints
// remain intact. The old migration paths must never recreate these checks.
func (d *Database) dropProviderEnumConstraints(ctx context.Context) error {
	constraints := []consoleConstraint{
		{&accountModel{}, "provider_accounts", "chk_accounts_provider"},
		{&modelRouteModel{}, "model_routes", "chk_model_routes_provider"},
		{&requestAuditModel{}, "request_audits", "chk_request_audits_provider"},
		{&requestAuditModel{}, "request_audits", "chk_request_audits_egress_scope"},
		{&responseOwnershipModel{}, "response_ownership", "chk_response_ownership_provider"},
		{&egressNodeModel{}, "egress_nodes", "chk_egress_nodes_specific_scope"},
		{&egressSubscriptionSourceModel{}, "egress_subscription_sources", "chk_egress_subscription_sources_scope"},
		{&mediaJobModel{}, "media_jobs", "chk_media_jobs_provider"},
		{&mediaJobModel{}, "media_jobs", "chk_media_jobs_egress_scope"},
		{&accountCredentialModel{}, "account_credentials", "chk_account_credentials_auth_type"},
	}
	apply := func() error {
		for _, c := range constraints {
			if d.db.WithContext(ctx).Migrator().HasConstraint(c.model, c.name) {
				if err := d.db.WithContext(ctx).Migrator().DropConstraint(c.model, c.name); err != nil {
					return err
				}
			}
		}

		c := consoleConstraint{&accountCredentialModel{}, "account_credentials", "chk_account_credentials_secret"}
		definition, err := d.constraintDefinition(ctx, c)
		if err != nil {
			return err
		}
		if strings.Contains(definition, "auth_type") {
			if err := d.db.WithContext(ctx).Migrator().DropConstraint(c.model, c.name); err != nil {
				return err
			}
			return d.db.WithContext(ctx).Migrator().CreateConstraint(c.model, c.name)
		}
		return nil
	}
	if d.dialect == "sqlite" {
		if err := d.withSQLiteForeignKeysDisabled(ctx, apply); err != nil {
			return err
		}
	} else if err := apply(); err != nil {
		return err
	}
	return nil
}

// retiredChannelTables belonged to the removed extension-channel subsystem,
// listed children first.
var retiredChannelTables = []string{
	"channel_asset_refs", "channel_image_items", "channel_quota_reservations", "channel_execution_slots", "channel_image_jobs",
	"channel_accounts", "channel_oauth_sessions", "channel_client_key_grants", "channel_prices", "channel_settings",
}

// dropChannelTables retires the extension-channel subsystem. Its data is not
// migrated: the tables are dropped together with the ChatGPT accounts
// (auth_type channel_v1) and manual routes the channel created. It acts only
// while a channel table still exists, so later native manual routes are kept.
func (d *Database) dropChannelTables(ctx context.Context) error {
	db := d.db.WithContext(ctx)
	present := false
	for _, table := range retiredChannelTables {
		present = present || db.Migrator().HasTable(table)
	}
	if !present {
		return nil
	}
	return db.Transaction(func(tx *gorm.DB) error {
		for _, table := range retiredChannelTables {
			if err := tx.Migrator().DropTable(table); err != nil {
				return err
			}
		}
		if err := tx.Where("provider = ? AND origin = ?", account.ProviderChatGPT, model.OriginManual).Delete(&modelRouteModel{}).Error; err != nil {
			return err
		}
		legacy := tx.Model(&accountCredentialModel{}).Select("account_id").Where("auth_type = ?", "channel_v1")
		return tx.Where("provider = ? AND id IN (?)", account.ProviderChatGPT, legacy).Delete(&accountModel{}).Error
	})
}

// dropChatGPTImageChatRoutes removes the Responses routes that model discovery
// created for ChatGPT image models before every gpt-image-* and
// codex-gpt-image-* ID was treated as an image route. Referencing tables
// cascade; audits keep their route IDs.
func (d *Database) dropChatGPTImageChatRoutes(ctx context.Context) error {
	return d.db.WithContext(ctx).
		Where("provider = ? AND origin = ? AND capability = ? AND (upstream_model LIKE ? OR upstream_model LIKE ?)", account.ProviderChatGPT, model.OriginDiscovered, model.CapabilityResponses, "gpt-image-%", "codex-gpt-image-%").
		Delete(&modelRouteModel{}).Error
}

// rescheduleChatGPTCredentialRefresh moves ChatGPT renewals that an earlier
// release scheduled minutes before expiry to the provider's lead. It only
// ever moves a renewal earlier, so running it on every start is a no-op once
// applied; failed renewals keep their retry schedule.
func (d *Database) rescheduleChatGPTCredentialRefresh(ctx context.Context) error {
	var rows []struct {
		AccountID    uint64
		ExpiresAt    time.Time
		RefreshDueAt time.Time
	}
	err := d.db.WithContext(ctx).Table("account_credentials AS credential").
		Select("credential.account_id, credential.expires_at, credential.refresh_due_at").
		Joins("JOIN provider_accounts AS account ON account.id = credential.account_id").
		Where("account.provider = ? AND credential.refresh_failures = 0 AND credential.expires_at IS NOT NULL AND credential.refresh_due_at IS NOT NULL", account.ProviderChatGPT).
		Scan(&rows).Error
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, row := range rows {
		due := account.CredentialRefreshDueAt(account.ProviderChatGPT, row.AccountID, row.ExpiresAt, now)
		if !due.Before(row.RefreshDueAt) {
			continue
		}
		if err := d.db.WithContext(ctx).Model(&accountCredentialModel{}).Where("account_id = ?", row.AccountID).Update("refresh_due_at", due).Error; err != nil {
			return err
		}
	}
	return nil
}

// resyncChatGPTQuotaWithoutResetTimes marks ChatGPT accounts unsynced when an
// exhausted window has no reset time, which only windows stored before
// reset_after was read lack (an available window may legitimately have none).
// The quota catch-up then resyncs them first; the exhausted window gets a
// reset time and with it a recovery event.
func (d *Database) resyncChatGPTQuotaWithoutResetTimes(ctx context.Context) error {
	legacy := d.db.Table("account_quota_windows AS quota").Select("quota.account_id").
		Joins("JOIN provider_accounts AS account ON account.id = quota.account_id").
		Where("account.provider = ? AND quota.remaining = 0 AND quota.reset_at IS NULL AND quota.synced_at IS NOT NULL", account.ProviderChatGPT)
	if err := d.db.WithContext(ctx).Exec("UPDATE provider_accounts SET quota_synced_at = NULL WHERE id IN (?)", legacy).Error; err != nil {
		return err
	}
	return d.db.WithContext(ctx).Model(&quotaWindowModel{}).Where("account_id IN (?)", legacy).Update("synced_at", nil).Error
}
