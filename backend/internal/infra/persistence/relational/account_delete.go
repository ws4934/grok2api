package relational

import (
	"context"
	"fmt"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (r *AccountRepository) Delete(ctx context.Context, id uint64) error {
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockAccountLinkMutation(tx); err != nil {
			return err
		}
		var lockedID uint64
		if err := tx.Model(&accountModel{}).Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id).Pluck("id", &lockedID).Error; err != nil {
			return err
		}
		if lockedID == 0 {
			return repository.ErrNotFound
		}
		if err := rejectAccountsWithMediaJobs(tx, []uint64{id}); err != nil {
			return err
		}
		return mapError(tx.Delete(&accountModel{}, id).Error)
	})
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged})
	}
	return err
}

func (r *AccountRepository) DeleteMany(ctx context.Context, ids []uint64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var deleted int64
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockAccountLinkMutation(tx); err != nil {
			return err
		}
		var lockedIDs []uint64
		if err := tx.Model(&accountModel{}).Clauses(clause.Locking{Strength: "UPDATE"}).Where("id IN ?", ids).Pluck("id", &lockedIDs).Error; err != nil {
			return err
		}
		if err := rejectAccountsWithMediaJobs(tx, lockedIDs); err != nil {
			return err
		}
		result := tx.Where("id IN ?", lockedIDs).Delete(&accountModel{})
		deleted = result.RowsAffected
		return result.Error
	})
	if err == nil && deleted > 0 {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged})
	}
	return deleted, err
}

func (r *AccountRepository) ListAutoCleanReauthCandidates(ctx context.Context, markedBefore time.Time, includeDisabled bool, afterID uint64, limit int) ([]uint64, error) {
	if limit < 1 {
		limit = 100
	}
	query := r.db.db.WithContext(ctx).Model(&accountModel{}).
		Select("id").
		Where("auth_status = ? AND reauth_marked_at IS NOT NULL AND reauth_marked_at < ?", account.AuthStatusReauthRequired, markedBefore.UTC()).
		Where(accountUnclassifiedRefreshReauthPredicate).
		Where("NOT EXISTS (SELECT 1 FROM media_jobs job WHERE job.account_id = provider_accounts.id AND job.status IN ?)", []string{string(media.StatusQueued), string(media.StatusInProgress)})
	if afterID > 0 {
		query = query.Where("id > ?", afterID)
	}
	if !includeDisabled {
		query = query.Where("enabled = ?", true)
	}
	var candidates []uint64
	err := query.Order("id ASC").Limit(limit).Pluck("id", &candidates).Error
	return candidates, err
}

func (r *AccountRepository) DeleteAutoCleanReauthCandidates(ctx context.Context, markedBefore time.Time, includeDisabled bool, candidateIDs []uint64) ([]uint64, error) {
	if len(candidateIDs) == 0 {
		return []uint64{}, nil
	}
	deletedIDs := make([]uint64, 0, len(candidateIDs))
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockAccountLinkMutation(tx); err != nil {
			return err
		}
		deletable, err := excludeAccountsWithActiveMediaJobs(tx, candidateIDs)
		if err != nil {
			return err
		}
		if len(deletable) == 0 {
			return nil
		}

		var lockedIDs []uint64
		lockQuery := tx.Model(&accountModel{}).Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id IN ? AND auth_status = ? AND reauth_marked_at IS NOT NULL AND reauth_marked_at < ?", deletable, account.AuthStatusReauthRequired, markedBefore.UTC()).
			Where(accountUnclassifiedRefreshReauthPredicate)
		if !includeDisabled {
			lockQuery = lockQuery.Where("enabled = ?", true)
		}
		if err := lockQuery.Pluck("id", &lockedIDs).Error; err != nil {
			return err
		}
		// lock 后再过滤活动视频任务，避免 list 与 delete 之间的 TOCTOU。
		lockedIDs, err = excludeAccountsWithActiveMediaJobs(tx, lockedIDs)
		if err != nil {
			return err
		}
		if len(lockedIDs) == 0 {
			return nil
		}
		deletion := tx.Where("id IN ? AND auth_status = ? AND reauth_marked_at IS NOT NULL AND reauth_marked_at < ?", lockedIDs, account.AuthStatusReauthRequired, markedBefore.UTC()).
			Where(accountUnclassifiedRefreshReauthPredicate)
		if !includeDisabled {
			deletion = deletion.Where("enabled = ?", true)
		}
		result := deletion.Delete(&accountModel{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == int64(len(lockedIDs)) {
			deletedIDs = append(deletedIDs, lockedIDs...)
			return nil
		}
		var remaining []uint64
		if err := tx.Model(&accountModel{}).Where("id IN ?", lockedIDs).Pluck("id", &remaining).Error; err != nil {
			return err
		}
		remainingSet := make(map[uint64]struct{}, len(remaining))
		for _, id := range remaining {
			remainingSet[id] = struct{}{}
		}
		for _, id := range lockedIDs {
			if _, exists := remainingSet[id]; !exists {
				deletedIDs = append(deletedIDs, id)
			}
		}
		return nil
	})
	if err == nil && len(deletedIDs) > 0 {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged})
	}
	return deletedIDs, err
}

// excludeAccountsWithActiveMediaJobs 返回无 queued/in_progress 视频任务的账号 ID（顺序保持输入顺序）。
func excludeAccountsWithActiveMediaJobs(db *gorm.DB, ids []uint64) ([]uint64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var blocked []uint64
	if err := db.Model(&mediaJobModel{}).
		Distinct("account_id").
		Where("account_id IN ? AND status IN ?", ids, []string{string(media.StatusQueued), string(media.StatusInProgress)}).
		Pluck("account_id", &blocked).Error; err != nil {
		return nil, err
	}
	if len(blocked) == 0 {
		out := make([]uint64, len(ids))
		copy(out, ids)
		return out, nil
	}
	blockedSet := make(map[uint64]struct{}, len(blocked))
	for _, id := range blocked {
		blockedSet[id] = struct{}{}
	}
	out := make([]uint64, 0, len(ids)-len(blocked))
	for _, id := range ids {
		if _, skip := blockedSet[id]; skip {
			continue
		}
		out = append(out, id)
	}
	return out, nil
}

// activeMediaJobStatuses lists video states that still require the account and block deletion.
func activeMediaJobStatuses() []string {
	return []string{string(media.StatusQueued), string(media.StatusInProgress)}
}

// rejectAccountsWithMediaJobs 仅保护仍需账号继续执行的活动视频任务。
// completed/failed 已保存账号名称等快照，删除账号后由外键 SET NULL 保留历史。
func rejectAccountsWithMediaJobs(db *gorm.DB, ids []uint64) error {
	var count int64
	if err := db.Model(&mediaJobModel{}).
		Where("account_id IN ? AND status IN ?", ids, activeMediaJobStatuses()).
		Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("%w: 账号仍关联 %d 条排队中或进行中的视频任务，请等待任务结束后重试", repository.ErrConflict, count)
	}
	return nil
}
