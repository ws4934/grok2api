package console

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestVideoModerationKeepsRawResponseAndChinesePublicMessage(t *testing.T) {
	body := []byte(`{"error":{"message":"Generated video rejected by content moderation.","code":"invalid_argument"}}`)
	err := newConsoleMediaUpstreamError(http.StatusBadRequest, body, 0)
	message, public := provider.ErrorPublicMessage(err)
	if !public || message != provider.ContentPolicyMessage || !provider.IsRequestScopedError(err) || !strings.Contains(err.Error(), "Generated video rejected") {
		t.Fatalf("error = %v, public = %q", err, message)
	}
	var diagnostic provider.UpstreamDiagnosticError
	if !errors.As(err, &diagnostic) {
		t.Fatal("missing upstream response")
	}
	_, response := diagnostic.UpstreamDiagnostic()
	if string(response.Body) != string(body) || response.StatusCode != 400 {
		t.Fatalf("diagnostic = %+v", response)
	}
}
