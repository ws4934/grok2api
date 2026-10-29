package gateway

import (
	"net/http"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// Inspect only rejected requests and replay the inspected body, so ordinary
// parameter errors retain their original response and never trigger failover.
func apiKeyRejectedResponse(response *provider.Response, credential account.Credential) *UpstreamFailure {
	if response == nil || response.StatusCode != http.StatusBadRequest {
		return nil
	}
	if response.Diagnostic != nil {
		failure := newHTTPUpstreamFailure(response.StatusCode, response.Diagnostic.Body, credential.ID, credential.DisplayName())
		if failure.APIKeyRejected {
			return failure
		}
	}
	body, replay, _, err := readResponseBody(response.Body)
	response.Body = replay
	if err != nil {
		return nil
	}
	failure := newHTTPUpstreamFailure(response.StatusCode, body, credential.ID, credential.DisplayName())
	if !failure.APIKeyRejected {
		return nil
	}
	return failure
}
