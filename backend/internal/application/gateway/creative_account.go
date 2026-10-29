package gateway

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
)

type creativeAccountKey struct{}

// WithCreativeAccount is reserved for the authenticated administrator console.
// It bypasses cached account quota/cooldown gates, never client-key permissions,
// credential validity or concurrency limits. Public API input cannot set it.
func WithCreativeAccount(ctx context.Context, accountID uint64) context.Context {
	return context.WithValue(ctx, creativeAccountKey{}, accountID)
}

func creativeAccountID(ctx context.Context) uint64 {
	value, _ := ctx.Value(creativeAccountKey{}).(uint64)
	return value
}

func (s *Selector) creativeCandidate(ctx context.Context, provider account.Provider, accountID, routeID uint64, model, quotaMode string, scope clientkey.AccountScope) (account.RoutingCandidate, error) {
	values, err := s.loadCandidates(ctx, provider, routeID, model, quotaMode, time.Now().UTC(), true)
	if err != nil {
		return account.RoutingCandidate{}, err
	}
	for _, candidate := range values {
		value := candidate.Credential
		if value.ID != accountID {
			continue
		}
		if !value.Enabled || value.AuthStatus != account.AuthStatusActive || !s.accountScopeAllowsCandidate(provider, scope, candidate) {
			return account.RoutingCandidate{}, pinnedUnavailableError(accountID, value.DisplayName())
		}
		return candidate, nil
	}
	return account.RoutingCandidate{}, pinnedUnavailableError(accountID, "")
}

func (s *Selector) acquireCreativeCandidate(ctx context.Context, candidate account.RoutingCandidate, quotaMode string) (*accountLease, error) {
	lease, err := s.acquirePinnedCapacity(ctx, candidate.Credential, nil)
	if err != nil {
		return nil, err
	}
	lease.Billing, lease.routingCandidate = candidate.Billing, &candidate
	lease.QuotaMode = s.effectiveQuotaMode(candidate, quotaMode)
	lease.creativePinned = true
	return lease, nil
}
