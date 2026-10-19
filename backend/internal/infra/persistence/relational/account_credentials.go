package relational

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

// CountBuildBotFlagged counts persisted Build risk metadata without loading an
// account-ID slice or credential material.
func (r *AccountRepository) CountBuildBotFlagged(ctx context.Context) (int64, error) {
	var count int64
	err := r.db.db.WithContext(ctx).Model(&accountModel{}).
		Joins("JOIN account_credentials AS credential ON credential.account_id = provider_accounts.id").
		Where("provider_accounts.provider = ? AND credential.build_bot_flag_source IN (1,2)", account.ProviderBuild).
		Count(&count).Error
	return count, err
}

// CountAvailableBuildBotFlagged uses the same availability predicate as
// Summarize without expanding a potentially unbounded ID list.
func (r *AccountRepository) CountAvailableBuildBotFlagged(ctx context.Context, now time.Time) (int64, error) {
	var count int64
	query := r.db.db.WithContext(ctx).Model(&accountModel{}).
		Joins("JOIN account_credentials AS credential ON credential.account_id = provider_accounts.id").
		Where("provider_accounts.provider = ? AND credential.build_bot_flag_source IN (1,2)", account.ProviderBuild)
	query = applyAccountStatusFilter(query, "active", now)
	err := query.Count(&count).Error
	return count, err
}

// ListBuildBotFlaggedAccountIDs reads persisted non-sensitive metadata only; it
// never loads or decrypts access tokens on the scheduling path.
func (r *AccountRepository) ListBuildBotFlaggedAccountIDs(ctx context.Context) ([]uint64, error) {
	var ids []uint64
	err := r.db.db.WithContext(ctx).
		Table("provider_accounts AS account").
		Select("account.id").
		Joins("JOIN account_credentials AS credential ON credential.account_id = account.id").
		Where("account.provider = ? AND credential.build_bot_flag_source IN (1,2)", account.ProviderBuild).
		Order("account.id ASC").
		Scan(&ids).Error
	return ids, err
}

// ListBuildBotFlagCredentialBatch returns the minimum projection required for
// startup backfill of the persisted risk source.
func (r *AccountRepository) ListBuildBotFlagCredentialBatch(ctx context.Context, afterID uint64, limit int) ([]repository.BuildBotFlagCredential, error) {
	if limit < 1 {
		return []repository.BuildBotFlagCredential{}, nil
	}
	var rows []struct {
		AccountID            uint64
		EncryptedAccessToken string
		StoredSource         int
	}
	err := r.db.db.WithContext(ctx).
		Table("provider_accounts AS account").
		Select("account.id AS account_id, credential.encrypted_primary AS encrypted_access_token, credential.build_bot_flag_source AS stored_source").
		Joins("JOIN account_credentials AS credential ON credential.account_id = account.id").
		Where("account.provider = ? AND account.id > ?", account.ProviderBuild, afterID).
		Order("account.id ASC").Limit(limit).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	result := make([]repository.BuildBotFlagCredential, 0, len(rows))
	for _, row := range rows {
		result = append(result, repository.BuildBotFlagCredential{
			AccountID: row.AccountID, EncryptedAccessToken: row.EncryptedAccessToken, StoredSource: row.StoredSource,
		})
	}
	return result, nil
}

// UpdateBuildBotFlagSources persists a bounded backfill batch transactionally.
func (r *AccountRepository) UpdateBuildBotFlagSources(ctx context.Context, values []repository.BuildBotFlagSourceUpdate) error {
	if len(values) == 0 {
		return nil
	}
	changed := false
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, value := range values {
			source := normalizeBuildBotFlagSource(account.ProviderBuild, value.Source)
			result := tx.Model(&accountCredentialModel{}).
				Where("account_id = ? AND encrypted_primary = ?", value.AccountID, value.ExpectedEncryptedAccessToken).
				Update("build_bot_flag_source", source)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected > 0 {
				changed = true
			}
		}
		return nil
	})
	if err == nil && changed {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountCredentialChanged, Provider: account.ProviderBuild})
	}
	return err
}

func (r *AccountRepository) ListEnabledCredentialRefreshAccountIDs(ctx context.Context, provider account.Provider, refreshableOnly bool) ([]uint64, error) {
	query := r.db.db.WithContext(ctx).
		Table("provider_accounts AS account").
		Select("account.id").
		Where("account.provider = ? AND account.enabled = ? AND account.auth_status IN ?", provider, true, []account.AuthStatus{account.AuthStatusActive, account.AuthStatusReauthRequired})
	if refreshableOnly {
		query = query.
			Joins("JOIN account_credentials AS credential ON credential.account_id = account.id").
			Where("credential.encrypted_refresh <> ''")
	}
	var ids []uint64
	err := query.Order("account.id ASC").Scan(&ids).Error
	return ids, err
}

// GetCredentialMaterial hydrates the encrypted provider data for one account
// after routing has selected it. Routing candidate queries intentionally never
// load these encrypted columns.
func (r *AccountRepository) GetCredentialMaterial(ctx context.Context, accountID uint64, provider account.Provider) (account.CredentialMaterial, error) {
	var row accountCredentialModel
	if err := r.db.db.WithContext(ctx).
		Table("account_credentials AS credential").
		Select("credential.*").
		Joins("JOIN provider_accounts AS account ON account.id = credential.account_id").
		Where("credential.account_id = ? AND account.provider = ? AND account.enabled = TRUE AND account.auth_status = ?", accountID, provider, account.AuthStatusActive).
		Take(&row).Error; err != nil {
		return account.CredentialMaterial{}, mapError(err)
	}
	return toCredentialMaterialDomain(row, provider), nil
}

func (r *AccountRepository) UpdateTokens(ctx context.Context, id uint64, accessToken, refreshToken, state string, expiresAt time.Time, buildBotFlagSource int) (account.Credential, error) {
	now := time.Now().UTC()
	if err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var providerRow struct{ Provider string }
		if err := tx.Model(&accountModel{}).Select("provider").Where("id = ?", id).Take(&providerRow).Error; err != nil {
			return err
		}
		refreshDueAt := account.CredentialRefreshDueAt(account.Provider(providerRow.Provider), id, expiresAt, now)
		updates := map[string]any{
			"encrypted_primary": accessToken, "expires_at": expiresAt, "refresh_due_at": refreshDueAt,
			"build_bot_flag_source": normalizeBuildBotFlagSource(account.Provider(providerRow.Provider), buildBotFlagSource),
			"last_refresh_at":       now, "refresh_failures": 0, "refresh_unclassified_auth_failures": 0, "last_refresh_error_status": 0, "last_refresh_error": "", "last_refresh_error_message": "", "last_refresh_error_response": "", "refresh_permanent": false, "updated_at": now,
		}
		if refreshToken != "" {
			updates["encrypted_refresh"] = refreshToken
		}
		if state != "" {
			updates["encrypted_state"] = state
		}
		if err := tx.Model(&accountCredentialModel{}).Where("account_id = ?", id).Updates(updates).Error; err != nil {
			return err
		}
		return tx.Model(&accountModel{}).Where("id = ?", id).Updates(map[string]any{"auth_status": string(account.AuthStatusActive), "last_error": "", "reauth_marked_at": nil}).Error
	}); err != nil {
		return account.Credential{}, err
	}
	stored, err := r.Get(ctx, id)
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountCredentialChanged, Provider: stored.Provider, AccountID: id})
	} else {
		// The database write already committed; retain a broad fallback if the read-back fails.
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountCredentialChanged, AccountID: id})
	}
	return stored, err
}

// BackfillCredentialRefreshSchedules 为升级前凭据分批补齐调度时间，不解密 Token，也不发起 OAuth 请求。
func (r *AccountRepository) BackfillCredentialRefreshSchedules(ctx context.Context, providers []account.Provider, now time.Time, limit int) (int, error) {
	if limit < 1 || len(providers) == 0 {
		return 0, nil
	}
	var rows []struct {
		AccountID        uint64
		Provider         string
		ExpiresAt        *time.Time
		EncryptedPrimary string
	}
	err := r.db.db.WithContext(ctx).
		Table("account_credentials AS credential").
		Select("credential.account_id, account.provider, credential.expires_at, credential.encrypted_primary").
		Joins("JOIN provider_accounts AS account ON account.id = credential.account_id").
		Where("account.provider IN ? AND account.enabled = ? AND account.auth_status = ?", providers, true, account.AuthStatusActive).
		Where("credential.auth_type = ? AND credential.encrypted_refresh <> '' AND credential.refresh_due_at IS NULL", account.AuthTypeOAuth).
		Where("credential.expires_at IS NOT NULL OR credential.encrypted_primary = ''").
		Order("credential.account_id ASC").Limit(limit).Scan(&rows).Error
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	err = r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, row := range rows {
			dueAt := now
			if row.EncryptedPrimary != "" && row.ExpiresAt != nil && !row.ExpiresAt.IsZero() {
				dueAt = account.CredentialRefreshDueAt(account.Provider(row.Provider), row.AccountID, *row.ExpiresAt, now)
			}
			if err := tx.Model(&accountCredentialModel{}).Where("account_id = ? AND refresh_due_at IS NULL", row.AccountID).Update("refresh_due_at", dueAt).Error; err != nil {
				return err
			}
		}
		return nil
	})
	return len(rows), err
}

// ListCriticalCredentialRefreshIDs 只返回重启后必须优先恢复的凭据，避免启动时刷新整个账号池。
func (r *AccountRepository) ListCriticalCredentialRefreshIDs(ctx context.Context, providers []account.Provider, now, expiresBefore time.Time, limit int) ([]uint64, error) {
	if limit < 1 || len(providers) == 0 {
		return []uint64{}, nil
	}
	var ids []uint64
	err := r.db.db.WithContext(ctx).
		Table("account_credentials AS credential").
		Select("credential.account_id").
		Joins("JOIN provider_accounts AS account ON account.id = credential.account_id").
		Where("account.provider IN ? AND account.enabled = ? AND account.auth_status = ?", providers, true, account.AuthStatusActive).
		Where("credential.auth_type = ? AND credential.encrypted_refresh <> ''", account.AuthTypeOAuth).
		Where("credential.encrypted_primary = '' OR credential.expires_at <= ? OR (credential.refresh_failures > 0 AND credential.refresh_due_at IS NOT NULL AND credential.refresh_due_at <= ?)", expiresBefore.UTC(), now.UTC()).
		Order(gorm.Expr("CASE WHEN credential.encrypted_primary = '' THEN 0 WHEN credential.expires_at <= ? THEN 1 ELSE 2 END, credential.expires_at ASC, credential.account_id ASC", now.UTC())).
		Limit(limit).
		Scan(&ids).Error
	return ids, err
}

func (r *AccountRepository) ListDueCredentialRefreshIDs(ctx context.Context, providers []account.Provider, now time.Time, limit int) ([]uint64, error) {
	if limit < 1 || len(providers) == 0 {
		return []uint64{}, nil
	}
	var ids []uint64
	err := r.db.db.WithContext(ctx).
		Table("account_credentials AS credential").
		Select("credential.account_id").
		Joins("JOIN provider_accounts AS account ON account.id = credential.account_id").
		Where("account.provider IN ? AND account.enabled = ? AND account.auth_status = ?", providers, true, account.AuthStatusActive).
		Where("credential.auth_type = ? AND credential.encrypted_refresh <> '' AND credential.refresh_due_at IS NOT NULL AND credential.refresh_due_at <= ?", account.AuthTypeOAuth, now).
		Order("credential.refresh_due_at ASC, credential.account_id ASC").Limit(limit).Scan(&ids).Error
	return ids, err
}

func (r *AccountRepository) NextCredentialRefreshDueAt(ctx context.Context, providers []account.Provider) (*time.Time, error) {
	if len(providers) == 0 {
		return nil, nil
	}
	var rows []struct{ RefreshDueAt time.Time }
	err := r.db.db.WithContext(ctx).
		Table("account_credentials AS credential").
		Select("credential.refresh_due_at").
		Joins("JOIN provider_accounts AS account ON account.id = credential.account_id").
		Where("account.provider IN ? AND account.enabled = ? AND account.auth_status = ?", providers, true, account.AuthStatusActive).
		Where("credential.auth_type = ? AND credential.encrypted_refresh <> '' AND credential.refresh_due_at IS NOT NULL", account.AuthTypeOAuth).
		Order("credential.refresh_due_at ASC, credential.account_id ASC").Limit(1).Scan(&rows).Error
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	value := rows[0].RefreshDueAt.UTC()
	return &value, nil
}

func (r *AccountRepository) UpdateCredentialRefreshFailure(ctx context.Context, id uint64, failure repository.CredentialRefreshFailure) error {
	err := r.db.db.WithContext(ctx).Model(&accountCredentialModel{}).Where("account_id = ?", id).Updates(map[string]any{
		"refresh_due_at": failure.RetryAt.UTC(), "refresh_failures": max(0, failure.Count),
		"refresh_unclassified_auth_failures": max(0, failure.UnclassifiedAuthFailureCount),
		"last_refresh_error_status":          max(0, failure.Status), "last_refresh_error": truncate(failure.Code, 100),
		"last_refresh_error_message": truncate(failure.Message, 512), "last_refresh_error_response": truncate(failure.Response, 4096),
		"refresh_permanent": failure.Permanent, "updated_at": time.Now().UTC(),
	}).Error
	if err == nil && failure.Permanent {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountCredentialChanged, AccountID: id})
	}
	return err
}
