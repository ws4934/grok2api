package chatgpt

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestImageModerationIsPublicChineseRequestFailureWithRawDiagnostics(t *testing.T) {
	message := "We’re so sorry, but the image we created may violate our content policies. If you think we got it wrong, please retry or edit your prompt."
	err := openAIImageTerminalError(map[string]any{"error": map[string]any{"message": message}})
	if provider.ErrorHTTPStatusOrZero(err) != http.StatusUnprocessableEntity || !provider.IsRequestScopedError(err) {
		t.Fatalf("error = %v", err)
	}
	if public, ok := provider.ErrorPublicMessage(err); !ok || public != provider.ContentPolicyMessage {
		t.Fatalf("public = %q, %v", public, ok)
	}
	var diagnostic provider.UpstreamDiagnosticError
	if !errors.As(err, &diagnostic) {
		t.Fatal("missing diagnostic")
	}
	_, response := diagnostic.UpstreamDiagnostic()
	if response.StatusCode != 422 || !strings.Contains(string(response.Body), message) || !strings.Contains(err.Error(), "content policies") {
		t.Fatalf("diagnostic = %+v, error = %v", response, err)
	}
}
