package relational

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (r *AccountRepository) UpsertByIdentity(ctx context.Context, value account.Credential) (account.Credential, bool, error) {
	var result repository.AccountUpsertResult
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		result, err = upsertAccountByIdentity(tx, value)
		return err
	})
	if err != nil {
		return account.Credential{}, false, mapError(err)
	}
	r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: value.Provider, AccountID: result.ID})
	stored, err := r.Get(ctx, result.ID)
	return stored, result.Created, err
}

func (r *AccountRepository) UpsertManyByIdentity(ctx context.Context, values []account.Credential) ([]repository.AccountUpsertResult, error) {
	if len(values) == 0 {
		return []repository.AccountUpsertResult{}, nil
	}
	results := make([]repository.AccountUpsertResult, len(values))
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		identityKeys := make([]string, 0, len(values))
		sourceKeysByProvider := make(map[account.Provider][]string)
		for _, value := range values {
			identityKeys = append(identityKeys, fromAccountDomain(value).IdentityKey)
			if strings.TrimSpace(value.SourceKey) != "" {
				sourceKeysByProvider[value.Provider] = append(sourceKeysByProvider[value.Provider], value.SourceKey)
			}
		}
		var existingRows []accountModel
		if err := tx.Where("identity_key IN ?", identityKeys).Find(&existingRows).Error; err != nil {
			return err
		}
		existingByIdentity := make(map[string]accountModel, len(values))
		for _, row := range existingRows {
			existingByIdentity[row.IdentityKey] = row
		}
		existingBySource := make(map[string]accountModel, len(values))
		for providerValue, sourceKeys := range sourceKeysByProvider {
			var sourceRows []accountModel
			if err := tx.Where("provider = ? AND source_key IN ?", providerValue, sourceKeys).Find(&sourceRows).Error; err != nil {
				return err
			}
			for _, row := range sourceRows {
				key := providerSourceLookupKey(row.Provider, row.SourceKey)
				if existing, duplicate := existingBySource[key]; duplicate && existing.ID != row.ID {
					return fmt.Errorf("Provider %s 的来源凭据匹配多个账号", row.Provider)
				}
				existingBySource[key] = row
			}
		}
		for index, value := range values {
			identityKey := fromAccountDomain(value).IdentityKey
			existing, foundByIdentity := existingByIdentity[identityKey]
			bySource, foundBySource := existingBySource[providerSourceLookupKey(string(value.Provider), value.SourceKey)]
			if foundByIdentity && foundBySource && existing.ID != bySource.ID {
				return fmt.Errorf("账号身份与来源凭据指向不同账号")
			}
			if !foundByIdentity && foundBySource {
				existing = bySource
			}
			var current *accountModel
			if foundByIdentity || foundBySource {
				current = &existing
			}
			result, stored, err := upsertKnownAccountByIdentity(tx, value, current)
			if err != nil {
				return err
			}
			results[index] = result
			existingByIdentity[stored.IdentityKey] = stored
			existingBySource[providerSourceLookupKey(stored.Provider, stored.SourceKey)] = stored
		}
		return nil
	})
	if err != nil {
		return nil, mapError(err)
	}
	providers := make(map[account.Provider]struct{})
	for _, value := range values {
		providers[value.Provider] = struct{}{}
	}
	for providerValue := range providers {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: providerValue})
	}
	return results, nil
}

func upsertAccountByIdentity(tx *gorm.DB, value account.Credential) (repository.AccountUpsertResult, error) {
	row := fromAccountDomain(value)
	var byIdentity accountModel
	identityErr := tx.Where("identity_key = ?", row.IdentityKey).First(&byIdentity).Error
	if identityErr != nil && !errors.Is(identityErr, gorm.ErrRecordNotFound) {
		return repository.AccountUpsertResult{}, identityErr
	}
	var sourceRows []accountModel
	if strings.TrimSpace(row.SourceKey) != "" {
		if err := tx.Where("provider = ? AND source_key = ?", row.Provider, row.SourceKey).Limit(2).Find(&sourceRows).Error; err != nil {
			return repository.AccountUpsertResult{}, err
		}
		if len(sourceRows) > 1 {
			return repository.AccountUpsertResult{}, fmt.Errorf("Provider %s 的来源凭据匹配多个账号", row.Provider)
		}
	}
	if identityErr == nil && len(sourceRows) == 1 && byIdentity.ID != sourceRows[0].ID {
		return repository.AccountUpsertResult{}, fmt.Errorf("账号身份与来源凭据指向不同账号")
	}
	if identityErr == nil {
		result, _, err := upsertKnownAccountByIdentity(tx, value, &byIdentity)
		return result, err
	}
	if len(sourceRows) == 1 {
		result, _, err := upsertKnownAccountByIdentity(tx, value, &sourceRows[0])
		return result, err
	}
	result, _, err := upsertKnownAccountByIdentity(tx, value, nil)
	return result, err
}

func providerSourceLookupKey(providerValue, sourceKey string) string {
	return providerValue + "\x00" + sourceKey
}

func upsertKnownAccountByIdentity(tx *gorm.DB, value account.Credential, existing *accountModel) (repository.AccountUpsertResult, accountModel, error) {
	row := fromAccountDomain(value)
	if existing != nil {
		if value.EncryptedCloudflareCookie == "" || value.EncryptedState == "" {
			var storedCredential accountCredentialModel
			if err := tx.Where("account_id = ?", existing.ID).First(&storedCredential).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return repository.AccountUpsertResult{}, accountModel{}, err
			}
			if value.EncryptedCloudflareCookie == "" {
				value.EncryptedCloudflareCookie = storedCredential.EncryptedCloudflareCookie
			}
			// Re-imports without provider state keep the stored state.
			if value.EncryptedState == "" {
				value.EncryptedState = storedCredential.EncryptedState
			}
		}
		if row.Plan == "" {
			row.Plan = existing.Plan
		}
		row.ID = existing.ID
		row.CreatedAt = existing.CreatedAt
		row.Enabled = existing.Enabled
		row.Priority = existing.Priority
		row.MaxConcurrent = existing.MaxConcurrent
		row.MinimumRemaining = existing.MinimumRemaining
		row.FailureCount = existing.FailureCount
		row.CooldownUntil = existing.CooldownUntil
		row.LastError = existing.LastError
		row.LastUsedAt = existing.LastUsedAt
		row.ObservedModel = existing.ObservedModel
		row.ObservedModelAt = existing.ObservedModelAt
		// 账号级 Build 路由、XAI 回退记录与 Super entitlement 在 upsert/转换/刷新路径中保留。
		row.BuildAPIFallback = existing.BuildAPIFallback
		row.BuildRouteMode = existing.BuildRouteMode
		row.BuildSuperEntitled = existing.BuildSuperEntitled
		row.EgressNodeID = existing.EgressNodeID
		row.EgressAssignmentMode = existing.EgressAssignmentMode
		row.EgressAssignedAt = existing.EgressAssignedAt
		// reauth_marked_at 与 Update 路径一致：保持 reauth 时永不被普通 upsert 改写。
		applyReauthMarkedAtTransition(&row, *existing)
		if err := tx.Save(&row).Error; err != nil {
			return repository.AccountUpsertResult{}, accountModel{}, err
		}
		if _, err := deleteInvalidEgressLeaseBlocksForAccount(tx, row); err != nil {
			return repository.AccountUpsertResult{}, accountModel{}, err
		}
		if err := saveAccountRelations(tx, value, row.ID); err != nil {
			return repository.AccountUpsertResult{}, accountModel{}, err
		}
		return repository.AccountUpsertResult{ID: row.ID}, row, nil
	}
	if row.AuthStatus == "" {
		row.AuthStatus = string(account.AuthStatusActive)
	}
	if row.AuthStatus == string(account.AuthStatusReauthRequired) && row.ReauthMarkedAt == nil {
		now := time.Now().UTC()
		row.ReauthMarkedAt = &now
	}
	if row.AuthStatus != string(account.AuthStatusReauthRequired) {
		row.ReauthMarkedAt = nil
	}
	if row.Priority == 0 {
		row.Priority = account.DefaultPriority
	}
	if row.MaxConcurrent == 0 {
		row.MaxConcurrent = account.DefaultMaxConcurrent
	}
	row.Enabled = true
	if err := tx.Create(&row).Error; err != nil {
		return repository.AccountUpsertResult{}, accountModel{}, err
	}
	if err := saveAccountRelations(tx, value, row.ID); err != nil {
		return repository.AccountUpsertResult{}, accountModel{}, err
	}
	return repository.AccountUpsertResult{ID: row.ID, Created: true}, row, nil
}

// Update writes only the settings an administrator edits. Tokens, auth state,
// health, usage, egress and synced provider data have their own writers, and a
// save based on an earlier read must not roll them back.
func (r *AccountRepository) Update(ctx context.Context, value account.Credential) (account.Credential, error) {
	var row accountModel
	if err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing accountModel
		if err := tx.Select("id", "provider", "auth_status", "egress_node_id").First(&existing, value.ID).Error; err != nil {
			return err
		}
		value.Provider = account.Provider(existing.Provider)
		row = fromAccountDomain(value)
		row.ID, row.AuthStatus, row.EgressNodeID = existing.ID, existing.AuthStatus, existing.EgressNodeID
		if err := tx.Model(&row).Select("name", "enabled", "priority", "max_concurrent", "minimum_remaining", "build_super_entitled", "build_route_mode", "updated_at").Updates(&row).Error; err != nil {
			return err
		}
		if err := tx.Model(&accountCredentialModel{}).Where("account_id = ?", row.ID).Update("encrypted_cloudflare_cookie", value.EncryptedCloudflareCookie).Error; err != nil {
			return err
		}
		_, err := deleteInvalidEgressLeaseBlocksForAccount(tx, row)
		return err
	}); err != nil {
		return account.Credential{}, mapError(err)
	}
	r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: account.Provider(row.Provider), AccountID: row.ID})
	return r.Get(ctx, row.ID)
}

// MarkReauthRequired records a rejected credential; the reauth anchor keeps
// the time the account first entered reauthRequired.
func (r *AccountRepository) MarkReauthRequired(ctx context.Context, id uint64, reason string) error {
	var row accountModel
	if err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing accountModel
		if err := tx.Select("id", "provider", "enabled", "auth_status", "reauth_marked_at", "egress_node_id").First(&existing, id).Error; err != nil {
			return err
		}
		row = existing
		row.AuthStatus, row.ReauthMarkedAt, row.LastError = string(account.AuthStatusReauthRequired), nil, truncate(reason, 512)
		applyReauthMarkedAtTransition(&row, existing)
		if err := tx.Model(&row).Select("auth_status", "reauth_marked_at", "last_error", "updated_at").Updates(&row).Error; err != nil {
			return err
		}
		_, err := deleteInvalidEgressLeaseBlocksForAccount(tx, row)
		return err
	}); err != nil {
		return mapError(err)
	}
	r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: account.Provider(row.Provider), AccountID: id})
	return nil
}

// applyReauthMarkedAtTransition 仅在状态切入 reauthRequired 时打锚点；保持 reauth 时保留原锚点；离开 reauth 时清空。
func applyReauthMarkedAtTransition(row *accountModel, existing accountModel) {
	if row.AuthStatus == string(account.AuthStatusReauthRequired) {
		if existing.AuthStatus == string(account.AuthStatusReauthRequired) && existing.ReauthMarkedAt != nil {
			row.ReauthMarkedAt = existing.ReauthMarkedAt
			return
		}
		if row.ReauthMarkedAt == nil {
			now := time.Now().UTC()
			row.ReauthMarkedAt = &now
		}
		return
	}
	row.ReauthMarkedAt = nil
}

func saveAccountRelations(tx *gorm.DB, value account.Credential, accountID uint64) error {
	value.ID = accountID
	credential := fromAccountCredentialDomain(value)
	if err := tx.Save(&credential).Error; err != nil {
		return err
	}
	if profile := fromWebProfileDomain(value); profile != nil {
		updates := []string{"tier", "synced_at"}
		if profile.NSFWEnabledAt != nil {
			updates = append(updates, "nsfw_enabled_at")
		}
		if profile.TermsAcceptedAt != nil {
			updates = append(updates, "terms_accepted_at")
		}
		if profile.TermsAcceptedVersion > 0 {
			updates = append(updates, "terms_accepted_version")
		}
		if profile.BirthDateSetAt != nil {
			updates = append(updates, "birth_date_set_at")
		}
		if strings.TrimSpace(profile.EgressIdentity) != "" {
			updates = append(updates, "egress_identity")
		}
		return tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "account_id"}},
			DoUpdates: clause.AssignmentColumns(updates),
		}).Create(profile).Error
	}
	return tx.Where("account_id = ?", accountID).Delete(&webAccountProfileModel{}).Error
}

func (r *AccountRepository) UpdateMany(ctx context.Context, providerValue account.Provider, ids []uint64, updates repository.AccountUpdates) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	values := make(map[string]any, 4)
	if updates.Enabled != nil {
		values["enabled"] = *updates.Enabled
	}
	if updates.Priority != nil {
		values["priority"] = *updates.Priority
	}
	if updates.MaxConcurrent != nil {
		values["max_concurrent"] = *updates.MaxConcurrent
	}
	if updates.MinimumRemaining != nil {
		values["minimum_remaining"] = *updates.MinimumRemaining
	}
	if len(values) == 0 {
		return 0, nil
	}
	var updated int64
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for start := 0; start < len(ids); start += accountUpdateBatchSize {
			end := min(start+accountUpdateBatchSize, len(ids))
			var rows []accountModel
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id", "provider").Where("id IN ?", ids[start:end]).Order("id ASC").Find(&rows).Error; err != nil {
				return err
			}
			if len(rows) != end-start {
				return repository.ErrAccountPoolMismatch
			}
			for _, row := range rows {
				if account.Provider(row.Provider) != providerValue {
					return repository.ErrAccountPoolMismatch
				}
			}
		}
		for start := 0; start < len(ids); start += accountUpdateBatchSize {
			end := min(start+accountUpdateBatchSize, len(ids))
			result := tx.Model(&accountModel{}).Where("provider = ? AND id IN ?", providerValue, ids[start:end]).Updates(values)
			if result.Error != nil {
				return result.Error
			}
			updated += result.RowsAffected
		}
		if providerValue == account.ProviderBuild && updates.Enabled != nil && !*updates.Enabled {
			if err := tx.Where("account_id IN ?", ids).Delete(&accountEgressLeaseBlockModel{}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if updated > 0 {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged})
	}
	return updated, nil
}
