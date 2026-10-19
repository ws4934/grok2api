package account

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/pkg/batch"
	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

var errQuotaRefreshBusy = errors.New("额度同步已由其他实例执行")

type quotaRefreshState struct {
	generation          uint64
	publishedGeneration uint64
	sharedGeneration    uint64
	queued              bool
	running             bool
	pending             bool
	failures            int
	nextAttemptAt       time.Time
}

type quotaRefreshRequest struct {
	key       string
	accountID uint64
	mode      string
}

type QuotaRefreshStats struct {
	Pending int
	Queued  int
	Running int
}

// quotaRefreshAccounts is the account storage the quota refresh queue reads.
type quotaRefreshAccounts interface {
	Get(ctx context.Context, id uint64) (accountdomain.Credential, error)
	GetQuotaWindows(ctx context.Context, accountIDs []uint64) (map[uint64][]accountdomain.QuotaWindow, error)
}

// quotaRefreshQueue coalesces background quota refreshes per account and mode,
// runs them on a fixed worker set and retries failures with backoff. The
// optional coordinator shares dirty generations across instances.
type quotaRefreshQueue struct {
	mu       sync.Mutex
	states   map[string]*quotaRefreshState
	requests chan quotaRefreshRequest
	wake     chan struct{}

	coordinator repository.QuotaRefreshCoordinator
	accounts    quotaRefreshAccounts
	lock        repository.DistributedLock
	// syncMode refreshes one mode upstream; a nil error leaves nothing to retry.
	syncMode       func(ctx context.Context, accountID uint64, mode string) error
	isSnapshotMode func(mode string) bool
	isDeclaredMode func(mode string) bool
	logger         *slog.Logger
	now            func() time.Time
}

func (s *Service) QuotaRefreshStats() QuotaRefreshStats {
	return s.quotaRefresh.stats()
}

// QueueQuotaRefresh asynchronously refreshes the remote quota window after a successful request.
func (s *Service) QueueQuotaRefresh(id uint64, mode string) {
	s.quotaRefresh.enqueue(id, mode)
}

// RunQuotaRefresh uses a fixed worker set to avoid unbounded goroutine creation.
func (s *Service) RunQuotaRefresh(ctx context.Context) {
	s.quotaRefresh.run(ctx)
}

// syncQueuedQuota refreshes one queued mode on the shared sync pool.
func (s *Service) syncQueuedQuota(ctx context.Context, accountID uint64, mode string) error {
	var refreshErr error
	if err := s.syncPool.Do(ctx, func(workCtx context.Context) error {
		if mode == accountdomain.QuotaGroupWebImagine {
			var refreshed quotaRefreshResult
			refreshed, refreshErr = s.refreshQuotaGroup(workCtx, accountID, mode)
			if refreshErr == nil {
				refreshErr = s.reconcileQuotaGroupWindows(workCtx, refreshed.Credential.Provider, accountID, refreshed.Modes, refreshed.Windows)
			}
		} else {
			_, refreshErr = s.RefreshQuotaMode(workCtx, accountID, mode)
			if errors.Is(refreshErr, accountdomain.ErrQuotaNotLimited) {
				// The stored snapshot no longer limits this mode: nothing to retry.
				refreshErr = nil
			}
		}
		return refreshErr
	}); err != nil {
		refreshErr = err
	}
	return refreshErr
}

func (q *quotaRefreshQueue) stats() QuotaRefreshStats {
	q.mu.Lock()
	defer q.mu.Unlock()
	result := QuotaRefreshStats{}
	for _, state := range q.states {
		if state == nil {
			continue
		}
		if state.pending || state.queued || state.running {
			result.Pending++
		}
		if state.queued {
			result.Queued++
		}
		if state.running {
			result.Running++
		}
	}
	return result
}

func (q *quotaRefreshQueue) enqueue(id uint64, mode string) {
	mode = strings.TrimSpace(mode)
	if isWebImagineQuotaMode(mode) {
		mode = accountdomain.QuotaGroupWebImagine
	}
	if id == 0 || (!isConsoleUsageQuotaMode(mode) && mode != "weekly" && mode != accountdomain.QuotaGroupWebImagine && !isWebChatQuotaMode(mode) && !q.isDeclaredMode(mode)) {
		return
	}
	key := strconv.FormatUint(id, 10) + ":" + mode
	q.mu.Lock()
	state := q.states[key]
	now := q.now().UTC()
	if state != nil && !state.pending && !state.queued && !state.running && !now.Before(state.nextAttemptAt) {
		delete(q.states, key)
		state = nil
	}
	if state == nil {
		state = &quotaRefreshState{}
		q.states[key] = state
	}
	state.generation++
	state.pending = true
	enqueued := state.queued || state.running || now.Before(state.nextAttemptAt) || q.enqueueLocked(quotaRefreshRequest{key: key, accountID: id, mode: mode}, state)
	q.mu.Unlock()
	if !enqueued {
		perfmetrics.Default.Add("quota_refresh_events", perfmetrics.Labels{Subsystem: "quota", Stage: "enqueue", Outcome: "queue_full"}, 1)
		q.logger.Warn("quota_refresh_queue_full", "account_id", id, "mode", mode)
		q.wakeRecovery()
	}
}

func (q *quotaRefreshQueue) enqueueLocked(request quotaRefreshRequest, state *quotaRefreshState) bool {
	if state == nil || state.queued || state.running {
		return state != nil
	}
	select {
	case q.requests <- request:
		state.queued = true
		return true
	default:
		return false
	}
}

func (q *quotaRefreshQueue) wakeRecovery() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *quotaRefreshQueue) run(ctx context.Context) {
	var workers sync.WaitGroup
	workers.Add(managedTaskWorkerCeiling + 1)
	for range managedTaskWorkerCeiling {
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case request := <-q.requests:
					q.mu.Lock()
					state := q.states[request.key]
					if state == nil || state.running {
						q.mu.Unlock()
						continue
					}
					state.queued = false
					state.running = true
					state.pending = false
					q.mu.Unlock()
					if err := batch.Do(ctx, func(workCtx context.Context) error {
						q.refresh(workCtx, request)
						return nil
					}); err != nil {
						q.mu.Lock()
						if state := q.states[request.key]; state != nil {
							state.running = false
							state.pending = true
							state.failures++
							state.nextAttemptAt = q.now().UTC().Add(quotaRefreshRetryDelay(state.failures))
						}
						q.mu.Unlock()
						q.wakeRecovery()
						if ctx.Err() == nil {
							var panicErr *batch.PanicError
							if errors.As(err, &panicErr) {
								q.logger.Error("quota_refresh_worker_panicked", "account_id", request.accountID, "mode", request.mode, "error", panicErr, "stack", string(panicErr.Stack))
							} else {
								q.logger.Error("quota_refresh_worker_failed", "account_id", request.accountID, "mode", request.mode, "error", err)
							}
						}
					}
				}
			}
		}()
	}
	go func() {
		defer workers.Done()
		q.runRecovery(ctx)
	}()
	workers.Wait()
}

func (q *quotaRefreshQueue) refresh(parent context.Context, request quotaRefreshRequest) {
	for {
		q.mu.Lock()
		state := q.states[request.key]
		if state == nil {
			q.mu.Unlock()
			return
		}
		localGeneration := state.generation
		publishedGeneration := state.publishedGeneration
		sharedGeneration := state.sharedGeneration
		state.pending = false
		q.mu.Unlock()

		ctx, cancel := context.WithTimeout(parent, quotaRefreshTimeout)
		if q.coordinator != nil && publishedGeneration < localGeneration {
			generation, err := q.coordinator.MarkQuotaRefreshDirty(ctx, request.accountID, request.mode, quotaRefreshDirtyTTL)
			if err != nil {
				cancel()
				q.deferRetry(request.key)
				perfmetrics.Default.Add("quota_refresh_events", perfmetrics.Labels{Subsystem: "quota", Stage: "publish", Outcome: "failed"}, 1)
				q.logger.Warn("quota_refresh_dirty_publish_failed", "account_id", request.accountID, "mode", request.mode, "error", err)
				return
			}
			sharedGeneration = generation
			q.mu.Lock()
			if current := q.states[request.key]; current != nil && current.publishedGeneration < localGeneration {
				current.publishedGeneration = localGeneration
				current.sharedGeneration = generation
			}
			q.mu.Unlock()
		}
		if q.coordinator != nil && publishedGeneration >= localGeneration && sharedGeneration > 0 {
			generation, dirty, err := q.coordinator.QuotaRefreshGeneration(ctx, request.accountID, request.mode)
			if err != nil {
				cancel()
				q.deferRetry(request.key)
				return
			}
			// A lower generation means the shared marker was lost (Redis
			// restart, eviction or TTL): adopt it, or the post-sync check never
			// matches and the refresh loops on the upstream without pause.
			if generation != sharedGeneration {
				sharedGeneration = generation
				q.mu.Lock()
				if current := q.states[request.key]; current != nil {
					current.sharedGeneration = generation
				}
				q.mu.Unlock()
			}
			if !dirty && generation == sharedGeneration {
				cancel()
				q.mu.Lock()
				if current := q.states[request.key]; current != nil && current.generation == localGeneration {
					delete(q.states, request.key)
				}
				q.mu.Unlock()
				return
			}
		}
		// Background refreshes serve accounts the gateway can route to; a
		// deleted, disabled or reauth-required account would be retried forever.
		if credential, err := q.accounts.Get(ctx, request.accountID); errors.Is(err, repository.ErrNotFound) || (err == nil && (!credential.Enabled || credential.AuthStatus != accountdomain.AuthStatusActive)) {
			cancel()
			q.mu.Lock()
			if current := q.states[request.key]; current != nil && current.generation == localGeneration {
				delete(q.states, request.key)
			}
			q.mu.Unlock()
			return
		}
		refreshMode := request.mode
		consoleMode := isConsoleUsageQuotaMode(request.mode)
		// Snapshot providers (ChatGPT) read every window in one call: like
		// Console, their refreshes share a per-account lock and a minimum
		// interval, and right after a sync the local decrement already counts
		// the request.
		snapshotMode := !consoleMode && q.isSnapshotMode(request.mode)
		intervalMode := consoleMode || snapshotMode
		minInterval := consoleQuotaRefreshMinInterval
		if snapshotMode {
			minInterval = snapshotQuotaRefreshMinInterval
		}
		skipUpstream := false
		if windows, err := q.accounts.GetQuotaWindows(ctx, []uint64{request.accountID}); err == nil {
			if consoleMode {
				for _, window := range windows[request.accountID] {
					if window.Mode == request.mode && window.SyncedAt != nil && q.now().UTC().Sub(window.SyncedAt.UTC()) < consoleQuotaRefreshMinInterval {
						skipUpstream = true
						break
					}
				}
			} else if snapshotMode {
				for _, window := range windows[request.accountID] {
					if window.SyncedAt != nil && q.now().UTC().Sub(window.SyncedAt.UTC()) < minInterval {
						skipUpstream = true
						break
					}
				}
			} else if request.mode != accountdomain.QuotaGroupWebImagine {
				// Weekly remains a Grok Web capability. Console never inherits this
				// legacy mode and always refreshes its authoritative /usage snapshot.
				// Imagine 配额组走 /rest/media/imagine/quota_info，不可被改刷 weekly。
				for _, window := range windows[request.accountID] {
					if window.Mode == "weekly" {
						refreshMode = "weekly"
						break
					}
				}
			}
		}
		var refreshErr error
		acquired := true
		var release func()
		if !skipUpstream && q.lock != nil {
			lockKey := "quota-refresh:" + strconv.FormatUint(request.accountID, 10) + ":" + refreshMode
			if consoleMode {
				// Every Console mode reads the same /usage snapshot. Serialize all
				// three kinds across instances to avoid duplicate upstream probes.
				lockKey = consoleQuotaRefreshLockKey(request.accountID)
			} else if snapshotMode {
				lockKey = "quota-refresh:" + strconv.FormatUint(request.accountID, 10) + ":snapshot"
			}
			release, acquired, refreshErr = q.lock.Acquire(ctx, lockKey, quotaRefreshTimeout)
		}
		if !skipUpstream && refreshErr == nil && acquired {
			refreshErr = q.syncMode(ctx, request.accountID, refreshMode)
		}
		if release != nil {
			release()
		}
		cancel()
		if refreshErr != nil || !acquired {
			if refreshErr != nil && !errors.Is(refreshErr, context.Canceled) {
				q.logger.Warn("quota_refresh_failed", "account_id", request.accountID, "mode", refreshMode, "error", refreshErr)
			}
			q.deferRetry(request.key)
			perfmetrics.Default.Add("quota_refresh_events", perfmetrics.Labels{Subsystem: "quota", Stage: "refresh", Outcome: "retry"}, 1)
			return
		}

		currentShared := sharedGeneration
		sharedDirty := q.coordinator != nil
		if q.coordinator != nil {
			generationCtx, generationCancel := context.WithTimeout(context.WithoutCancel(parent), 3*time.Second)
			var generationErr error
			currentShared, sharedDirty, generationErr = q.coordinator.QuotaRefreshGeneration(generationCtx, request.accountID, request.mode)
			generationCancel()
			if generationErr != nil {
				q.deferRetry(request.key)
				return
			}
		}
		q.mu.Lock()
		state = q.states[request.key]
		localChanged := state != nil && state.generation != localGeneration
		q.mu.Unlock()
		if localChanged || (q.coordinator != nil && currentShared != sharedGeneration) {
			perfmetrics.Default.Add("quota_refresh_events", perfmetrics.Labels{Subsystem: "quota", Stage: "refresh", Outcome: "trailing"}, 1)
			if intervalMode {
				q.deferSuccess(request.key, true, minInterval)
				return
			}
			continue
		}
		if q.coordinator != nil && sharedDirty {
			clearCtx, clearCancel := context.WithTimeout(context.WithoutCancel(parent), 3*time.Second)
			cleared, clearErr := q.coordinator.ClearQuotaRefreshDirty(clearCtx, request.accountID, request.mode, sharedGeneration)
			clearCancel()
			if clearErr != nil || !cleared {
				if clearErr != nil {
					q.logger.Warn("quota_refresh_dirty_clear_failed", "account_id", request.accountID, "mode", request.mode, "error", clearErr)
				}
				if intervalMode {
					q.deferSuccess(request.key, true, minInterval)
					return
				}
				continue
			}
		}
		q.mu.Lock()
		state = q.states[request.key]
		if state != nil && state.generation == localGeneration {
			if intervalMode {
				state.running = false
				state.pending = false
				state.failures = 0
				state.nextAttemptAt = q.now().UTC().Add(minInterval)
			} else {
				delete(q.states, request.key)
			}
			q.mu.Unlock()
			perfmetrics.Default.Add("quota_refresh_events", perfmetrics.Labels{Subsystem: "quota", Stage: "refresh", Outcome: "success"}, 1)
			return
		}
		if intervalMode && state != nil {
			state.running = false
			state.pending = true
			state.failures = 0
			state.nextAttemptAt = q.now().UTC().Add(minInterval)
			q.mu.Unlock()
			q.wakeRecovery()
			return
		}
		q.mu.Unlock()
	}
}

func (q *quotaRefreshQueue) deferRetry(key string) {
	q.mu.Lock()
	if state := q.states[key]; state != nil {
		state.running = false
		state.pending = true
		state.failures++
		state.nextAttemptAt = q.now().UTC().Add(quotaRefreshRetryDelay(state.failures))
	}
	q.mu.Unlock()
	q.wakeRecovery()
}

func (q *quotaRefreshQueue) deferSuccess(key string, pending bool, interval time.Duration) {
	q.mu.Lock()
	if state := q.states[key]; state != nil {
		state.running = false
		state.pending = pending
		state.failures = 0
		state.nextAttemptAt = q.now().UTC().Add(interval)
	}
	q.mu.Unlock()
	q.wakeRecovery()
}

func quotaRefreshRetryDelay(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	shift := min(failures-1, 10)
	delay := quotaRefreshBackoffBase * time.Duration(1<<shift)
	if delay > quotaRefreshBackoffMax {
		delay = quotaRefreshBackoffMax
	}
	// Equal jitter keeps retries bounded away from zero while preventing a
	// shared upstream outage from synchronizing every account worker.
	half := delay / 2
	if half <= 0 {
		return delay
	}
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

func (q *quotaRefreshQueue) runRecovery(ctx context.Context) {
	retryTicker := time.NewTicker(quotaRefreshPollInterval)
	sharedTicker := time.NewTicker(quotaRefreshSharedPoll)
	defer retryTicker.Stop()
	defer sharedTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-q.wake:
			q.requeue()
		case <-retryTicker.C:
			q.requeue()
		case now := <-sharedTicker.C:
			q.recoverShared(ctx, now.UTC())
			q.requeue()
		}
	}
}

func (q *quotaRefreshQueue) requeue() {
	now := q.now().UTC()
	q.mu.Lock()
	for key, state := range q.states {
		if state == nil {
			delete(q.states, key)
			continue
		}
		if !state.pending {
			if !state.queued && !state.running && !now.Before(state.nextAttemptAt) {
				delete(q.states, key)
			}
			continue
		}
		if state.queued || state.running || now.Before(state.nextAttemptAt) {
			continue
		}
		separator := strings.IndexByte(key, ':')
		if separator <= 0 || separator == len(key)-1 {
			continue
		}
		accountID, err := strconv.ParseUint(key[:separator], 10, 64)
		if err != nil {
			continue
		}
		if !q.enqueueLocked(quotaRefreshRequest{key: key, accountID: accountID, mode: key[separator+1:]}, state) {
			break
		}
	}
	q.mu.Unlock()
}

func (q *quotaRefreshQueue) recoverShared(parent context.Context, now time.Time) {
	if q.coordinator == nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	values, err := q.coordinator.ListQuotaRefreshDirty(ctx, now, 100)
	cancel()
	if err != nil {
		q.logger.Warn("quota_refresh_dirty_list_failed", "error", err)
		return
	}
	q.mu.Lock()
	for _, value := range values {
		key := strconv.FormatUint(value.AccountID, 10) + ":" + value.Mode
		state := q.states[key]
		if state == nil {
			state = &quotaRefreshState{generation: 1, publishedGeneration: 1, sharedGeneration: value.Generation, pending: true}
			q.states[key] = state
		} else {
			if value.Generation > state.sharedGeneration {
				state.sharedGeneration = value.Generation
			}
			state.pending = true
		}
		if !state.queued && !state.running && !now.Before(state.nextAttemptAt) && !q.enqueueLocked(quotaRefreshRequest{key: key, accountID: value.AccountID, mode: value.Mode}, state) {
			break
		}
	}
	q.mu.Unlock()
}
