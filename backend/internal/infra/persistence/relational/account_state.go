package relational

import (
	"context"
	"fmt"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MarkWebNSFWEnabled 幂等保存首次成功开启时间；重复执行不会覆盖已有标记。
func (r *AccountRepository) MarkWebNSFWEnabled(ctx context.Context, id uint64, enabledAt time.Time) error {
	if id == 0 || enabledAt.IsZero() {
		return fmt.Errorf("Web NSFW 标记参数无效")
	}
	err := r.markWebProfileTimestamp(ctx, id, "nsfw_enabled_at", enabledAt)
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: account.ProviderWeb, AccountID: id})
	}
	return err
}

// MarkWebTermsAccepted 幂等保存已完整接受的产品协议版本。
// 协议升级时会同步更新完成时间；相同或更高版本不会被覆盖。
func (r *AccountRepository) MarkWebTermsAccepted(ctx context.Context, id uint64, version int, acceptedAt time.Time) error {
	if id == 0 || version <= 0 || acceptedAt.IsZero() {
		return fmt.Errorf("Web 服务协议标记参数无效")
	}
	acceptedAt = acceptedAt.UTC()
	err := mapError(r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var accountRow accountModel
		if err := tx.Select("id", "provider").First(&accountRow, id).Error; err != nil {
			return err
		}
		if account.Provider(accountRow.Provider) != account.ProviderWeb {
			return fmt.Errorf("仅 Grok Web 账号支持资料状态标记")
		}
		profile := webAccountProfileModel{
			AccountID: id, Tier: string(account.WebTierAuto),
			TermsAcceptedAt: &acceptedAt, TermsAcceptedVersion: version,
		}
		created := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&profile)
		if created.Error != nil || created.RowsAffected > 0 {
			return created.Error
		}
		return tx.Model(&webAccountProfileModel{}).
			Where("account_id = ? AND (terms_accepted_version < ? OR terms_accepted_at IS NULL)", id, version).
			Updates(map[string]any{"terms_accepted_at": acceptedAt, "terms_accepted_version": version}).Error
	}))
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: account.ProviderWeb, AccountID: id})
	}
	return err
}

// MarkWebBirthDateSet 幂等保存首次成功设置或确认已有生日的时间。
func (r *AccountRepository) MarkWebBirthDateSet(ctx context.Context, id uint64, setAt time.Time) error {
	if id == 0 || setAt.IsZero() {
		return fmt.Errorf("Web 生日标记参数无效")
	}
	err := r.markWebProfileTimestamp(ctx, id, "birth_date_set_at", setAt)
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: account.ProviderWeb, AccountID: id})
	}
	return err
}

func (r *AccountRepository) markWebProfileTimestamp(ctx context.Context, id uint64, column string, value time.Time) error {
	value = value.UTC()
	return mapError(r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var accountRow accountModel
		if err := tx.Select("id", "provider").First(&accountRow, id).Error; err != nil {
			return err
		}
		if account.Provider(accountRow.Provider) != account.ProviderWeb {
			return fmt.Errorf("仅 Grok Web 账号支持资料状态标记")
		}
		profile := webAccountProfileModel{AccountID: id, Tier: string(account.WebTierAuto)}
		switch column {
		case "nsfw_enabled_at":
			profile.NSFWEnabledAt = &value
		case "birth_date_set_at":
			profile.BirthDateSetAt = &value
		default:
			return fmt.Errorf("Web 资料状态字段无效")
		}
		created := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&profile)
		if created.Error != nil || created.RowsAffected > 0 {
			return created.Error
		}
		switch column {
		case "nsfw_enabled_at":
			return tx.Model(&webAccountProfileModel{}).
				Where("account_id = ? AND nsfw_enabled_at IS NULL", id).
				Update("nsfw_enabled_at", value).Error
		case "birth_date_set_at":
			return tx.Model(&webAccountProfileModel{}).
				Where("account_id = ? AND birth_date_set_at IS NULL", id).
				Update("birth_date_set_at", value).Error
		default:
			return fmt.Errorf("Web 资料状态字段无效")
		}
	}))
}

func (r *AccountRepository) UpdateObservedModel(ctx context.Context, id uint64, model string, observedAt time.Time) error {
	_, err := r.UpdateObservedModelIfNewer(ctx, id, model, observedAt)
	return err
}

func (r *AccountRepository) UpdateObservedModelIfNewer(ctx context.Context, id uint64, model string, observedAt time.Time) (bool, error) {
	model = truncate(model, 255)
	result := r.db.db.WithContext(ctx).Model(&accountModel{}).
		Where("id = ? AND (observed_model_at IS NULL OR observed_model_at <= ?) AND (COALESCE(observed_model, '') <> ? OR observed_model_at <= ?)", id, observedAt, model, observedAt.Add(-30*time.Minute)).
		Updates(map[string]any{"observed_model": model, "observed_model_at": observedAt})
	if result.Error == nil && result.RowsAffected > 0 {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, AccountID: id})
	}
	return result.RowsAffected > 0, result.Error
}

// MarkBuildAPIFallback idempotently updates the XAI inference fallback marker for Grok Build accounts.
func (r *AccountRepository) MarkBuildAPIFallback(ctx context.Context, id uint64, enabled bool) error {
	result := r.db.db.WithContext(ctx).Model(&accountModel{}).
		Where("id = ? AND provider = ?", id, account.ProviderBuild).
		Update("build_api_fallback", enabled)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		var count int64
		if err := r.db.db.WithContext(ctx).Model(&accountModel{}).Where("id = ?", id).Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			return repository.ErrNotFound
		}
		return fmt.Errorf("仅 grok_build 账号支持 Build API 降级标记")
	}
	r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: account.ProviderBuild, AccountID: id})
	return nil
}

func (r *AccountRepository) UpdateHealth(ctx context.Context, id uint64, provider account.Provider, failureCount int, cooldownUntil *time.Time, lastError string, success bool) error {
	if id == 0 || !provider.IsValid() {
		return repository.ErrNotFound
	}
	failureCount = max(0, failureCount)
	lastError = truncate(lastError, 512)
	updates := map[string]any{"failure_count": failureCount, "cooldown_until": cooldownUntil, "last_error": lastError}
	if success {
		now := time.Now().UTC()
		updates["last_used_at"] = &now
	}
	result := r.db.db.WithContext(ctx).Model(&accountModel{}).Where("id = ? AND provider = ?", id, provider).Updates(updates)
	if result.Error != nil {
		return mapError(result.Error)
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	r.notifyInvalidation(ctx, repository.InvalidationEvent{
		Kind: repository.InvalidationAccountHealthChanged, Provider: provider, AccountID: id,
		FailureCount: failureCount, CooldownUntil: cooldownUntil, HealthMarker: account.NormalizeHealthMarker(lastError),
	})
	return nil
}

func (r *AccountRepository) TouchLastUsed(ctx context.Context, id uint64, usedAt time.Time) error {
	if id == 0 || usedAt.IsZero() {
		return repository.ErrNotFound
	}
	// A single UPDATE on the request path needs no wrapping transaction.
	result := r.db.db.WithContext(ctx).Session(&gorm.Session{SkipDefaultTransaction: true}).
		Model(&accountModel{}).Where("id = ?", id).Update("last_used_at", usedAt.UTC())
	if result.Error != nil {
		return mapError(result.Error)
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}
