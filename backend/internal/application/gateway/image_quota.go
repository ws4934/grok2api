package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// Local decrements survive cache reloads of another mode in a shared pool.
// Only a newer authoritative upstream snapshot resets the budget baseline.
type imageQuotaBudget struct {
	remaining int
	syncedAt  time.Time
}

func quotaWindowSyncedAt(window *account.QuotaWindow) time.Time {
	if window.SyncedAt != nil {
		return *window.SyncedAt
	}
	return time.Time{}
}

func (s *Service) decrementImageQuota(ctx context.Context, value account.Provider, accountID uint64, mode string, units int) (bool, error) {
	pool := s.providers.QuotaPoolMode(value, mode)
	modes := []string{mode}
	if definition, ok := s.providers.Definition(value); ok {
		for _, other := range definition.Profile.QuotaModes {
			if other != mode && s.providers.QuotaPoolMode(value, other) == pool {
				modes = append(modes, other)
			}
		}
	}
	updated := false
	for _, current := range modes {
		changed, err := s.accounts.DecrementQuota(ctx, accountID, current, units)
		if err != nil {
			return updated, err
		}
		updated = updated || changed
	}
	return updated, nil
}

// reserveImageQuota fits one native batch to the selected account's budget.
// Its hold stays separate from consumed quota, so refresh invalidations cannot
// erase in-flight work. A Console budget is cents, not a number of images.
func (s *Service) reserveImageQuota(lease *accountLease, options provider.ImageRequestOptions) (provider.ImageRequestOptions, func(), error) {
	noop := func() {}
	if _, known := s.providers.ImageQuotaUnits(lease.Credential.Provider, lease.QuotaMode, options); !known {
		return options, noop, nil
	}
	candidate := lease.routingCandidate
	if candidate == nil || candidate.QuotaWindow == nil || candidate.QuotaWindow.Mode != lease.QuotaMode || candidate.QuotaWindow.Source != account.QuotaSourceUpstream {
		// Do not turn an unknown allowance into a blind multi-output request.
		options.Count = 1
		return options, noop, nil
	}
	window := candidate.QuotaWindow
	selector := s.selector
	key := quotaConsumptionKey{provider: lease.Credential.Provider, accountID: lease.Credential.ID, mode: selector.quotaPoolMode(lease.Credential.Provider, lease.QuotaMode)}
	selector.quotaMu.Lock()
	if selector.imageBudgets == nil {
		selector.imageBudgets = make(map[quotaConsumptionKey]imageQuotaBudget)
	}
	budget, exists := selector.imageBudgets[key]
	stamp := quotaWindowSyncedAt(window)
	if !exists || stamp.After(budget.syncedAt) {
		budget = imageQuotaBudget{remaining: window.Remaining - selector.quotaConsumed[key], syncedAt: stamp}
		selector.imageBudgets[key] = budget
	}
	available := budget.remaining - selector.quotaReserved[key]
	units := 0
	for options.Count > 0 {
		units, _ = s.providers.ImageQuotaUnits(lease.Credential.Provider, lease.QuotaMode, options)
		if units > 0 && units <= available {
			break
		}
		options.Count--
	}
	if options.Count == 0 {
		selector.quotaMu.Unlock()
		retryAfter := time.Duration(0)
		if window.ResetAt != nil {
			retryAfter = max(0, time.Until(*window.ResetAt))
		}
		return options, noop, &SelectionUnavailableError{Reason: SelectionQuotaExhausted, RetryAfter: retryAfter}
	}
	if selector.quotaReserved == nil {
		selector.quotaReserved = make(map[quotaConsumptionKey]int)
	}
	selector.quotaReserved[key] += units
	selector.quotaMu.Unlock()
	var once sync.Once
	release := func() {
		once.Do(func() {
			selector.quotaMu.Lock()
			selector.quotaReserved[key] -= units
			if selector.quotaReserved[key] == 0 {
				delete(selector.quotaReserved, key)
			}
			selector.quotaMu.Unlock()
		})
	}
	return options, release, nil
}
