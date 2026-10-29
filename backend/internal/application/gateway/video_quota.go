package gateway

import (
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func videoJobQuotaOptions(job media.Job, upstreamModel string, inputs videoInputs) provider.VideoRequestOptions {
	return provider.VideoRequestOptions{
		Operation: job.Operation, UpstreamModel: upstreamModel, Duration: job.Seconds, Resolution: job.Quality,
		HasImage: inputs.imageURL != "", HasLastFrame: inputs.lastFrameURL != "",
		ReferenceImages: len(inputs.referenceURLs), ReferenceAudios: len(inputs.referenceAudios), ReferenceVideos: len(inputs.referenceVideoURLs),
	}
}

// reserveVideoQuota guards the account picked from an immutable plan against
// concurrent media spending. An over-budget video holds the whole available
// window, not an impossible amount larger than that window. Unknown snapshots
// remain callable under their existing contract.
func (s *Selector) reserveVideoQuota(lease *accountLease, units int) (func(), error) {
	noop := func() {}
	candidate := lease.routingCandidate
	if units <= 0 || candidate == nil || candidate.QuotaWindow == nil || candidate.QuotaWindow.Source != account.QuotaSourceUpstream {
		return noop, nil
	}
	window := candidate.QuotaWindow
	key := quotaConsumptionKey{provider: lease.Credential.Provider, accountID: lease.Credential.ID, mode: s.quotaPoolMode(lease.Credential.Provider, lease.QuotaMode)}
	s.quotaMu.Lock()
	budget := s.quotaBudgetLocked(key, window)
	available := budget.remaining - s.quotaReserved[key]
	// The plan may have been made before another request reserved/consumed the
	// same budget. Retry selection locally instead of provoking an upstream 402.
	if available <= 0 || available < min(units, window.Remaining) {
		s.quotaMu.Unlock()
		return noop, &SelectionUnavailableError{Reason: SelectionQuotaExhausted}
	}
	release := s.holdQuotaLocked(key, min(units, available))
	s.quotaMu.Unlock()
	return release, nil
}
