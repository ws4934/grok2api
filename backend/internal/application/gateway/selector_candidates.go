package gateway

import (
	"context"
	"fmt"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/pkg/flight"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func (s *Selector) maybeFilterBotFlagged(ctx context.Context, provider account.Provider, values []account.RoutingCandidate, includeBotFlagged bool) ([]account.RoutingCandidate, error) {
	if includeBotFlagged {
		return values, nil
	}
	return s.applyBuildBotFlaggedFilter(ctx, provider, values)
}

// applyBuildBotFlaggedFilter drops accounts the provider flags as a scheduling
// risk while the administrator excludes them.
func (s *Selector) applyBuildBotFlaggedFilter(_ context.Context, provider account.Provider, values []account.RoutingCandidate) ([]account.RoutingCandidate, error) {
	risk, ok := s.tierOrders.(interface {
		SchedulingRisk(account.Provider, account.Credential) bool
	})
	if !ok || len(values) == 0 || !s.excludeBuildBotFlaggedEnabled() {
		return values, nil
	}
	filtered := make([]account.RoutingCandidate, 0, len(values))
	for _, candidate := range values {
		if risk.SchedulingRisk(provider, candidate.Credential) {
			continue
		}
		filtered = append(filtered, candidate)
	}
	return filtered, nil
}

func (s *Selector) loadCandidates(ctx context.Context, provider account.Provider, modelRouteID uint64, upstreamModel, quotaMode string, now time.Time, includeBotFlagged ...bool) ([]account.RoutingCandidate, error) {
	include := false
	if len(includeBotFlagged) > 0 {
		include = includeBotFlagged[0]
	}
	if _, ok := s.accounts.(repository.RoutingLayerRepository); ok {
		return s.loadLayeredCandidates(ctx, provider, modelRouteID, upstreamModel, quotaMode, now, include)
	}
	return s.loadCombinedCandidates(ctx, provider, modelRouteID, upstreamModel, quotaMode, now, include)
}

func (s *Selector) loadCombinedCandidates(ctx context.Context, provider account.Provider, modelRouteID uint64, upstreamModel, quotaMode string, now time.Time, includeBotFlagged bool) ([]account.RoutingCandidate, error) {
	key := candidateCacheKey{provider: provider, modelRouteID: modelRouteID, upstreamModel: upstreamModel, quotaMode: quotaMode, includeBotFlagged: includeBotFlagged}
	s.candidateMu.Lock()
	if snapshot, ok := s.candidates[key]; ok && now.Before(snapshot.expiresAt) {
		snapshot.lastAccess = now
		s.candidates[key] = snapshot
		s.candidateMu.Unlock()
		return snapshot.values, nil
	}
	s.candidateMu.Unlock()
	loadKey := fmt.Sprintf("%s\x00%d\x00%s\x00%s\x00%t", provider, modelRouteID, upstreamModel, quotaMode, includeBotFlagged)
	return flight.Do(ctx, &s.candidateLoads, loadKey, candidateLoadTimeout, func(ctx context.Context) ([]account.RoutingCandidate, error) {
		checkTime := time.Now().UTC()
		var stale candidateSnapshot
		hasStale := false
		s.candidateMu.Lock()
		if snapshot, ok := s.candidates[key]; ok {
			if checkTime.Before(snapshot.expiresAt) {
				snapshot.lastAccess = checkTime
				s.candidates[key] = snapshot
				s.candidateMu.Unlock()
				return snapshot.values, nil
			}
			if checkTime.Before(snapshot.staleUntil) {
				stale, hasStale = snapshot, true
			}
		}
		s.candidateMu.Unlock()
		values, err := s.accounts.ListRoutingCandidates(ctx, provider, modelRouteID, upstreamModel, quotaMode)
		if err != nil {
			if hasStale && canUseStaleRoutingSnapshot(ctx, err) {
				s.candidateMu.Lock()
				stale.lastAccess = checkTime
				stale.expiresAt = staleRetryExpiry(checkTime, stale.staleUntil)
				s.storeCandidateSnapshotLocked(key, stale, checkTime)
				s.candidateMu.Unlock()
				s.logStaleRoutingFallback("combined", provider, checkTime, stale.staleUntil, err)
				return stale.values, nil
			}
			return nil, err
		}
		values, err = s.maybeFilterBotFlagged(ctx, provider, values, includeBotFlagged)
		if err != nil {
			return nil, err
		}
		s.candidateMu.Lock()
		s.storeCandidateSnapshotLocked(key, newCandidateSnapshot(values, checkTime.Add(candidateCacheTTL)), checkTime)
		s.candidateMu.Unlock()
		return values, nil
	})
}

func (s *Selector) loadLayeredCandidates(ctx context.Context, provider account.Provider, modelRouteID uint64, upstreamModel, quotaMode string, now time.Time, includeBotFlagged bool) ([]account.RoutingCandidate, error) {
	key := candidateCacheKey{provider: provider, modelRouteID: modelRouteID, upstreamModel: upstreamModel, quotaMode: quotaMode, includeBotFlagged: includeBotFlagged}
	s.candidateMu.Lock()
	if snapshot, ok := s.candidates[key]; ok && now.Before(snapshot.expiresAt) {
		snapshot.lastAccess = now
		s.candidates[key] = snapshot
		s.candidateMu.Unlock()
		return snapshot.values, nil
	}
	s.candidateMu.Unlock()
	loadKey := fmt.Sprintf("assembled\x00%s\x00%d\x00%s\x00%s\x00%t", provider, modelRouteID, upstreamModel, quotaMode, includeBotFlagged)
	return flight.Do(ctx, &s.candidateLoads, loadKey, candidateLoadTimeout, func(ctx context.Context) ([]account.RoutingCandidate, error) {
		checkTime := time.Now().UTC()
		s.candidateMu.Lock()
		if snapshot, ok := s.candidates[key]; ok && checkTime.Before(snapshot.expiresAt) {
			snapshot.lastAccess = checkTime
			s.candidates[key] = snapshot
			s.candidateMu.Unlock()
			return snapshot.values, nil
		}
		s.candidateMu.Unlock()
		layered := s.accounts.(repository.RoutingLayerRepository)
		for attempt := 0; attempt < 4; attempt++ {
			bases, baseVersion, loadErr := s.loadRoutingBases(ctx, layered, provider, quotaMode, checkTime)
			if loadErr != nil {
				return nil, loadErr
			}
			overlay, overlayVersion, loadErr := s.loadRoutingOverlay(ctx, layered, provider, modelRouteID, upstreamModel, checkTime)
			if loadErr != nil {
				return nil, loadErr
			}
			if !s.routingVersionsStable(provider, baseVersion, overlayVersion) {
				checkTime = time.Now().UTC()
				continue
			}
			values := assembleRoutingCandidates(s.staticModel(provider, quotaMode), bases, overlay)
			values, filterErr := s.maybeFilterBotFlagged(ctx, provider, values, includeBotFlagged)
			if filterErr != nil {
				return nil, filterErr
			}
			s.candidateMu.Lock()
			stable := baseVersion == s.routingBaseVersionLocked(provider) && overlayVersion == s.routingOverlayVersionLocked(provider)
			if stable {
				s.storeCandidateSnapshotLocked(key, newCandidateSnapshot(values, checkTime.Add(candidateCacheTTL)), checkTime)
			}
			s.candidateMu.Unlock()
			if stable {
				return values, nil
			}
			checkTime = time.Now().UTC()
		}
		// Sustained account synchronization must not turn cache churn into user-facing
		// failures. Fall back to the established authoritative combined query.
		values, err := s.accounts.ListRoutingCandidates(ctx, provider, modelRouteID, upstreamModel, quotaMode)
		if err != nil {
			return nil, err
		}
		return s.maybeFilterBotFlagged(ctx, provider, values, includeBotFlagged)
	})
}

func (s *Selector) loadRoutingBases(ctx context.Context, layered repository.RoutingLayerRepository, provider account.Provider, quotaMode string, now time.Time) ([]account.RoutingAccountBase, routingLayerVersion, error) {
	key := routingBaseCacheKey{provider: provider, quotaMode: quotaMode}
	version := s.routingBaseVersion(provider)
	s.candidateMu.Lock()
	if snapshot, ok := s.routingBases[key]; ok && now.Before(snapshot.expiresAt) && snapshot.version == version {
		snapshot.lastAccess = now
		s.routingBases[key] = snapshot
		values := snapshot.values
		s.candidateMu.Unlock()
		return values, version, nil
	}
	s.candidateMu.Unlock()
	loadKey := "base\x00" + string(provider) + "\x00" + quotaMode
	result, err := flight.Do(ctx, &s.candidateLoads, loadKey, candidateLoadTimeout, func(ctx context.Context) (routingBaseLoadResult, error) {
		checkTime := time.Now().UTC()
		checkVersion := s.routingBaseVersion(provider)
		var stale routingBaseSnapshot
		hasStale := false
		s.candidateMu.Lock()
		if snapshot, ok := s.routingBases[key]; ok && snapshot.version == checkVersion {
			if checkTime.Before(snapshot.expiresAt) {
				snapshot.lastAccess = checkTime
				s.routingBases[key] = snapshot
				values := snapshot.values
				s.candidateMu.Unlock()
				return routingBaseLoadResult{values: values, version: checkVersion}, nil
			}
			if checkTime.Before(snapshot.staleUntil) {
				stale, hasStale = snapshot, true
			}
		}
		s.candidateMu.Unlock()
		values, loadErr := layered.ListRoutingAccountBases(ctx, provider, quotaMode)
		if loadErr != nil {
			if hasStale && canUseStaleRoutingSnapshot(ctx, loadErr) {
				s.candidateMu.Lock()
				stale.lastAccess = checkTime
				stale.expiresAt = staleRetryExpiry(checkTime, stale.staleUntil)
				s.storeRoutingBaseSnapshotLocked(key, stale, checkTime)
				s.candidateMu.Unlock()
				s.logStaleRoutingFallback("base", provider, checkTime, stale.staleUntil, loadErr)
				return routingBaseLoadResult{values: stale.values, version: checkVersion}, nil
			}
			return routingBaseLoadResult{}, loadErr
		}
		s.candidateMu.Lock()
		currentVersion := s.routingBaseVersionLocked(provider)
		if currentVersion == checkVersion {
			s.clearQuotaConsumption(provider)
			s.storeRoutingBaseSnapshotLocked(key, routingBaseSnapshot{values: values, version: checkVersion, expiresAt: checkTime.Add(candidateCacheTTL)}, checkTime)
			for accountID, cachedProvider := range s.routingAccountProvider {
				if cachedProvider == provider {
					delete(s.routingAccountProvider, accountID)
				}
			}
			for _, value := range values {
				s.routingAccountProvider[value.Credential.ID] = provider
			}
		}
		s.candidateMu.Unlock()
		return routingBaseLoadResult{values: values, version: checkVersion}, nil
	})
	if err != nil {
		return nil, routingLayerVersion{}, err
	}
	return result.values, result.version, nil
}

func (s *Selector) loadRoutingOverlay(ctx context.Context, layered repository.RoutingLayerRepository, provider account.Provider, modelRouteID uint64, upstreamModel string, now time.Time) (account.RoutingOverlaySnapshot, routingLayerVersion, error) {
	key := routingOverlayCacheKey{provider: provider, modelRouteID: modelRouteID, upstreamModel: upstreamModel}
	version := s.routingOverlayVersion(provider)
	s.candidateMu.Lock()
	if snapshot, ok := s.routingOverlays[key]; ok && now.Before(snapshot.expiresAt) && snapshot.version == version {
		snapshot.lastAccess = now
		s.routingOverlays[key] = snapshot
		value := snapshot.value
		s.candidateMu.Unlock()
		return value, version, nil
	}
	s.candidateMu.Unlock()
	loadKey := fmt.Sprintf("overlay\x00%s\x00%d\x00%s", provider, modelRouteID, upstreamModel)
	result, err := flight.Do(ctx, &s.candidateLoads, loadKey, candidateLoadTimeout, func(ctx context.Context) (routingOverlayLoadResult, error) {
		checkTime := time.Now().UTC()
		checkVersion := s.routingOverlayVersion(provider)
		var stale routingOverlaySnapshot
		hasStale := false
		s.candidateMu.Lock()
		if snapshot, ok := s.routingOverlays[key]; ok && snapshot.version == checkVersion {
			if checkTime.Before(snapshot.expiresAt) {
				snapshot.lastAccess = checkTime
				s.routingOverlays[key] = snapshot
				value := snapshot.value
				s.candidateMu.Unlock()
				return routingOverlayLoadResult{value: value, version: checkVersion}, nil
			}
			if checkTime.Before(snapshot.staleUntil) {
				stale, hasStale = snapshot, true
			}
		}
		s.candidateMu.Unlock()
		value, loadErr := layered.ListRoutingAccountOverlays(ctx, provider, modelRouteID, upstreamModel)
		if loadErr != nil {
			if hasStale && canUseStaleRoutingSnapshot(ctx, loadErr) {
				s.candidateMu.Lock()
				stale.lastAccess = checkTime
				stale.expiresAt = staleRetryExpiry(checkTime, stale.staleUntil)
				s.storeRoutingOverlaySnapshotLocked(key, stale, checkTime)
				s.candidateMu.Unlock()
				s.logStaleRoutingFallback("overlay", provider, checkTime, stale.staleUntil, loadErr)
				return routingOverlayLoadResult{value: stale.value, version: checkVersion}, nil
			}
			return routingOverlayLoadResult{}, loadErr
		}
		s.candidateMu.Lock()
		currentVersion := s.routingOverlayVersionLocked(provider)
		if currentVersion == checkVersion {
			s.storeRoutingOverlaySnapshotLocked(key, routingOverlaySnapshot{value: value, version: checkVersion, expiresAt: checkTime.Add(candidateCacheTTL)}, checkTime)
		}
		s.candidateMu.Unlock()
		return routingOverlayLoadResult{value: value, version: checkVersion}, nil
	})
	if err != nil {
		return account.RoutingOverlaySnapshot{}, routingLayerVersion{}, err
	}
	return result.value, result.version, nil
}

func (s *Selector) routingBaseVersion(provider account.Provider) routingLayerVersion {
	s.candidateMu.Lock()
	defer s.candidateMu.Unlock()
	return s.routingBaseVersionLocked(provider)
}

func (s *Selector) routingBaseVersionLocked(provider account.Provider) routingLayerVersion {
	return routingLayerVersion{global: s.baseGlobalVersion, provider: s.baseProviderVersion[provider]}
}

func (s *Selector) routingOverlayVersion(provider account.Provider) routingLayerVersion {
	s.candidateMu.Lock()
	defer s.candidateMu.Unlock()
	return s.routingOverlayVersionLocked(provider)
}

func (s *Selector) routingOverlayVersionLocked(provider account.Provider) routingLayerVersion {
	return routingLayerVersion{global: s.overlayGlobalVersion, provider: s.overlayProviderVersion[provider]}
}

func (s *Selector) routingVersionsStable(provider account.Provider, base, overlay routingLayerVersion) bool {
	s.candidateMu.Lock()
	defer s.candidateMu.Unlock()
	return base == s.routingBaseVersionLocked(provider) && overlay == s.routingOverlayVersionLocked(provider)
}

type routingBaseLoadResult struct {
	values  []account.RoutingAccountBase
	version routingLayerVersion
}

type routingOverlayLoadResult struct {
	value   account.RoutingOverlaySnapshot
	version routingLayerVersion
}

// staticModel asks the provider registry whether every account serves the
// models of quotaMode; without a registry no model is static.
func (s *Selector) staticModel(provider account.Provider, quotaMode string) bool {
	registry, ok := s.tierOrders.(interface {
		StaticModel(account.Provider, string) bool
	})
	return ok && registry.StaticModel(provider, quotaMode)
}

func assembleRoutingCandidates(staticProviderModel bool, bases []account.RoutingAccountBase, overlay account.RoutingOverlaySnapshot) []account.RoutingCandidate {
	byAccount := make(map[uint64]account.RoutingAccountOverlay, len(overlay.Values))
	for _, value := range overlay.Values {
		byAccount[value.AccountID] = value
	}
	// Build Super entitlements share models: one Super account proving support
	// covers every Super account. IsBuildSuper is false for other providers.
	sharedSuperBuildModel := false
	if !overlay.HasBindings {
		for _, base := range bases {
			value, exists := byAccount[base.Credential.ID]
			if exists && value.SupportsModel && account.IsBuildSuper(base.Credential, base.Billing) {
				sharedSuperBuildModel = true
				break
			}
		}
	}
	result := make([]account.RoutingCandidate, 0, len(bases))
	for _, base := range bases {
		overlayValue := byAccount[base.Credential.ID]
		if overlay.HasBindings && !overlayValue.Bound {
			continue
		}
		known, supports := overlayValue.ModelCapabilityKnown, overlayValue.SupportsModel
		if staticProviderModel {
			known, supports = true, true
		} else if overlay.HasBindings {
			known, supports = true, true
		} else if sharedSuperBuildModel && account.IsBuildSuper(base.Credential, base.Billing) {
			known, supports = true, true
		}
		result = append(result, account.RoutingCandidate{
			Credential: base.Credential, Billing: base.Billing, QuotaWindow: base.QuotaWindow, QuotaRecovery: base.QuotaRecovery,
			EgressLeaseBlock: base.EgressLeaseBlock, ModelQuotaBlock: overlayValue.ModelQuotaBlock, ModelCapabilityKnown: known, SupportsModel: supports,
		})
	}
	return result
}
