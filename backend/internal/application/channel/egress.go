package channel

import (
	"context"
	"net/http"

	domain "github.com/chenyme/grok2api/backend/internal/domain/channel"
)

// egressOutcome feeds one upstream outcome back to egress health and reports
// whether the failure belongs to the egress (transport or Cloudflare challenge)
// rather than to the account or the request.
func egressOutcome(ctx context.Context, m domain.Module, err error) bool {
	env := domain.EnvironmentFrom(ctx)
	if err == nil {
		if env.Report != nil {
			env.Report(http.StatusOK, nil)
		}
		return false
	}
	classifier, ok := m.(domain.FailureClassifier)
	if !ok {
		return false
	}
	switch classifier.Classify(err).Code {
	case domain.FailureCodeTransport:
		if env.Report != nil {
			env.Report(0, err)
		}
		return true
	case domain.FailureCodeChallenge:
		if env.InvalidateClearance != nil {
			env.InvalidateClearance()
		}
		if env.Report != nil {
			env.Report(http.StatusForbidden, nil)
		}
		return true
	}
	return false
}
