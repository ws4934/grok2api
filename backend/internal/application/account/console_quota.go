package account

import (
	"context"
	"errors"
	"strconv"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

// SyncIncompleteConsoleQuotas replaces pre-/usage synthetic windows and
// partial snapshots without refreshing accounts that already have all three
// authoritative Console quota kinds. It is safe to run periodically and uses
// the shared sync pool to preserve the deployment-wide upstream limit.
func (s *Service) SyncIncompleteConsoleQuotas(ctx context.Context) (int, int, error) {
	const batchSize = 1000
	var succeeded, failed int
	var afterID uint64
	for {
		values, _, err := s.accounts.ListProviderAccountBatch(ctx, accountdomain.ProviderConsole, afterID, batchSize)
		if err != nil {
			return succeeded, failed, err
		}
		if len(values) == 0 {
			return succeeded, failed, nil
		}
		ids := make([]uint64, 0, len(values))
		for _, value := range values {
			if value.Enabled && value.AuthStatus == accountdomain.AuthStatusActive {
				ids = append(ids, value.ID)
			}
		}
		windows, err := s.accounts.GetQuotaWindows(ctx, ids)
		if err != nil {
			return succeeded, failed, err
		}
		pending := make([]uint64, 0, len(ids))
		for _, id := range ids {
			if !completeConsoleUsageSnapshot(windows[id]) {
				pending = append(pending, id)
			}
		}
		var batchSucceeded, batchFailed int
		if len(pending) > 0 {
			batchSucceeded, batchFailed, err = s.syncConsoleQuotaAccounts(ctx, "console_usage_migration", pending)
		}
		succeeded += batchSucceeded
		failed += batchFailed
		if err != nil {
			return succeeded, failed, err
		}
		afterID = values[len(values)-1].ID
		if len(values) < batchSize {
			return succeeded, failed, nil
		}
	}
}

// SyncStaleConsoleQuotas refreshes a bounded batch of complete but old /usage
// snapshots. Active accounts are already refreshed after successful requests;
// this catch-up covers idle pools without turning a large deployment into a
// periodic upstream request burst.
func (s *Service) SyncStaleConsoleQuotas(ctx context.Context, before time.Time, afterID uint64, limit int) (int, int, uint64, error) {
	if limit <= 0 || limit > accountTaskBatchSize {
		limit = 50
	}
	pending := make([]uint64, 0, limit)
	nextAfterID := afterID
	reachedEnd := false
	for len(pending) < limit {
		values, _, err := s.accounts.ListProviderAccountBatch(ctx, accountdomain.ProviderConsole, nextAfterID, accountTaskBatchSize)
		if err != nil {
			return 0, 0, nextAfterID, err
		}
		if len(values) == 0 {
			reachedEnd = true
			break
		}
		ids := make([]uint64, 0, len(values))
		for _, value := range values {
			if value.Enabled && value.AuthStatus == accountdomain.AuthStatusActive {
				ids = append(ids, value.ID)
			}
		}
		windows, err := s.accounts.GetQuotaWindows(ctx, ids)
		if err != nil {
			return 0, 0, nextAfterID, err
		}
		for _, value := range values {
			nextAfterID = value.ID
			if value.Enabled && value.AuthStatus == accountdomain.AuthStatusActive && staleCompleteConsoleUsageSnapshot(windows[value.ID], before) {
				pending = append(pending, value.ID)
				if len(pending) == limit {
					break
				}
			}
		}
		if len(values) < accountTaskBatchSize {
			reachedEnd = len(pending) < limit
			break
		}
	}
	if reachedEnd {
		nextAfterID = 0
	}
	if len(pending) == 0 {
		return 0, 0, nextAfterID, nil
	}
	succeeded, failed, err := s.syncConsoleQuotaAccounts(ctx, "console_quota_stale_catchup", pending)
	return succeeded, failed, nextAfterID, err
}

func staleCompleteConsoleUsageSnapshot(windows []accountdomain.QuotaWindow, before time.Time) bool {
	if !completeConsoleUsageSnapshot(windows) {
		return false
	}
	for _, window := range windows {
		if !isConsoleUsageQuotaMode(window.Mode) {
			continue
		}
		if window.SyncedAt == nil || window.SyncedAt.Before(before) {
			return true
		}
	}
	return false
}

func (s *Service) syncConsoleQuotaAccounts(ctx context.Context, operation string, ids []uint64) (int, int, error) {
	return s.runAccountBatch(ctx, operation, ids, s.syncPool, nil, func(workCtx context.Context, id uint64) error {
		_, refreshErr := s.refreshConsoleQuotaModeLocked(workCtx, id, "console")
		if errors.Is(refreshErr, errQuotaRefreshBusy) {
			// Another replica already owns the refresh. Treat that as accepted work:
			// queuing the same account locally only creates a trailing duplicate probe.
			return nil
		}
		if refreshErr != nil && workCtx.Err() == nil {
			s.QueueQuotaRefresh(id, "console")
		}
		return refreshErr
	})
}

func (s *Service) refreshConsoleQuotaModeLocked(ctx context.Context, id uint64, mode string) (accountdomain.QuotaWindow, error) {
	var release func()
	if s.refreshLock != nil {
		acquiredRelease, acquired, err := s.refreshLock.Acquire(ctx, consoleQuotaRefreshLockKey(id), 2*quotaRefreshTimeout)
		if err != nil {
			return accountdomain.QuotaWindow{}, err
		}
		if !acquired {
			return accountdomain.QuotaWindow{}, errQuotaRefreshBusy
		}
		release = acquiredRelease
		defer release()
	}
	return s.RefreshQuotaMode(ctx, id, mode)
}

func consoleQuotaRefreshLockKey(id uint64) string {
	return "quota-refresh:console:" + strconv.FormatUint(id, 10)
}

func completeConsoleUsageSnapshot(windows []accountdomain.QuotaWindow) bool {
	var present uint8
	for _, window := range windows {
		if window.Source != accountdomain.QuotaSourceUpstream || window.SyncedAt == nil {
			continue
		}
		switch window.Mode {
		case "console":
			present |= 1
		case "console_image":
			present |= 2
		case "console_video":
			present |= 4
		}
	}
	return present == 7
}
