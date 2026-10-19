package gateway

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// Routing writes publish precise invalidation events, so the TTL is only a
// safety net for out-of-process database changes and missed notifications.
// Keeping a one-second TTL made large pools rebuild continuously under load.
const candidateCacheTTL = 30 * time.Second
const candidateCacheStaleTTL = 5 * time.Minute
const candidateCacheRetryTTL = 5 * time.Second
const candidateCacheStaleLogInterval = time.Minute

// candidateLoadTimeout bounds a shared routing read, which no longer stops
// when the request that started it leaves. Large pools can take seconds.
const candidateLoadTimeout = 30 * time.Second
const maxCandidateCacheSnapshots = 64
const maxCandidateCacheValues = 100_000
const maxRoutingBaseSnapshots = 8
const maxRoutingBaseValues = 150_000
const maxRoutingOverlaySnapshots = 64
const maxRoutingOverlayValues = 250_000

type candidateSnapshot struct {
	values     []account.RoutingCandidate
	byAccount  map[uint64]int
	expiresAt  time.Time
	staleUntil time.Time
	lastAccess time.Time
}

func newCandidateSnapshot(values []account.RoutingCandidate, expiresAt time.Time) candidateSnapshot {
	byAccount := make(map[uint64]int, len(values))
	for index, value := range values {
		if _, exists := byAccount[value.Credential.ID]; !exists {
			byAccount[value.Credential.ID] = index
		}
	}
	now := time.Now().UTC()
	return candidateSnapshot{values: values, byAccount: byAccount, expiresAt: expiresAt, staleUntil: expiresAt.Add(candidateCacheStaleTTL), lastAccess: now}
}

type candidateCacheKey struct {
	provider          account.Provider
	modelRouteID      uint64
	upstreamModel     string
	quotaMode         string
	includeBotFlagged bool
}

type routingBaseCacheKey struct {
	provider  account.Provider
	quotaMode string
}

type routingOverlayCacheKey struct {
	provider      account.Provider
	modelRouteID  uint64
	upstreamModel string
}

type routingLayerVersion struct {
	global   uint64
	provider uint64
}

type routingBaseSnapshot struct {
	values     []account.RoutingAccountBase
	version    routingLayerVersion
	expiresAt  time.Time
	staleUntil time.Time
	lastAccess time.Time
}

type routingOverlaySnapshot struct {
	value      account.RoutingOverlaySnapshot
	version    routingLayerVersion
	expiresAt  time.Time
	staleUntil time.Time
	lastAccess time.Time
}

// invalidateCachedProviderCandidates drops the candidate caches of every
// provider with cached candidates.
func (s *Selector) invalidateCachedProviderCandidates() {
	s.candidateMu.Lock()
	providers := make(map[account.Provider]struct{})
	for key := range s.candidates {
		providers[key.provider] = struct{}{}
	}
	s.candidateMu.Unlock()
	for provider := range providers {
		s.invalidateProviderCandidateCache(provider)
	}
}

func (s *Selector) invalidateProviderCandidateCache(provider account.Provider) {
	s.candidateMu.Lock()
	defer s.candidateMu.Unlock()
	for key := range s.candidates {
		if key.provider == provider {
			delete(s.candidates, key)
		}
	}
	clearRoutingBases(s.routingBases, provider)
	clearRoutingOverlays(s.routingOverlays, provider)
	if provider != "" {
		s.baseProviderVersion[provider]++
		s.overlayProviderVersion[provider]++
	}
}

// ApplyInvalidation advances local layer generations before any remote publish.
func (s *Selector) ApplyInvalidation(event repository.InvalidationEvent) {
	if !event.Valid() || event.Layer() == repository.InvalidationLayerPrice {
		return
	}
	if event.Kind == repository.InvalidationAccountHealthChanged {
		s.applyHealthInvalidation(event)
		return
	}
	s.clearHealthOverrides(event.Provider, event.AccountID)
	layer := event.Layer()
	if layer != repository.InvalidationLayerRoute && layer != repository.InvalidationLayerBase && layer != repository.InvalidationLayerOverlay {
		return
	}
	s.candidateMu.Lock()
	provider := event.Provider
	if provider == "" && event.AccountID != 0 {
		provider = s.routingAccountProvider[event.AccountID]
		if provider == "" {
			for key, snapshot := range s.candidates {
				if _, ok := snapshot.byAccount[event.AccountID]; ok {
					provider = key.provider
					break
				}
			}
		}
	}
	base := layer == repository.InvalidationLayerBase
	overlay := layer == repository.InvalidationLayerOverlay || layer == repository.InvalidationLayerRoute
	if base {
		s.clearQuotaConsumption(provider)
		if provider == "" {
			s.baseGlobalVersion++
			clearRoutingBases(s.routingBases, "")
		} else {
			s.baseProviderVersion[provider]++
			clearRoutingBases(s.routingBases, provider)
		}
	}
	if overlay {
		if provider == "" {
			s.overlayGlobalVersion++
			clearRoutingOverlays(s.routingOverlays, "")
		} else {
			s.overlayProviderVersion[provider]++
			clearRoutingOverlays(s.routingOverlays, provider)
		}
	}
	for key := range s.candidates {
		if provider == "" || key.provider == provider {
			delete(s.candidates, key)
		}
	}
	s.candidateMu.Unlock()
}

func clearRoutingBases(values map[routingBaseCacheKey]routingBaseSnapshot, provider account.Provider) {
	for key := range values {
		if provider == "" || key.provider == provider {
			delete(values, key)
		}
	}
}

func clearRoutingOverlays(values map[routingOverlayCacheKey]routingOverlaySnapshot, provider account.Provider) {
	for key := range values {
		if provider == "" || key.provider == provider {
			delete(values, key)
		}
	}
}

func cacheSnapshotAccess(lastAccess, expiresAt time.Time) time.Time {
	if !lastAccess.IsZero() {
		return lastAccess
	}
	return expiresAt
}

func staleRetryExpiry(now, staleUntil time.Time) time.Time {
	expiresAt := now.Add(candidateCacheRetryTTL)
	if !staleUntil.IsZero() && expiresAt.After(staleUntil) {
		return staleUntil
	}
	return expiresAt
}

// canUseStaleRoutingSnapshot deliberately accepts only errors that carry a
// transient signal. Serving stale data for cancellations, schema/query bugs,
// or repository validation failures would hide correctness problems.
func canUseStaleRoutingSnapshot(ctx context.Context, err error) bool {
	if err == nil || ctx != nil && ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	_, transient := transientRoutingStoreFailureClass(err)
	return transient
}

// transientRoutingStoreFailureClass 仅识别带有明确瞬态信号的存储错误，
// 同时返回不包含账号或错误正文的稳定分类，供单次选号的故障放大保护使用。
func transientRoutingStoreFailureClass(err error) (string, bool) {
	if err == nil || errors.Is(err, context.Canceled) {
		return "", false
	}
	if errors.Is(err, repository.ErrNotFound) || errors.Is(err, repository.ErrConflict) || errors.Is(err, repository.ErrLimitExceeded) || errors.Is(err, repository.ErrInvalidRecord) || errors.Is(err, repository.ErrAccountPoolMismatch) {
		return "", false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline", true
	}
	if errors.Is(err, sql.ErrConnDone) || errors.Is(err, driver.ErrBadConn) {
		return "sql_connection", true
	}
	var networkError net.Error
	if errors.As(err, &networkError) && (networkError.Timeout() || networkError.Temporary()) {
		return fmt.Sprintf("network:%T", networkError), true
	}
	var temporary interface{ Temporary() bool }
	if errors.As(err, &temporary) && temporary.Temporary() {
		return fmt.Sprintf("temporary:%T", temporary), true
	}
	// modernc SQLite exposes the primary/extended result through Code().
	// SQLITE_BUSY (5) and SQLITE_LOCKED (6) are safe to retry.
	var sqliteError interface{ Code() int }
	if errors.As(err, &sqliteError) {
		primaryCode := sqliteError.Code() & 0xff
		switch primaryCode {
		case 5, 6:
			return fmt.Sprintf("sqlite:%d", primaryCode), true
		}
	}
	// pgx exposes SQLSTATE without requiring the application layer to depend on
	// a concrete PostgreSQL driver type.
	var postgresError interface{ SQLState() string }
	if errors.As(err, &postgresError) {
		state := postgresError.SQLState()
		switch {
		case strings.HasPrefix(state, "08"), strings.HasPrefix(state, "40"), state == "55P03", state == "57P01", state == "57P02", state == "57P03":
			return "postgres:" + state, true
		}
	}
	return "", false
}

func (s *Selector) logStaleRoutingFallback(layer string, provider account.Provider, now, staleUntil time.Time, err error) {
	key := layer + "\x00" + string(provider)
	s.staleLogMu.Lock()
	if s.staleFallbackLoggedAt == nil {
		s.staleFallbackLoggedAt = make(map[string]time.Time)
	}
	last := s.staleFallbackLoggedAt[key]
	if !last.IsZero() && now.Sub(last) < candidateCacheStaleLogInterval {
		s.staleLogMu.Unlock()
		return
	}
	s.staleFallbackLoggedAt[key] = now
	s.staleLogMu.Unlock()

	staleFor := now.Sub(staleUntil.Add(-candidateCacheStaleTTL))
	if staleFor < 0 {
		staleFor = 0
	}
	logger := s.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("routing_snapshot_stale_fallback",
		"provider", provider,
		"layer", layer,
		"stale_for_ms", staleFor.Milliseconds(),
		"retry_after_ms", candidateCacheRetryTTL.Milliseconds(),
		"error_type", fmt.Sprintf("%T", err),
	)
}

type snapshotCacheMetadata struct {
	values     int
	staleUntil time.Time
	accessedAt time.Time
}

func pruneSnapshotCache[K comparable, V any](values map[K]V, protected K, now time.Time, maxSnapshots, maxValues int, metadata func(V) snapshotCacheMetadata) {
	for key, value := range values {
		entry := metadata(value)
		if key != protected && !entry.staleUntil.IsZero() && !now.Before(entry.staleUntil) {
			delete(values, key)
		}
	}
	for {
		total := 0
		for _, value := range values {
			total += metadata(value).values
		}
		// A single oversized provider pool must remain usable. The budget becomes
		// a strict bound as soon as there is another evictable snapshot.
		if len(values) <= maxSnapshots && (total <= maxValues || len(values) == 1) {
			return
		}
		var oldestKey K
		var oldestAt time.Time
		found := false
		for key, value := range values {
			if key == protected {
				continue
			}
			accessedAt := metadata(value).accessedAt
			if !found || accessedAt.Before(oldestAt) {
				oldestKey, oldestAt, found = key, accessedAt, true
			}
		}
		if !found {
			return
		}
		delete(values, oldestKey)
	}
}

func (s *Selector) storeCandidateSnapshotLocked(key candidateCacheKey, snapshot candidateSnapshot, now time.Time) {
	snapshot.lastAccess = now
	if snapshot.staleUntil.IsZero() {
		snapshot.staleUntil = snapshot.expiresAt.Add(candidateCacheStaleTTL)
	}
	s.candidates[key] = snapshot
	pruneSnapshotCache(s.candidates, key, now, maxCandidateCacheSnapshots, maxCandidateCacheValues, func(value candidateSnapshot) snapshotCacheMetadata {
		return snapshotCacheMetadata{values: len(value.values), staleUntil: value.staleUntil, accessedAt: cacheSnapshotAccess(value.lastAccess, value.expiresAt)}
	})
}

func (s *Selector) storeRoutingBaseSnapshotLocked(key routingBaseCacheKey, snapshot routingBaseSnapshot, now time.Time) {
	snapshot.lastAccess = now
	if snapshot.staleUntil.IsZero() {
		snapshot.staleUntil = snapshot.expiresAt.Add(candidateCacheStaleTTL)
	}
	s.routingBases[key] = snapshot
	pruneSnapshotCache(s.routingBases, key, now, maxRoutingBaseSnapshots, maxRoutingBaseValues, func(value routingBaseSnapshot) snapshotCacheMetadata {
		return snapshotCacheMetadata{values: len(value.values), staleUntil: value.staleUntil, accessedAt: cacheSnapshotAccess(value.lastAccess, value.expiresAt)}
	})
}

func (s *Selector) storeRoutingOverlaySnapshotLocked(key routingOverlayCacheKey, snapshot routingOverlaySnapshot, now time.Time) {
	snapshot.lastAccess = now
	if snapshot.staleUntil.IsZero() {
		snapshot.staleUntil = snapshot.expiresAt.Add(candidateCacheStaleTTL)
	}
	s.routingOverlays[key] = snapshot
	pruneSnapshotCache(s.routingOverlays, key, now, maxRoutingOverlaySnapshots, maxRoutingOverlayValues, func(value routingOverlaySnapshot) snapshotCacheMetadata {
		return snapshotCacheMetadata{values: len(value.value.Values), staleUntil: value.staleUntil, accessedAt: cacheSnapshotAccess(value.lastAccess, value.expiresAt)}
	})
}

func (s *Selector) invalidateCandidates(provider account.Provider) {
	s.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: provider})
	s.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationAccountCapabilityChanged, Provider: provider})
}

// evictCandidate 从当前进程的候选快照中移除一个账号。持久化状态仍由调用方先写入；
// 下一个缓存周期会以数据库中的新状态重新加载该账号，不会因单账号变化清空整个 Provider。
func (s *Selector) evictCandidate(provider account.Provider, accountID uint64) {
	if accountID == 0 {
		return
	}
	s.candidateMu.Lock()
	defer s.candidateMu.Unlock()
	for key, snapshot := range s.candidates {
		if key.provider != provider {
			continue
		}
		// 候选快照会被并发请求的 selectionSession 只读复用；必须 copy-on-write，
		// 不能复用底层数组，否则会改写正在执行的请求视图。
		values := make([]account.RoutingCandidate, 0, len(snapshot.values))
		removed := false
		for _, candidate := range snapshot.values {
			if candidate.Credential.ID == accountID {
				removed = true
				continue
			}
			values = append(values, candidate)
		}
		if removed {
			// also update byAccount index if present
			if snapshot.byAccount != nil {
				byAccount := make(map[uint64]int, len(values))
				for idx, candidate := range values {
					// Keep the first position, as newCandidateSnapshot does.
					if _, exists := byAccount[candidate.Credential.ID]; !exists {
						byAccount[candidate.Credential.ID] = idx
					}
				}
				snapshot.byAccount = byAccount
			}
			snapshot.values = values
			s.candidates[key] = snapshot
		}
	}
}
