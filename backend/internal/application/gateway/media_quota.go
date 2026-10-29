package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// Local media debits survive reloads of another mode in a shared pool. Only a
// newer authoritative snapshot resets the baseline; in-flight holds survive it.
type nativeQuotaBudget struct {
	remaining int
	syncedAt  time.Time
}

func quotaWindowSyncedAt(window *account.QuotaWindow) time.Time {
	if window.SyncedAt != nil {
		return *window.SyncedAt
	}
	return time.Time{}
}

// quotaBudgetLocked and holdQuotaLocked require quotaMu to be held for writing.
func (s *Selector) quotaBudgetLocked(key quotaConsumptionKey, window *account.QuotaWindow) nativeQuotaBudget {
	if s.quotaBudgets == nil {
		s.quotaBudgets = make(map[quotaConsumptionKey]nativeQuotaBudget)
	}
	budget, exists := s.quotaBudgets[key]
	stamp := quotaWindowSyncedAt(window)
	if !exists || stamp.After(budget.syncedAt) {
		budget = nativeQuotaBudget{remaining: window.Remaining - s.quotaConsumed[key], syncedAt: stamp}
		s.quotaBudgets[key] = budget
	}
	return budget
}

func (s *Selector) holdQuotaLocked(key quotaConsumptionKey, units int) func() {
	if s.quotaReserved == nil {
		s.quotaReserved = make(map[quotaConsumptionKey]int)
	}
	s.quotaReserved[key] += units
	var once sync.Once
	return func() {
		once.Do(func() {
			s.quotaMu.Lock()
			s.quotaReserved[key] -= units
			if s.quotaReserved[key] == 0 {
				delete(s.quotaReserved, key)
			}
			s.quotaMu.Unlock()
		})
	}
}

func (s *Service) decrementMediaQuota(ctx context.Context, value account.Provider, accountID uint64, mode string, units int) (bool, error) {
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
