package relational

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

const postgresSchemaMigrationLockID int64 = 0x47524f4b32415049

var schemaModels = []any{
	&egressExtensionFallbackModel{},
	&adminModel{},
	&adminSessionModel{},
	&egressSubscriptionSourceModel{},
	&egressProxyProfileModel{},
	&egressNodeModel{},
	&egressOperationsConfigModel{},
	&accountModel{},
	&accountCredentialModel{},
	&accountProviderLinkModel{},
	&webConsoleAccountLinkModel{},
	&webAccountProfileModel{},
	&quotaWindowModel{},
	&billingModel{},
	&quotaRecoveryModel{},
	&modelRouteModel{},
	&modelRouteAliasModel{},
	&modelRouteAccountModel{},
	&accountModelCapabilityModel{},
	&accountModelSyncStateModel{},
	&accountModelQuotaBlockModel{},
	&accountEgressLeaseBlockModel{},
	&clientKeyModel{},
	&clientKeyModelPermission{},
	&clientKeyProviderScopeModel{},
	&billingReservationModel{},
	&modelPriceModel{},
	&requestAuditModel{},
	&requestAuditAttemptModel{},
	&responseOwnershipModel{},
	&webResponseStateModel{},
	&mediaJobModel{},
	&mediaAssetModel{},
	&mediaUploadTicketModel{},
	&runtimeSettingsModel{},
}

var schemaIndexes = []string{
	"CREATE INDEX IF NOT EXISTS idx_admin_sessions_admin_created ON admin_sessions(admin_id, created_at DESC, id DESC)",
	"CREATE INDEX IF NOT EXISTS idx_admin_sessions_expires ON admin_sessions(expires_at)",
	// SQLite 通过重建表修改 CHECK 约束，重建会删除独立存储的 GORM 唯一索引；
	// 在统一索引阶段显式恢复这些数据完整性约束。
	"CREATE UNIQUE INDEX IF NOT EXISTS idx_provider_accounts_identity_key ON provider_accounts(identity_key)",
	"CREATE INDEX IF NOT EXISTS idx_accounts_routing ON provider_accounts(provider, enabled, auth_status, priority DESC, id ASC)",
	"CREATE INDEX IF NOT EXISTS idx_accounts_created_id ON provider_accounts(created_at DESC, id DESC)",
	"CREATE INDEX IF NOT EXISTS idx_accounts_auto_clean_reauth ON provider_accounts(auth_status, reauth_marked_at, id)",
	"CREATE INDEX IF NOT EXISTS idx_accounts_auto_clean_reauth_cursor ON provider_accounts(auth_status, enabled, id, reauth_marked_at)",
	"CREATE INDEX IF NOT EXISTS idx_account_credentials_refresh_due ON account_credentials(refresh_due_at, account_id)",
	"CREATE INDEX IF NOT EXISTS idx_account_credentials_build_bot_flag ON account_credentials(build_bot_flag_source, account_id)",
	"CREATE INDEX IF NOT EXISTS idx_account_egress_lease_blocks_due ON account_egress_lease_blocks(cooldown_until, account_id, node_id)",
	"CREATE INDEX IF NOT EXISTS idx_quota_windows_due ON account_quota_windows(remaining, reset_at, account_id)",
	"CREATE INDEX IF NOT EXISTS idx_model_routes_public_id_lookup ON model_routes(public_id)",
	// Catalog/discovered rows remain idempotent per API capability. One public
	// image model may intentionally serve both generation and editing, while
	// manual rows may still share a public ID and form a route-target pool.
	"CREATE UNIQUE INDEX IF NOT EXISTS uidx_model_routes_managed_public_capability ON model_routes(public_id, capability) WHERE origin IN ('catalog', 'discovered')",
	"CREATE INDEX IF NOT EXISTS idx_model_routes_provider_upstream ON model_routes(provider, upstream_model)",
	"CREATE INDEX IF NOT EXISTS idx_model_routes_grouping ON model_routes(provider, public_id, upstream_model, origin, id)",
	"CREATE INDEX IF NOT EXISTS idx_model_routes_created_id ON model_routes(created_at DESC, id DESC)",
	"CREATE INDEX IF NOT EXISTS idx_model_routes_enabled ON model_routes(enabled, public_id, id)",
	// One active price per key; superseded rows keep the history.
	"CREATE UNIQUE INDEX IF NOT EXISTS uidx_model_prices_active ON model_prices(provider, pricing_model, operation) WHERE superseded_at IS NULL",
	"CREATE INDEX IF NOT EXISTS idx_model_route_aliases_route ON model_route_aliases(model_route_id, alias)",
	"CREATE INDEX IF NOT EXISTS idx_model_route_accounts_account_route ON model_route_accounts(account_id, model_route_id)",
	// Routing overlays, route availability and the dashboard look capabilities up by model.
	"CREATE INDEX IF NOT EXISTS idx_account_model_capabilities_model ON account_model_capabilities(upstream_model, account_id)",
	"CREATE INDEX IF NOT EXISTS idx_account_model_quota_blocks_due ON account_model_quota_blocks(cooldown_until, account_id)",
	"CREATE UNIQUE INDEX IF NOT EXISTS idx_client_keys_prefix ON client_keys(prefix)",
	"CREATE INDEX IF NOT EXISTS idx_client_keys_created_id ON client_keys(created_at DESC, id DESC)",
	"CREATE INDEX IF NOT EXISTS idx_client_keys_status ON client_keys(enabled, expires_at, created_at DESC, id DESC)",
	"CREATE UNIQUE INDEX IF NOT EXISTS idx_client_keys_internal_kind ON client_keys(internal_kind) WHERE internal_kind IS NOT NULL",
	"CREATE INDEX IF NOT EXISTS idx_client_key_models_route_key ON client_key_models(model_route_id, client_key_id)",
	"CREATE INDEX IF NOT EXISTS idx_billing_reservations_expiry ON billing_reservations(expires_at, client_key_id)",
	"CREATE INDEX IF NOT EXISTS idx_egress_nodes_scope_health ON egress_nodes(scope, enabled, health DESC, id ASC)",
	"CREATE INDEX IF NOT EXISTS idx_egress_nodes_probe_due ON egress_nodes(enabled, last_probed_at, id)",
	"CREATE INDEX IF NOT EXISTS idx_egress_nodes_proxy_profile ON egress_nodes(proxy_profile_id, id)",
	"CREATE INDEX IF NOT EXISTS idx_egress_sources_scope_name ON egress_subscription_sources(scope, LOWER(name), id)",
	"CREATE INDEX IF NOT EXISTS idx_audits_created_id ON request_audits(created_at DESC, id DESC)",
	"CREATE UNIQUE INDEX IF NOT EXISTS idx_audits_event_id ON request_audits(event_id) WHERE event_id <> ''",
	"CREATE INDEX IF NOT EXISTS idx_audits_account_created_id ON request_audits(account_id, created_at DESC, id DESC)",
	"CREATE INDEX IF NOT EXISTS idx_audits_status_created_id ON request_audits(status_code, created_at DESC, id DESC)",
	"CREATE INDEX IF NOT EXISTS idx_audits_streaming_created_id ON request_audits(streaming, created_at DESC, id DESC)",
	"CREATE INDEX IF NOT EXISTS idx_audits_provider_streaming_created_id ON request_audits(provider, streaming, created_at DESC, id DESC)",
	"CREATE INDEX IF NOT EXISTS idx_audit_attempts_audit_number ON request_audit_attempts(audit_id, number)",
	"CREATE INDEX IF NOT EXISTS idx_response_ownership_expires_id ON response_ownership(expires_at, response_id)",
	"CREATE INDEX IF NOT EXISTS idx_response_ownership_account ON response_ownership(account_id)",
	"CREATE INDEX IF NOT EXISTS idx_response_ownership_client_key ON response_ownership(client_key_id)",
	"CREATE INDEX IF NOT EXISTS idx_response_ownership_route ON response_ownership(model_route_id)",
	"CREATE INDEX IF NOT EXISTS idx_web_response_states_expires_id ON web_response_states(expires_at, response_id)",
	"CREATE INDEX IF NOT EXISTS idx_web_response_states_account ON web_response_states(account_id, created_at DESC)",
	"CREATE INDEX IF NOT EXISTS idx_media_jobs_client_created ON media_jobs(client_key_id, created_at DESC)",
	"CREATE INDEX IF NOT EXISTS idx_media_jobs_account_status ON media_jobs(account_id, status)",
	"CREATE INDEX IF NOT EXISTS idx_media_jobs_recovery ON media_jobs(status, lease_until, created_at, id)",
	"CREATE INDEX IF NOT EXISTS idx_media_jobs_usage_recovery ON media_jobs(status, usage_recorded_at, completed_at, id)",
	// Billing cleanup checks every expired reservation against the jobs whose
	// usage is still pending. usage_recorded_at makes the index covering, which
	// SQLite needs to prefer it over idx_media_jobs_kind_created without ANALYZE.
	"CREATE INDEX IF NOT EXISTS idx_media_jobs_usage_pending ON media_jobs(kind, id, usage_recorded_at) WHERE usage_recorded_at IS NULL",
	"CREATE INDEX IF NOT EXISTS idx_media_assets_created ON media_assets(created_at DESC, id)",
	"CREATE INDEX IF NOT EXISTS idx_media_assets_kind_created ON media_assets(kind, created_at DESC, id)",
	"CREATE INDEX IF NOT EXISTS idx_media_assets_expires ON media_assets(expires_at, id) WHERE expires_at IS NOT NULL",
	"CREATE INDEX IF NOT EXISTS idx_media_upload_tickets_expires ON media_upload_tickets(expires_at, consumed_at)",
	"CREATE INDEX IF NOT EXISTS idx_media_jobs_result_asset ON media_jobs(result_asset_id) WHERE result_asset_id <> ''",
	"CREATE INDEX IF NOT EXISTS idx_media_jobs_kind_created ON media_jobs(kind, created_at DESC, id DESC)",
	// Unique per client key only when the client sent an Idempotency-Key.
	"CREATE UNIQUE INDEX IF NOT EXISTS uidx_media_jobs_idempotency ON media_jobs(client_key_id, idempotency_key) WHERE idempotency_key <> ''",
	// Gallery provenance filters.
	"CREATE INDEX IF NOT EXISTS idx_media_assets_provider_created ON media_assets(provider, created_at DESC, id)",
	"CREATE INDEX IF NOT EXISTS idx_media_assets_client_key_created ON media_assets(client_key_id, created_at DESC, id)",
	"CREATE INDEX IF NOT EXISTS idx_media_assets_account_created ON media_assets(account_id, created_at DESC, id)",
}

// InitializeSchema 以当前持久化模型作为首版数据库结构基线。
func (d *Database) InitializeSchema(ctx context.Context) error {
	if d.dialect == "postgres" {
		return d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", postgresSchemaMigrationLockID).Error; err != nil {
				return fmt.Errorf("acquire PostgreSQL migration lock: %w", err)
			}
			locked := &Database{db: tx, dialect: d.dialect}
			return locked.initializeSchema(ctx)
		})
	}
	return d.initializeSchema(ctx)
}

func (d *Database) initializeSchema(ctx context.Context) error {
	db := d.db.WithContext(ctx)
	autoMigrate := func() error {
		return d.db.WithContext(ctx).AutoMigrate(schemaModels...)
	}
	var migrateErr error
	if d.dialect == "sqlite" {
		// SQLite 修改 CHECK 等表级约束时会重建表。provider_accounts 等父表已被多个
		// 子表引用，必须在固定连接上暂停外键，否则 DROP 旧父表会直接失败。
		migrateErr = d.withSQLiteForeignKeysDisabled(ctx, autoMigrate)
	} else {
		migrateErr = autoMigrate()
	}
	if migrateErr != nil {
		return fmt.Errorf("初始化数据库表: %w", migrateErr)
	}
	for _, statement := range schemaIndexes {
		if err := db.Exec(statement).Error; err != nil {
			return fmt.Errorf("初始化数据库索引: %w", err)
		}
	}
	return nil
}

// withSQLiteForeignKeysDisabled 将会重建父表的约束迁移固定到唯一连接。
// SQLite 的 DROP TABLE 即使只用于改 CHECK，也会执行 ON DELETE CASCADE；因此必须
// 在同一物理连接上临时关闭外键，迁移后再完整校验并恢复。
func (d *Database) withSQLiteForeignKeysDisabled(ctx context.Context, migrate func() error) error {
	sqlDB, err := d.db.DB()
	if err != nil {
		return err
	}
	// OpenSQLite 的正常池大小为 16。收敛为一个连接，确保 PRAGMA 与 GORM 的表重建
	// 使用同一 SQLite 会话；初始化阶段尚未启动业务协程，不会影响请求处理。
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	defer func() {
		sqlDB.SetMaxOpenConns(16)
		sqlDB.SetMaxIdleConns(16)
	}()
	db := d.db.WithContext(ctx)
	if err := db.Exec("PRAGMA foreign_keys = OFF").Error; err != nil {
		return fmt.Errorf("暂停 SQLite 外键约束: %w", err)
	}
	foreignKeysDisabled := true
	defer func() {
		if foreignKeysDisabled {
			_ = db.Exec("PRAGMA foreign_keys = ON").Error
		}
	}()
	var foreignKeys int
	if err := db.Raw("PRAGMA foreign_keys").Scan(&foreignKeys).Error; err != nil {
		return fmt.Errorf("确认 SQLite 外键状态: %w", err)
	}
	if foreignKeys != 0 {
		return fmt.Errorf("暂停 SQLite 外键约束失败")
	}
	migrationErr := migrate()
	if migrationErr == nil {
		var violations []struct {
			Table  string
			RowID  *int64
			Parent string
			FKID   int
		}
		if err := db.Raw("PRAGMA foreign_key_check").Scan(&violations).Error; err != nil {
			migrationErr = fmt.Errorf("校验 SQLite 外键: %w", err)
		} else if len(violations) > 0 {
			migrationErr = fmt.Errorf("SQLite 约束迁移产生 %d 条外键违规", len(violations))
		}
	}
	enableErr := db.Exec("PRAGMA foreign_keys = ON").Error
	if enableErr == nil {
		foreignKeysDisabled = false
	}
	if migrationErr != nil {
		if enableErr != nil {
			return fmt.Errorf("%w；恢复 SQLite 外键失败: %v", migrationErr, enableErr)
		}
		return migrationErr
	}
	if enableErr != nil {
		return fmt.Errorf("恢复 SQLite 外键约束: %w", enableErr)
	}
	return nil
}
