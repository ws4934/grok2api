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

func (s *Selector) acquire(ctx context.Context, provider account.Provider, modelRouteID uint64, upstreamModel, quotaMode, affinityKey string, excluded map[uint64]bool, allowQuotaProbe bool, requestedScope clientkeydomain.AccountScope, forcedEgressNodeID uint64) (lease *accountLease, err error) {
	accountScope, scopeValid := clientkeydomain.NormalizeAccountScope(requestedScope)
	defer annotateSelectionAccountScope(&err, accountScope)
	if !scopeValid || !accountScope.AllowsProvider(provider) {
		return nil, &SelectionUnavailableError{Reason: SelectionNoAccounts, Scope: accountScope}
	}
	now := time.Now().UTC()
	stickyKey := stickySessionKey(affinityKey)
	values, err := s.loadCandidates(ctx, provider, modelRouteID, upstreamModel, quotaMode, now, false)
	if err != nil {
		return nil, err
	}
	quotaConsumed := s.quotaConsumptionSnapshot(provider)
	healthOverrides := s.routingHealthSnapshot(provider, now)
	// 仅保留候选下标，避免每个请求复制包含凭据、计费和额度结构的完整账号切片。
	normalCandidates := make([]int, 0, len(values))
	probeCandidates := make([]int, 0, len(values))
	supportedCandidates := 0
	consideredCandidates := 0
	coolingCandidates := 0
	modelCoolingCandidates := 0
	quotaCandidates := 0
	var earliestRetry time.Time
	for index, candidate := range values {
		value := applyHealthSnapshot(candidate.Credential, healthOverrides)
		if forcedEgressNodeID != 0 && value.EgressNodeID != forcedEgressNodeID {
			continue
		}
		if !s.accountScopeAllowsCandidate(provider, accountScope, candidate) {
			continue
		}
		if excluded[value.ID] || value.AuthStatus != account.AuthStatusActive {
			continue
		}
		consideredCandidates++
		if !s.candidateSupportsModel(provider, upstreamModel, quotaMode, candidate) {
			continue
		}
		supportedCandidates++
		if candidate.ModelQuotaBlock != nil && now.Before(candidate.ModelQuotaBlock.CooldownUntil) {
			modelCoolingCandidates++
			earliestRetry = earlierFuture(earliestRetry, candidate.ModelQuotaBlock.CooldownUntil, now)
			continue
		}
		if candidateEgressLeaseCooling(candidate, value, now) {
			coolingCandidates++
			earliestRetry = earlierFuture(earliestRetry, candidate.EgressLeaseBlock.CooldownUntil, now)
			continue
		}
		if value.CooldownUntil != nil && now.Before(*value.CooldownUntil) {
			coolingCandidates++
			earliestRetry = earlierFuture(earliestRetry, *value.CooldownUntil, now)
			continue
		}
		quotaRecovery := candidate.QuotaRecovery
		if quotaRecovery != nil && quotaRecovery.Status != account.QuotaRecoveryStatusActive {
			if allowQuotaProbe && quotaRecovery.NextProbeAt != nil && !now.Before(*quotaRecovery.NextProbeAt) {
				probeCandidates = append(probeCandidates, index)
			} else {
				quotaCandidates++
				if quotaRecovery.NextProbeAt != nil {
					earliestRetry = earlierFuture(earliestRetry, *quotaRecovery.NextProbeAt, now)
				}
			}
			continue
		}
		if candidate.Billing != nil && candidate.Billing.IsExhausted(value.MinimumRemaining) {
			quotaCandidates++
			continue
		}
		if quotaWindowExhausted(candidate, quotaConsumed) {
			quotaCandidates++
			if candidate.QuotaWindow.ResetAt != nil {
				earliestRetry = earlierFuture(earliestRetry, *candidate.QuotaWindow.ResetAt, now)
			}
			continue
		}
		normalCandidates = append(normalCandidates, index)
	}
	materialFailures := credentialMaterialFailureTracker{}
	if len(normalCandidates) == 0 && len(probeCandidates) == 0 {
		reason := SelectionNoAccounts
		switch {
		case consideredCandidates > 0 && supportedCandidates == 0:
			reason = SelectionUnsupportedModel
		case modelCoolingCandidates > 0:
			reason = SelectionModelCooling
		case coolingCandidates > 0:
			reason = SelectionCooling
		case quotaCandidates > 0:
			reason = SelectionQuotaExhausted
		}
		return nil, &SelectionUnavailableError{Reason: reason, RetryAfter: retryDelay(now, earliestRetry)}
	}
	if len(probeCandidates) > 0 {
		staleClaims := 0
		capacityMisses := 0
		plan, err := s.planCandidateIndexes(ctx, values, probeCandidates, now, s.resolveTierOrder(provider, upstreamModel, quotaMode))
		if err != nil {
			return nil, err
		}
		for candidate, ok := plan.Next(); ok; candidate, ok = plan.Next() {
			lease, err := s.claimAccountSlotTracked(ctx, candidate.Credential, &materialFailures)
			if err != nil {
				if errors.Is(err, errRoutingCredentialStale) {
					staleClaims++
					continue
				}
				return nil, err
			}
			if lease == nil {
				capacityMisses++
				continue
			}
			claimed, err := s.accounts.ClaimQuotaProbe(ctx, candidate.Credential.ID, now, now.Add(quotaProbeLease))
			if err != nil || !claimed {
				lease.Release()
				if err != nil {
					return nil, err
				}
				continue
			}
			lease.QuotaProbe = true
			lease.QuotaProbeKind = candidate.QuotaRecovery.Kind
			lease.Billing = candidate.Billing
			return lease, nil
		}
		if len(normalCandidates) == 0 && staleClaims > 0 && capacityMisses == 0 {
			return nil, &SelectionUnavailableError{Reason: SelectionNoAccounts}
		}
	}
	var saturatedStickyID uint64
	if stickyKey != "" {
		stickyID, ok, err := s.sticky.Get(ctx, stickyKey, now)
		if err != nil {
			return nil, fmt.Errorf("读取会话粘滞状态: %w", err)
		}
		if ok {
			candidate, eligible := routingCandidateByID(values, normalCandidates, stickyID)
			if eligible {
				stickyTTL, _, _, _ := s.routingConfig()
				boundID, bindErr := s.sticky.Bind(ctx, stickyKey, stickyID, now, now.Add(stickyTTL))
				if bindErr != nil {
					return nil, fmt.Errorf("刷新会话粘滞状态: %w", bindErr)
				}
				if boundID != stickyID {
					candidate, eligible = routingCandidateByID(values, normalCandidates, boundID)
					stickyID = boundID
				}
				if eligible {
					lease, acquireErr := s.acquirePinnedCapacity(ctx, candidate.Credential, &materialFailures)
					if acquireErr == nil {
						lease.Billing = candidate.Billing
						lease.QuotaMode = s.effectiveQuotaMode(candidate, quotaMode)
						return lease, nil
					}
					if errors.Is(acquireErr, errRoutingCredentialStale) {
						_ = s.sticky.DeleteByAccount(ctx, stickyID)
					} else if !isSelectionUnavailable(acquireErr, SelectionSaturated) {
						return nil, acquireErr
					} else {
						saturatedStickyID = stickyID
					}
				}
			}
		}
	}
	// 粘性账号仅因并发满载而暂时不可用时，先等待该账号；超时后允许本次请求临时借用
	// 其他账号，但不覆盖原绑定，避免并行请求让活跃会话在账号池中来回抖动。
	if saturatedStickyID != 0 {
		plan, err := s.planCandidateIndexes(ctx, values, normalCandidates, time.Now().UTC(), s.resolveTierOrder(provider, upstreamModel, quotaMode))
		if err != nil {
			return nil, err
		}
		for candidate, ok := plan.Next(); ok; candidate, ok = plan.Next() {
			if candidate.Credential.ID == saturatedStickyID {
				continue
			}
			lease, claimErr := s.claimAccountSlotTracked(ctx, candidate.Credential, &materialFailures)
			if claimErr != nil {
				if errors.Is(claimErr, errRoutingCredentialStale) {
					continue
				}
				return nil, claimErr
			}
			if lease == nil {
				continue
			}
			lease.Billing = candidate.Billing
			lease.QuotaMode = s.effectiveQuotaMode(candidate, quotaMode)
			return lease, nil
		}
		return nil, &SelectionUnavailableError{Reason: SelectionSaturated, RetryAfter: time.Second}
	}
	activeRequest := s.nextSegmentedActiveRequest(provider, upstreamModel, quotaMode, len(normalCandidates))
	if activeRequest != nil {
		lease, acquireErr := s.acquireSegmentedCandidates(ctx, values, normalCandidates, quotaMode, s.resolveTierOrder(provider, upstreamModel, quotaMode), *activeRequest, &materialFailures)
		if acquireErr != nil || lease == nil || stickyKey == "" {
			return lease, acquireErr
		}
		if lease.routingCandidate == nil {
			lease.Release()
			return nil, errors.New("分段选号缺少候选上下文")
		}
		return s.completeStickyLease(ctx, stickyKey, values, normalCandidates, *lease.routingCandidate, lease, quotaMode, &materialFailures)
	}
	_, _, _, capacityWait := s.routingConfig()
	waitDeadline := time.Now().Add(capacityWait)
	for {
		currentTime := time.Now().UTC()
		staleClaims := 0
		capacityMisses := 0
		plan, err := s.planCandidateIndexes(ctx, values, normalCandidates, currentTime, s.resolveTierOrder(provider, upstreamModel, quotaMode))
		if err != nil {
			return nil, err
		}
		for candidate, ok := plan.Next(); ok; candidate, ok = plan.Next() {
			lease, err := s.claimAccountSlotTracked(ctx, candidate.Credential, &materialFailures)
			if err != nil {
				if errors.Is(err, errRoutingCredentialStale) {
					staleClaims++
					continue
				}
				return nil, err
			}
			if lease == nil {
				capacityMisses++
				continue
			}
			return s.completeStickyLease(ctx, stickyKey, values, normalCandidates, candidate, lease, quotaMode, &materialFailures)
		}
		if staleClaims > 0 && capacityMisses == 0 {
			return nil, &SelectionUnavailableError{Reason: SelectionNoAccounts}
		}
		if capacityWait <= 0 {
			return nil, &SelectionUnavailableError{Reason: SelectionSaturated, RetryAfter: time.Second}
		}
		retry, err := s.awaitLeaseRetry(ctx, waitDeadline)
		if err != nil {
			return nil, err
		}
		if !retry {
			return nil, &SelectionUnavailableError{Reason: SelectionSaturated, RetryAfter: time.Second}
		}
	}
}

func (s *Selector) completeStickyLease(ctx context.Context, stickyKey string, values []account.RoutingCandidate, normalCandidates []int, candidate account.RoutingCandidate, lease *accountLease, quotaMode string, materialFailures *credentialMaterialFailureTracker) (*accountLease, error) {
	if stickyKey != "" {
		stickyTTL, _, _, _ := s.routingConfig()
		now := time.Now().UTC()
		boundID, err := s.sticky.Bind(ctx, stickyKey, candidate.Credential.ID, now, now.Add(stickyTTL))
		if err != nil {
			lease.Release()
			return nil, fmt.Errorf("写入会话粘滞状态: %w", err)
		}
		if boundID != candidate.Credential.ID {
			if boundCandidate, eligible := routingCandidateByID(values, normalCandidates, boundID); eligible {
				boundLease, acquireErr := s.acquirePinnedCapacity(ctx, boundCandidate.Credential, materialFailures)
				if acquireErr == nil {
					lease.Release()
					lease = boundLease
					candidate = boundCandidate
				} else if errors.Is(acquireErr, errRoutingCredentialStale) {
					_ = s.sticky.DeleteByAccount(ctx, boundID)
					if err := s.sticky.Set(ctx, stickyKey, candidate.Credential.ID, now.Add(stickyTTL)); err != nil {
						lease.Release()
						return nil, fmt.Errorf("重建会话粘滞状态: %w", err)
					}
				} else if !isSelectionUnavailable(acquireErr, SelectionSaturated) {
					lease.Release()
					return nil, acquireErr
				}
				// 已绑定账号满载时保留原绑定，本次请求使用已获取的临时账号。
			} else if err := s.sticky.Set(ctx, stickyKey, candidate.Credential.ID, now.Add(stickyTTL)); err != nil {
				lease.Release()
				return nil, fmt.Errorf("重建会话粘滞状态: %w", err)
			}
		}
	}
	lease.Billing = candidate.Billing
	lease.QuotaMode = s.effectiveQuotaMode(candidate, quotaMode)
	return lease, nil
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
		candidate := values[index]
		if candidate.Credential.ID == accountID {
			return candidate, true
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
	quotaConsumed := s.quotaConsumptionSnapshot(provider)
	healthOverrides := s.routingHealthSnapshot(provider, now)
	for _, candidate := range values {
		value := applyHealthSnapshot(candidate.Credential, healthOverrides)
		if value.ID != accountID {
			continue
		}
		if !s.accountScopeAllowsCandidate(provider, accountScope, candidate) {
			if accountScope.IsRestricted() {
				return nil, &SelectionUnavailableError{Reason: SelectionNoAccounts, Scope: accountScope, AccountID: accountID, AccountName: value.Name}
			}
			return nil, pinnedUnavailableError(accountID, value.Name)
		}
		if !value.Enabled || value.AuthStatus != account.AuthStatusActive {
			return nil, pinnedUnavailableError(accountID, value.Name)
		}
		if inference {
			if !s.candidateSupportsModel(provider, upstreamModel, quotaMode, candidate) {
				return nil, &SelectionUnavailableError{Reason: SelectionUnsupportedModel}
			}
			if candidate.ModelQuotaBlock != nil && now.Before(candidate.ModelQuotaBlock.CooldownUntil) {
				return nil, &SelectionUnavailableError{Reason: SelectionModelCooling, RetryAfter: retryDelay(now, candidate.ModelQuotaBlock.CooldownUntil)}
			}
			if !ignoreEgressLeaseBlock && candidateEgressLeaseCooling(candidate, value, now) {
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
						return nil, pinnedUnavailableError(accountID, value.Name)
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
			if quotaWindowExhausted(candidate, quotaConsumed) {
				var retryAfter time.Duration
				if candidate.QuotaWindow.ResetAt != nil {
					retryAfter = retryDelay(now, *candidate.QuotaWindow.ResetAt)
				}
				return nil, &SelectionUnavailableError{Reason: SelectionQuotaExhausted, RetryAfter: retryAfter}
			}
		}
		lease, err := s.acquirePinnedCapacity(ctx, value, nil)
		if err != nil {
			if errors.Is(err, errRoutingCredentialStale) {
				return nil, pinnedUnavailableError(accountID, value.Name)
			}
			return nil, err
		}
		lease.Billing = candidate.Billing
		lease.QuotaMode = s.effectiveQuotaMode(candidate, quotaMode)
		return lease, nil
	}
	return nil, pinnedUnavailableError(accountID, "")
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
