package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
)

const quotaProbeLease = 5 * time.Minute

func (s *Selector) Acquire(ctx context.Context, provider account.Provider, modelRouteID uint64, upstreamModel, quotaMode, affinityKey string, excluded map[uint64]bool, allowQuotaProbe bool) (*accountLease, error) {
	return s.acquire(ctx, provider, modelRouteID, upstreamModel, quotaMode, affinityKey, excluded, allowQuotaProbe, clientkeydomain.AccountScope{}, 0)
}

func (s *Selector) AcquireForKey(ctx context.Context, provider account.Provider, modelRouteID uint64, upstreamModel, quotaMode, affinityKey string, excluded map[uint64]bool, allowQuotaProbe bool, scope clientkeydomain.AccountScope) (*accountLease, error) {
	return s.acquire(ctx, provider, modelRouteID, upstreamModel, quotaMode, affinityKey, excluded, allowQuotaProbe, scope, 0)
}

// AcquireForKeyOnEgressNode is reserved for administrator probes. It prefers a
// credential bound to the requested node, then borrows any schedulable
// credential when the node's own accounts are unavailable. The request layer
// still forces the physical call through nodeID.
func (s *Selector) AcquireForKeyOnEgressNode(ctx context.Context, provider account.Provider, modelRouteID uint64, upstreamModel, quotaMode, affinityKey string, excluded map[uint64]bool, allowQuotaProbe bool, scope clientkeydomain.AccountScope, nodeID uint64) (*accountLease, error) {
	if nodeID == 0 {
		return nil, &SelectionUnavailableError{Reason: SelectionNoAccounts, Scope: scope}
	}
	lease, err := s.acquire(ctx, provider, modelRouteID, upstreamModel, quotaMode, affinityKey, excluded, allowQuotaProbe, scope, nodeID)
	if err == nil {
		return lease, nil
	}
	var unavailable *SelectionUnavailableError
	if !errors.As(err, &unavailable) {
		return nil, err
	}
	// Probe borrowing must not create or reuse ordinary sticky affinity.
	return s.acquire(ctx, provider, modelRouteID, upstreamModel, quotaMode, "", excluded, allowQuotaProbe, scope, 0)
}

func (s *Selector) acquire(ctx context.Context, provider account.Provider, modelRouteID uint64, upstreamModel, quotaMode, affinityKey string, excluded map[uint64]bool, allowQuotaProbe bool, requestedScope clientkeydomain.AccountScope, forcedEgressNodeID uint64) (*accountLease, error) {
	session, err := s.beginSelectionSessionOnNode(ctx, provider, modelRouteID, upstreamModel, quotaMode, affinityKey, excluded, allowQuotaProbe, requestedScope, forcedEgressNodeID)
	if err != nil {
		return nil, err
	}
	return session.Acquire(ctx, excluded, allowQuotaProbe)
}

// stickySessionKey 将调用方粘滞 identity 压缩为固定长度，仅用于账号粘滞索引。
func stickySessionKey(value string) string {
	if value == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func routingCandidateByID(values []account.RoutingCandidate, indexes []int, accountID uint64) (account.RoutingCandidate, bool) {
	for _, index := range indexes {
		if values[index].Credential.ID == accountID {
			return values[index], true
		}
	}
	return account.RoutingCandidate{}, false
}

func isSelectionUnavailable(err error, reason SelectionUnavailableReason) bool {
	var unavailable *SelectionUnavailableError
	return errors.As(err, &unavailable) && unavailable.Reason == reason
}

// AcquirePinned 为 previous_response_id 等账号归属请求获取指定账号租约。
func (s *Selector) AcquirePinned(ctx context.Context, provider account.Provider, accountID, modelRouteID uint64, upstreamModel, quotaMode string, inference bool) (*accountLease, error) {
	return s.acquirePinned(ctx, provider, accountID, modelRouteID, upstreamModel, quotaMode, inference, false, clientkeydomain.AccountScope{})
}

func (s *Selector) AcquirePinnedForKey(ctx context.Context, provider account.Provider, accountID, modelRouteID uint64, upstreamModel, quotaMode string, inference bool, scope clientkeydomain.AccountScope) (*accountLease, error) {
	if pinned := creativeAccountID(ctx); pinned != 0 {
		if pinned != accountID {
			return nil, pinnedUnavailableError(pinned, "")
		}
		normalized, valid := clientkeydomain.NormalizeAccountScope(scope)
		if !valid || !normalized.AllowsProvider(provider) {
			return nil, &SelectionUnavailableError{Reason: SelectionNoAccounts, Scope: normalized}
		}
		candidate, err := s.creativeCandidate(ctx, provider, accountID, modelRouteID, upstreamModel, quotaMode, normalized)
		if err != nil {
			return nil, err
		}
		return s.acquireCreativeCandidate(ctx, candidate, quotaMode)
	}
	return s.acquirePinned(ctx, provider, accountID, modelRouteID, upstreamModel, quotaMode, inference, false, scope)
}

// AcquirePinnedForQualityProbe keeps every ordinary eligibility check while
// bypassing only the exact account+node lease block being recovery-tested.
func (s *Selector) AcquirePinnedForQualityProbe(ctx context.Context, provider account.Provider, accountID, modelRouteID uint64, upstreamModel, quotaMode string, scope clientkeydomain.AccountScope) (*accountLease, error) {
	return s.acquirePinned(ctx, provider, accountID, modelRouteID, upstreamModel, quotaMode, true, true, scope)
}

func (s *Selector) acquirePinned(ctx context.Context, provider account.Provider, accountID, modelRouteID uint64, upstreamModel, quotaMode string, inference, ignoreEgressLeaseBlock bool, requestedScope clientkeydomain.AccountScope) (lease *accountLease, err error) {
	accountScope, scopeValid := clientkeydomain.NormalizeAccountScope(requestedScope)
	defer annotateSelectionAccountScope(&err, accountScope)
	if !scopeValid || !accountScope.AllowsProvider(provider) {
		return nil, &SelectionUnavailableError{Reason: SelectionNoAccounts, Scope: accountScope}
	}
	now := time.Now().UTC()
	values, err := s.loadCandidates(ctx, provider, modelRouteID, upstreamModel, quotaMode, now, true)
	if err != nil {
		return nil, err
	}
	healthOverrides := s.routingHealthSnapshot(provider, now)
	index, found := s.pinnedCandidateIndex(candidateCacheKey{provider: provider, modelRouteID: modelRouteID, upstreamModel: upstreamModel, quotaMode: quotaMode, includeBotFlagged: true}, values, accountID)
	if !found {
		return nil, pinnedUnavailableError(accountID, "")
	}
	candidate := values[index]
	value := applyHealthSnapshot(candidate.Credential, healthOverrides)
	if !s.accountScopeAllowsCandidate(provider, accountScope, candidate) {
		if accountScope.IsRestricted() {
			return nil, &SelectionUnavailableError{Reason: SelectionNoAccounts, Scope: accountScope, AccountID: accountID, AccountName: value.DisplayName()}
		}
		return nil, pinnedUnavailableError(accountID, value.DisplayName())
	}
	if !value.Enabled || value.AuthStatus != account.AuthStatusActive {
		return nil, pinnedUnavailableError(accountID, value.DisplayName())
	}
	if inference {
		if !s.candidateSupportsModel(provider, upstreamModel, quotaMode, candidate) {
			return nil, &SelectionUnavailableError{Reason: SelectionUnsupportedModel}
		}
		if candidate.ModelQuotaBlock != nil && now.Before(candidate.ModelQuotaBlock.CooldownUntil) {
			return nil, &SelectionUnavailableError{Reason: SelectionModelCooling, RetryAfter: retryDelay(now, candidate.ModelQuotaBlock.CooldownUntil)}
		}
		if !ignoreEgressLeaseBlock && candidateEgressLeaseCooling(&candidate, now) {
			return nil, &SelectionUnavailableError{Reason: SelectionCooling, RetryAfter: retryDelay(now, candidate.EgressLeaseBlock.CooldownUntil)}
		}
		if value.CooldownUntil != nil && now.Before(*value.CooldownUntil) {
			return nil, &SelectionUnavailableError{Reason: SelectionCooling, RetryAfter: retryDelay(now, *value.CooldownUntil)}
		}
		if recovery := candidate.QuotaRecovery; recovery != nil && recovery.Status != account.QuotaRecoveryStatusActive {
			if recovery.NextProbeAt == nil || now.Before(*recovery.NextProbeAt) {
				var retryAfter time.Duration
				if recovery.NextProbeAt != nil {
					retryAfter = retryDelay(now, *recovery.NextProbeAt)
				}
				return nil, &SelectionUnavailableError{Reason: SelectionQuotaExhausted, RetryAfter: retryAfter}
			}
			lease, err := s.acquirePinnedCapacity(ctx, value, nil)
			if err != nil {
				if errors.Is(err, errRoutingCredentialStale) {
					return nil, pinnedUnavailableError(accountID, value.DisplayName())
				}
				return nil, err
			}
			claimed, err := s.accounts.ClaimQuotaProbe(ctx, value.ID, now, now.Add(quotaProbeLease))
			if err != nil || !claimed {
				lease.Release()
				if err != nil {
					return nil, err
				}
				return nil, fmt.Errorf("绑定的上游账号恢复探测已被占用")
			}
			lease.QuotaProbe = true
			lease.QuotaProbeKind = recovery.Kind
			lease.Billing = candidate.Billing
			return lease, nil
		}
		if candidate.Billing != nil && candidate.Billing.IsExhausted(value.MinimumRemaining) {
			return nil, &SelectionUnavailableError{Reason: SelectionQuotaExhausted}
		}
		s.quotaMu.RLock()
		quotaExhausted := quotaWindowExhausted(candidate, s.quotaConsumptionSnapshot(provider))
		s.quotaMu.RUnlock()
		if quotaExhausted {
			var retryAfter time.Duration
			if candidate.QuotaWindow.ResetAt != nil {
				retryAfter = retryDelay(now, *candidate.QuotaWindow.ResetAt)
			}
			return nil, &SelectionUnavailableError{Reason: SelectionQuotaExhausted, RetryAfter: retryAfter}
		}
	}
	lease, err = s.acquirePinnedCapacity(ctx, value, nil)
	if err != nil {
		if errors.Is(err, errRoutingCredentialStale) {
			return nil, pinnedUnavailableError(accountID, value.DisplayName())
		}
		return nil, err
	}
	lease.Billing = candidate.Billing
	lease.QuotaMode = s.effectiveQuotaMode(candidate, quotaMode)
	return lease, nil
}

// pinnedCandidateIndex finds accountID through the cached snapshot's byAccount
// index when values is that snapshot, and by a linear scan otherwise.
func (s *Selector) pinnedCandidateIndex(key candidateCacheKey, values []account.RoutingCandidate, accountID uint64) (int, bool) {
	if len(values) > 0 {
		s.candidateMu.Lock()
		snapshot, cached := s.candidates[key]
		s.candidateMu.Unlock()
		// Published snapshots and their byAccount maps are never mutated in place.
		if cached && snapshot.byAccount != nil && len(snapshot.values) == len(values) && &snapshot.values[0] == &values[0] {
			index, ok := snapshot.byAccount[accountID]
			return index, ok
		}
	}
	for index := range values {
		if values[index].Credential.ID == accountID {
			return index, true
		}
	}
	return 0, false
}

func pinnedUnavailableError(accountID uint64, name string) *SelectionUnavailableError {
	return &SelectionUnavailableError{Reason: SelectionPinnedUnavailable, AccountID: accountID, AccountName: name}
}

func annotateSelectionAccountScope(err *error, scope clientkeydomain.AccountScope) {
	if err == nil || *err == nil || !scope.IsRestricted() {
		return
	}
	var unavailable *SelectionUnavailableError
	if errors.As(*err, &unavailable) {
		unavailable.Scope = scope
	}
}
