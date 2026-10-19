package account

import (
	"context"
	"errors"
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

func (s *Service) QuotaRefreshStats() QuotaRefreshStats {
	s.quotaRefreshMu.Lock()
	defer s.quotaRefreshMu.Unlock()
	result := QuotaRefreshStats{}
	for _, state := range s.quotaRefreshes {
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

// QueueQuotaRefresh asynchronously refreshes the remote quota window after a successful request.
func (s *Service) QueueQuotaRefresh(id uint64, mode string) {
	mode = strings.TrimSpace(mode)
	if isWebImagineQuotaMode(mode) {
		mode = accountdomain.QuotaGroupWebImagine
	}
	if id == 0 || (!isConsoleUsageQuotaMode(mode) && mode != "weekly" && mode != accountdomain.QuotaGroupWebImagine && !isWebChatQuotaMode(mode) && !s.isDeclaredQuotaMode(mode)) {
		return
	}
	key := strconv.FormatUint(id, 10) + ":" + mode
	s.quotaRefreshMu.Lock()
	state := s.quotaRefreshes[key]
	now := s.now().UTC()
	if state != nil && !state.pending && !state.queued && !state.running && !now.Before(state.nextAttemptAt) {
		delete(s.quotaRefreshes, key)
		state = nil
	}
	if state == nil {
		state = &quotaRefreshState{}
		s.quotaRefreshes[key] = state
	}
	state.generation++
	state.pending = true
	enqueued := state.queued || state.running || now.Before(state.nextAttemptAt) || s.enqueueQuotaRefreshLocked(quotaRefreshRequest{key: key, accountID: id, mode: mode}, state)
	s.quotaRefreshMu.Unlock()
	if !enqueued {
		perfmetrics.Default.Add("quota_refresh_events", perfmetrics.Labels{Subsystem: "quota", Stage: "enqueue", Outcome: "queue_full"}, 1)
		s.logger.Warn("quota_refresh_queue_full", "account_id", id, "mode", mode)
		s.wakeQuotaRefreshRecovery()
	}
}

func (s *Service) enqueueQuotaRefreshLocked(request quotaRefreshRequest, state *quotaRefreshState) bool {
	if state == nil || state.queued || state.running {
		return state != nil
	}
	select {
	case s.quotaRefreshQueue <- request:
		state.queued = true
		return true
	default:
		return false
	}
}

func (s *Service) wakeQuotaRefreshRecovery() {
	select {
	case s.quotaRefreshWake <- struct{}{}:
	default:
	}
}

// RunQuotaRefresh uses a fixed worker set to avoid unbounded goroutine creation.
func (s *Service) RunQuotaRefresh(ctx context.Context) {
	var workers sync.WaitGroup
	workers.Add(managedTaskWorkerCeiling + 1)
	for range managedTaskWorkerCeiling {
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case request := <-s.quotaRefreshQueue:
					s.quotaRefreshMu.Lock()
					state := s.quotaRefreshes[request.key]
					if state == nil || state.running {
						s.quotaRefreshMu.Unlock()
						continue
					}
					state.queued = false
					state.running = true
					state.pending = false
					s.quotaRefreshMu.Unlock()
					if err := batch.Do(ctx, func(workCtx context.Context) error {
						s.runQuotaRefresh(workCtx, request)
						return nil
					}); err != nil {
						s.quotaRefreshMu.Lock()
						if state := s.quotaRefreshes[request.key]; state != nil {
							state.running = false
							state.pending = true
							state.failures++
							state.nextAttemptAt = s.now().UTC().Add(quotaRefreshRetryDelay(state.failures))
						}
						s.quotaRefreshMu.Unlock()
						s.wakeQuotaRefreshRecovery()
						if ctx.Err() == nil {
							var panicErr *batch.PanicError
							if errors.As(err, &panicErr) {
								s.logger.Error("quota_refresh_worker_panicked", "account_id", request.accountID, "mode", request.mode, "error", panicErr, "stack", string(panicErr.Stack))
							} else {
								s.logger.Error("quota_refresh_worker_failed", "account_id", request.accountID, "mode", request.mode, "error", err)
							}
						}
					}
				}
			}
		}()
	}
	go func() {
		defer workers.Done()
		s.runQuotaRefreshRecovery(ctx)
	}()
	workers.Wait()
}

func (s *Service) runQuotaRefresh(parent context.Context, request quotaRefreshRequest) {
	for {
		s.quotaRefreshMu.Lock()
		state := s.quotaRefreshes[request.key]
		if state == nil {
			s.quotaRefreshMu.Unlock()
			return
		}
		localGeneration := state.generation
		publishedGeneration := state.publishedGeneration
		sharedGeneration := state.sharedGeneration
		state.pending = false
		s.quotaRefreshMu.Unlock()

		ctx, cancel := context.WithTimeout(parent, quotaRefreshTimeout)
		if s.quotaRefreshState != nil && publishedGeneration < localGeneration {
			generation, err := s.quotaRefreshState.MarkQuotaRefreshDirty(ctx, request.accountID, request.mode, quotaRefreshDirtyTTL)
			if err != nil {
				cancel()
				s.deferQuotaRefresh(request.key)
				perfmetrics.Default.Add("quota_refresh_events", perfmetrics.Labels{Subsystem: "quota", Stage: "publish", Outcome: "failed"}, 1)
				s.logger.Warn("quota_refresh_dirty_publish_failed", "account_id", request.accountID, "mode", request.mode, "error", err)
				return
			}
			sharedGeneration = generation
			s.quotaRefreshMu.Lock()
			if current := s.quotaRefreshes[request.key]; current != nil && current.publishedGeneration < localGeneration {
				current.publishedGeneration = localGeneration
				current.sharedGeneration = generation
			}
			s.quotaRefreshMu.Unlock()
		}
		if s.quotaRefreshState != nil && publishedGeneration >= localGeneration && sharedGeneration > 0 {
			generation, dirty, err := s.quotaRefreshState.QuotaRefreshGeneration(ctx, request.accountID, request.mode)
			if err != nil {
				cancel()
				s.deferQuotaRefresh(request.key)
				return
			}
			// A lower generation means the shared marker was lost (Redis
			// restart, eviction or TTL): adopt it, or the post-sync check never
			// matches and the refresh loops on the upstream without pause.
			if generation != sharedGeneration {
				sharedGeneration = generation
				s.quotaRefreshMu.Lock()
				if current := s.quotaRefreshes[request.key]; current != nil {
					current.sharedGeneration = generation
				}
				s.quotaRefreshMu.Unlock()
			}
			if !dirty && generation == sharedGeneration {
				cancel()
				s.quotaRefreshMu.Lock()
				if current := s.quotaRefreshes[request.key]; current != nil && current.generation == localGeneration {
					delete(s.quotaRefreshes, request.key)
				}
				s.quotaRefreshMu.Unlock()
				return
			}
		}
		// Background refreshes serve accounts the gateway can route to; a
		// deleted, disabled or reauth-required account would be retried forever.
		if credential, err := s.accounts.Get(ctx, request.accountID); errors.Is(err, repository.ErrNotFound) || (err == nil && (!credential.Enabled || credential.AuthStatus != accountdomain.AuthStatusActive)) {
			cancel()
			s.quotaRefreshMu.Lock()
			if current := s.quotaRefreshes[request.key]; current != nil && current.generation == localGeneration {
				delete(s.quotaRefreshes, request.key)
			}
			s.quotaRefreshMu.Unlock()
			return
		}
		refreshMode := request.mode
		consoleMode := isConsoleUsageQuotaMode(request.mode)
		// Snapshot providers (ChatGPT) read every window in one call: like
		// Console, their refreshes share a per-account lock and a minimum
		// interval, and right after a sync the local decrement already counts
		// the request.
		snapshotMode := !consoleMode && s.snapshotQuotaMode(request.mode)
		intervalMode := consoleMode || snapshotMode
		minInterval := consoleQuotaRefreshMinInterval
		if snapshotMode {
			minInterval = snapshotQuotaRefreshMinInterval
		}
		skipUpstream := false
		if windows, err := s.accounts.GetQuotaWindows(ctx, []uint64{request.accountID}); err == nil {
			if consoleMode {
				for _, window := range windows[request.accountID] {
					if window.Mode == request.mode && window.SyncedAt != nil && s.now().UTC().Sub(window.SyncedAt.UTC()) < consoleQuotaRefreshMinInterval {
						skipUpstream = true
						break
					}
				}
			} else if snapshotMode {
				for _, window := range windows[request.accountID] {
					if window.SyncedAt != nil && s.now().UTC().Sub(window.SyncedAt.UTC()) < minInterval {
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
		if !skipUpstream && s.refreshLock != nil {
			lockKey := "quota-refresh:" + strconv.FormatUint(request.accountID, 10) + ":" + refreshMode
			if consoleMode {
				// Every Console mode reads the same /usage snapshot. Serialize all
				// three kinds across instances to avoid duplicate upstream probes.
				lockKey = consoleQuotaRefreshLockKey(request.accountID)
			} else if snapshotMode {
				lockKey = "quota-refresh:" + strconv.FormatUint(request.accountID, 10) + ":snapshot"
			}
			release, acquired, refreshErr = s.refreshLock.Acquire(ctx, lockKey, quotaRefreshTimeout)
		}
		if !skipUpstream && refreshErr == nil && acquired {
			if err := s.syncPool.Do(ctx, func(workCtx context.Context) error {
				if refreshMode == accountdomain.QuotaGroupWebImagine {
					var refreshed quotaRefreshResult
					refreshed, refreshErr = s.refreshQuotaGroup(workCtx, request.accountID, refreshMode)
					if refreshErr == nil {
						refreshErr = s.reconcileQuotaGroupWindows(workCtx, refreshed.Credential.Provider, request.accountID, refreshed.Modes, refreshed.Windows)
					}
				} else {
					_, refreshErr = s.RefreshQuotaMode(workCtx, request.accountID, refreshMode)
					if errors.Is(refreshErr, accountdomain.ErrQuotaNotLimited) {
						// The stored snapshot no longer limits this mode: nothing to retry.
						refreshErr = nil
					}
				}
				return refreshErr
			}); err != nil {
				refreshErr = err
			}
		}
		if release != nil {
			release()
		}
		cancel()
		if refreshErr != nil || !acquired {
			if refreshErr != nil && !errors.Is(refreshErr, context.Canceled) {
				s.logger.Warn("quota_refresh_failed", "account_id", request.accountID, "mode", refreshMode, "error", refreshErr)
			}
			s.deferQuotaRefresh(request.key)
			perfmetrics.Default.Add("quota_refresh_events", perfmetrics.Labels{Subsystem: "quota", Stage: "refresh", Outcome: "retry"}, 1)
			return
		}

		currentShared := sharedGeneration
		sharedDirty := s.quotaRefreshState != nil
		if s.quotaRefreshState != nil {
			generationCtx, generationCancel := context.WithTimeout(context.WithoutCancel(parent), 3*time.Second)
			var generationErr error
			currentShared, sharedDirty, generationErr = s.quotaRefreshState.QuotaRefreshGeneration(generationCtx, request.accountID, request.mode)
			generationCancel()
			if generationErr != nil {
				s.deferQuotaRefresh(request.key)
				return
			}
		}
		s.quotaRefreshMu.Lock()
		state = s.quotaRefreshes[request.key]
		localChanged := state != nil && state.generation != localGeneration
		s.quotaRefreshMu.Unlock()
		if localChanged || (s.quotaRefreshState != nil && currentShared != sharedGeneration) {
			perfmetrics.Default.Add("quota_refresh_events", perfmetrics.Labels{Subsystem: "quota", Stage: "refresh", Outcome: "trailing"}, 1)
			if intervalMode {
				s.deferSuccessfulQuotaRefresh(request.key, true, minInterval)
				return
			}
			continue
		}
		if s.quotaRefreshState != nil && sharedDirty {
			clearCtx, clearCancel := context.WithTimeout(context.WithoutCancel(parent), 3*time.Second)
			cleared, clearErr := s.quotaRefreshState.ClearQuotaRefreshDirty(clearCtx, request.accountID, request.mode, sharedGeneration)
			clearCancel()
			if clearErr != nil || !cleared {
				if clearErr != nil {
					s.logger.Warn("quota_refresh_dirty_clear_failed", "account_id", request.accountID, "mode", request.mode, "error", clearErr)
				}
				if intervalMode {
					s.deferSuccessfulQuotaRefresh(request.key, true, minInterval)
					return
				}
				continue
			}
		}
		s.quotaRefreshMu.Lock()
		state = s.quotaRefreshes[request.key]
		if state != nil && state.generation == localGeneration {
			if intervalMode {
				state.running = false
				state.pending = false
				state.failures = 0
				state.nextAttemptAt = s.now().UTC().Add(minInterval)
			} else {
				delete(s.quotaRefreshes, request.key)
			}
			s.quotaRefreshMu.Unlock()
			perfmetrics.Default.Add("quota_refresh_events", perfmetrics.Labels{Subsystem: "quota", Stage: "refresh", Outcome: "success"}, 1)
			return
		}
		if intervalMode && state != nil {
			state.running = false
			state.pending = true
			state.failures = 0
			state.nextAttemptAt = s.now().UTC().Add(minInterval)
			s.quotaRefreshMu.Unlock()
			s.wakeQuotaRefreshRecovery()
			return
		}
		s.quotaRefreshMu.Unlock()
	}
}

func (s *Service) deferQuotaRefresh(key string) {
	s.quotaRefreshMu.Lock()
	if state := s.quotaRefreshes[key]; state != nil {
		state.running = false
		state.pending = true
		state.failures++
		state.nextAttemptAt = s.now().UTC().Add(quotaRefreshRetryDelay(state.failures))
	}
	s.quotaRefreshMu.Unlock()
	s.wakeQuotaRefreshRecovery()
}

func (s *Service) deferSuccessfulQuotaRefresh(key string, pending bool, interval time.Duration) {
	s.quotaRefreshMu.Lock()
	if state := s.quotaRefreshes[key]; state != nil {
		state.running = false
		state.pending = pending
		state.failures = 0
		state.nextAttemptAt = s.now().UTC().Add(interval)
	}
	s.quotaRefreshMu.Unlock()
	s.wakeQuotaRefreshRecovery()
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

func (s *Service) runQuotaRefreshRecovery(ctx context.Context) {
	retryTicker := time.NewTicker(quotaRefreshPollInterval)
	sharedTicker := time.NewTicker(quotaRefreshSharedPoll)
	defer retryTicker.Stop()
	defer sharedTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.quotaRefreshWake:
			s.requeueQuotaRefreshes()
		case <-retryTicker.C:
			s.requeueQuotaRefreshes()
		case now := <-sharedTicker.C:
			s.recoverSharedQuotaRefreshes(ctx, now.UTC())
			s.requeueQuotaRefreshes()
		}
	}
}

func (s *Service) requeueQuotaRefreshes() {
	now := s.now().UTC()
	s.quotaRefreshMu.Lock()
	for key, state := range s.quotaRefreshes {
		if state == nil {
			delete(s.quotaRefreshes, key)
			continue
		}
		if !state.pending {
			if !state.queued && !state.running && !now.Before(state.nextAttemptAt) {
				delete(s.quotaRefreshes, key)
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
		if !s.enqueueQuotaRefreshLocked(quotaRefreshRequest{key: key, accountID: accountID, mode: key[separator+1:]}, state) {
			break
		}
	}
	s.quotaRefreshMu.Unlock()
}

func (s *Service) recoverSharedQuotaRefreshes(parent context.Context, now time.Time) {
	if s.quotaRefreshState == nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	values, err := s.quotaRefreshState.ListQuotaRefreshDirty(ctx, now, 100)
	cancel()
	if err != nil {
		s.logger.Warn("quota_refresh_dirty_list_failed", "error", err)
		return
	}
	s.quotaRefreshMu.Lock()
	for _, value := range values {
		key := strconv.FormatUint(value.AccountID, 10) + ":" + value.Mode
		state := s.quotaRefreshes[key]
		if state == nil {
			state = &quotaRefreshState{generation: 1, publishedGeneration: 1, sharedGeneration: value.Generation, pending: true}
			s.quotaRefreshes[key] = state
		} else {
			if value.Generation > state.sharedGeneration {
				state.sharedGeneration = value.Generation
			}
			state.pending = true
		}
		if !state.queued && !state.running && !now.Before(state.nextAttemptAt) && !s.enqueueQuotaRefreshLocked(quotaRefreshRequest{key: key, accountID: value.AccountID, mode: value.Mode}, state) {
			break
		}
	}
	s.quotaRefreshMu.Unlock()
}
