package gateway

import (
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

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
	budget := selector.quotaBudgetLocked(key, window)
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
	release := selector.holdQuotaLocked(key, units)
	selector.quotaMu.Unlock()
	return options, release, nil
}
