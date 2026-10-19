package account

import (
	"context"
	"strings"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type CleanupStatus string

const (
	CleanupStatusCooldown       CleanupStatus = "cooldown"
	CleanupStatusDisabled       CleanupStatus = "disabled"
	CleanupStatusReauthRequired CleanupStatus = "reauthRequired"
)

// AccountDeleteResult summarizes a single/batch delete with optional linked peers.
type AccountDeleteResult struct {
	Deleted           int64
	RootsDeleted      int64
	LinkedDeleted     int64
	Skipped           int64
	DeletedByProvider map[accountdomain.Provider]int64
}

// accountDeleteResultFromOutcome converts repository results using rows actually deleted.
func accountDeleteResultFromOutcome(providerValue accountdomain.Provider, outcome repository.LinkedDeleteOutcome) AccountDeleteResult {
	out := AccountDeleteResult{
		Deleted:           outcome.Deleted,
		RootsDeleted:      outcome.RootsDeleted,
		LinkedDeleted:     outcome.Deleted - outcome.RootsDeleted,
		Skipped:           int64(len(outcome.SkippedRoots)),
		DeletedByProvider: map[accountdomain.Provider]int64{},
	}
	if providerValue.IsValid() && outcome.RootsDeleted > 0 {
		out.DeletedByProvider[providerValue] = outcome.RootsDeleted
	}
	for provider, count := range outcome.LinkedDeletedByProvider {
		out.DeletedByProvider[provider] += count
	}
	return out
}

// deleteStickyAccounts uses the optional batch capability and falls back for custom stores.
func (s *Service) deleteStickyAccounts(ctx context.Context, accountIDs []uint64) (int, error) {
	if s.sticky == nil || len(accountIDs) == 0 {
		return 0, nil
	}
	if batchDeleter, ok := s.sticky.(repository.StickySessionBatchDeleter); ok {
		if err := batchDeleter.DeleteByAccounts(ctx, accountIDs); err != nil {
			return len(accountIDs), err
		}
		return 0, nil
	}
	failures := 0
	var firstErr error
	for _, id := range accountIDs {
		if err := s.sticky.DeleteByAccount(ctx, id); err != nil {
			failures++
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return failures, firstErr
}

// finishLinkedDelete clears runtime state after the database transaction commits.
func (s *Service) finishLinkedDelete(ctx context.Context, deletedIDs []uint64) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), linkedDeleteRuntimeCleanupLimit)
	defer cancel()
	if failures, err := s.deleteStickyAccounts(cleanupCtx, deletedIDs); err != nil && s.logger != nil {
		s.logger.Warn("linked_account_runtime_cleanup_failed", "accounts", len(deletedIDs), "failures", failures, "error", err)
	}
	for _, id := range deletedIDs {
		s.renewer.forget(id)
	}
}

// BatchDelete atomically removes roots and quota state without expanding linked accounts.
func (s *Service) BatchDelete(ctx context.Context, ids []uint64) (int64, error) {
	result, err := s.batchDeleteWithLinkedMode(ctx, accountdomain.Provider(""), ids, nil, true)
	return result.Deleted, err
}

// BatchDeleteWithLinked deletes root accounts and optional linked peers resolved from binding tables.
// Roots with active video jobs are skipped together with their linked group; other groups are deleted.
func (s *Service) BatchDeleteWithLinked(ctx context.Context, providerValue accountdomain.Provider, ids []uint64, targets []accountdomain.Provider) (AccountDeleteResult, error) {
	return s.batchDeleteWithLinkedMode(ctx, providerValue, ids, targets, true)
}

// batchDeleteWithLinkedMode is the shared atomic path; skipMedia selects reject-all or skip-group behavior.
func (s *Service) batchDeleteWithLinkedMode(ctx context.Context, providerValue accountdomain.Provider, ids []uint64, targets []accountdomain.Provider, skipMedia bool) (AccountDeleteResult, error) {
	var out AccountDeleteResult
	ids, err := normalizeBatchIDs(ids)
	if err != nil {
		return out, err
	}
	if len(ids) == 0 {
		return out, nil
	}
	if len(targets) > 0 && !providerValue.IsValid() {
		return out, invalidInput("账号来源无效")
	}
	// Atomic path: lock roots → expand links → lock final → media handling → delete.
	outcome, err := s.accounts.DeleteManyWithLinked(ctx, providerValue, ids, targets, skipMedia)
	if err != nil {
		return out, mapLinkedDeleteError(err)
	}
	s.finishLinkedDelete(ctx, outcome.DeletedIDs)
	if outcome.Deleted > 0 {
		s.invalidateBuildBotFlagCache()
	}
	return accountDeleteResultFromOutcome(providerValue, outcome), nil
}

// AccountsBelongToProvider 校验批量账号是否全部属于指定号池。
// 该校验只读取账号主表，避免详情页的额度、审计或关联查询影响批量操作。
func (s *Service) AccountsBelongToProvider(ctx context.Context, ids []uint64, providerValue accountdomain.Provider) (bool, error) {
	if !providerValue.IsValid() {
		return false, invalidInput("账号来源无效")
	}
	values, err := normalizeBatchIDs(ids)
	if err != nil {
		return false, err
	}
	count, err := s.accounts.CountProviderAccountsByIDs(ctx, providerValue, values)
	if err != nil {
		return false, err
	}
	return count == int64(len(values)), nil
}

// CleanupResult summarizes rows deleted and root groups skipped by one cleanup operation.
type CleanupResult struct {
	Deleted           int64
	RootsDeleted      int64
	LinkedDeleted     int64
	Skipped           int64
	DeletedByProvider map[accountdomain.Provider]int64
}

// validateCleanupSelection validates cleanup states and linked target providers.
// Any registered provider can be cleaned; linked targets exist only between Grok providers.
func (s *Service) validateCleanupSelection(providerValue accountdomain.Provider, statuses []CleanupStatus, targets []accountdomain.Provider) (map[CleanupStatus]struct{}, error) {
	if _, ok := s.ProviderDefinition(providerValue); !ok {
		return nil, invalidInput("账号来源无效")
	}
	selected := make(map[CleanupStatus]struct{}, len(statuses))
	for _, status := range statuses {
		switch status {
		case CleanupStatusCooldown, CleanupStatusDisabled, CleanupStatusReauthRequired:
			selected[status] = struct{}{}
		default:
			return nil, invalidInput("账号清理状态无效")
		}
	}
	if len(selected) == 0 {
		return nil, invalidInput("至少选择一种账号状态")
	}
	for _, target := range targets {
		if !target.IsLegacy() {
			return nil, invalidInput("关联删除目标无效")
		}
		if target == providerValue {
			return nil, invalidInput("关联删除目标不能包含当前号池")
		}
	}
	return selected, nil
}

// CleanupAccounts deletes accounts in selected admin states; healthy, waiting-reset, and probing accounts are excluded.
// Linked targets are resolved from binding tables regardless of peer state, and active-media groups are skipped whole.
// The ID cursor always advances, so skipped groups cannot stall a cleanup batch.
func (s *Service) CleanupAccounts(ctx context.Context, providerValue accountdomain.Provider, statuses []CleanupStatus, targets []accountdomain.Provider) (CleanupResult, error) {
	out := CleanupResult{DeletedByProvider: map[accountdomain.Provider]int64{}}
	selected, err := s.validateCleanupSelection(providerValue, statuses, targets)
	if err != nil {
		return out, err
	}

	const cleanupBatchSize = 500
	now := s.now()
	for _, status := range []CleanupStatus{CleanupStatusDisabled, CleanupStatusReauthRequired, CleanupStatusCooldown} {
		if _, ok := selected[status]; !ok {
			continue
		}
		var afterID uint64
		for {
			outcome, candidates, maxID, err := s.accounts.DeleteAccountStatusBatchWithLinked(ctx, providerValue, string(status), now, afterID, cleanupBatchSize, targets)
			if err != nil {
				return out, mapLinkedDeleteError(err)
			}
			s.finishLinkedDelete(ctx, outcome.DeletedIDs)
			out.Deleted += outcome.Deleted
			out.RootsDeleted += outcome.RootsDeleted
			out.LinkedDeleted += outcome.Deleted - outcome.RootsDeleted
			out.Skipped += int64(len(outcome.SkippedRoots))
			if outcome.RootsDeleted > 0 {
				out.DeletedByProvider[providerValue] += outcome.RootsDeleted
			}
			for provider, count := range outcome.LinkedDeletedByProvider {
				out.DeletedByProvider[provider] += count
			}
			if candidates < cleanupBatchSize {
				break
			}
			afterID = maxID
		}
	}
	if out.Deleted > 0 {
		s.invalidateBuildBotFlagCache()
	}
	return out, nil
}

// PreviewCleanup returns root and linked-peer counts for the cleanup confirmation dialog.
// The preview is informational; deletion revalidates state inside each transaction.
func (s *Service) PreviewCleanup(ctx context.Context, providerValue accountdomain.Provider, statuses []CleanupStatus, targets []accountdomain.Provider) (repository.CleanupPreview, error) {
	selected, err := s.validateCleanupSelection(providerValue, statuses, targets)
	if err != nil {
		return repository.CleanupPreview{}, err
	}
	raw := make([]string, 0, len(selected))
	for _, status := range []CleanupStatus{CleanupStatusDisabled, CleanupStatusReauthRequired, CleanupStatusCooldown} {
		if _, ok := selected[status]; ok {
			raw = append(raw, string(status))
		}
	}
	preview, err := s.accounts.CountCleanupWithLinked(ctx, providerValue, raw, s.now(), targets)
	if err != nil {
		return repository.CleanupPreview{}, mapLinkedDeleteError(err)
	}
	return preview, nil
}

func (s *Service) Delete(ctx context.Context, id uint64) error {
	// Single-account delete must preserve ErrNotFound when the root row is gone
	// (BatchDeleteWithLinked/DeleteMany return deleted=0, nil for missing IDs).
	result, err := s.DeleteWithLinked(ctx, accountdomain.Provider(""), id, nil)
	if err != nil {
		return err
	}
	if result.Deleted == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteWithLinked deletes one account and optional linked peers.
// A single delete is rejected if any account in the final group has an active video job.
func (s *Service) DeleteWithLinked(ctx context.Context, providerValue accountdomain.Provider, id uint64, targets []accountdomain.Provider) (AccountDeleteResult, error) {
	if id == 0 {
		return AccountDeleteResult{}, invalidInput("账号 ID 无效")
	}
	result, err := s.batchDeleteWithLinkedMode(ctx, providerValue, []uint64{id}, targets, false)
	if err != nil {
		return result, err
	}
	// Fail closed for the single-root API: missing root must not report success.
	if result.Deleted == 0 {
		return result, ErrNotFound
	}
	return result, nil
}

// PreviewLinkedDelete returns root/linked counts for the delete confirmation UI.
func (s *Service) PreviewLinkedDelete(ctx context.Context, providerValue accountdomain.Provider, ids []uint64, targets []accountdomain.Provider) (repository.LinkedDeleteResolution, error) {
	ids, err := normalizeBatchIDs(ids)
	if err != nil {
		return repository.LinkedDeleteResolution{}, err
	}
	if !providerValue.IsValid() {
		return repository.LinkedDeleteResolution{}, invalidInput("账号来源无效")
	}
	resolution, err := s.accounts.ResolveLinkedDeleteIDs(ctx, providerValue, ids, targets)
	if err != nil {
		return repository.LinkedDeleteResolution{}, mapLinkedDeleteError(err)
	}
	return resolution, nil
}

// mapRepositoryError 隔离持久化层错误，避免 transport 依赖仓储实现语义。
func mapLinkedDeleteError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(msg, "关联删除目标") || strings.Contains(msg, "账号来源无效") || strings.Contains(msg, "不支持清理账号状态") {
		return invalidInput(msg)
	}
	return mapRepositoryError(err)
}
