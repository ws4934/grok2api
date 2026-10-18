package chatgpt

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// TestVoiceFailureMessageIsShort: the client sees at most 300 characters of
// a voice backend's body, and none of an HTML error page (once 20,133).
func TestVoiceFailureMessageIsShort(t *testing.T) {
	failure := func(contentType, body string) string {
		response := &http.Response{
			StatusCode: http.StatusBadGateway, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body)),
			Request: &http.Request{URL: &url.URL{Scheme: "https", Host: "chatgpt.com", Path: "/backend-api/transcribe"}},
		}
		message, _ := provider.ErrorPublicMessage(voiceError(readOpenAIHTTPError(response, "/backend-api/transcribe")))
		return message
	}
	page := "<!DOCTYPE html><html><head><title>502 Bad Gateway</title></head><body>" + strings.Repeat("x", 20000) + "</body></html>"
	if got := failure("text/html", page); got != "ChatGPT /backend-api/transcribe failed: HTTP 502" {
		t.Fatalf("HTML page: %q", got)
	}
	got := failure("application/json", `{"detail":"`+strings.Repeat("语", 5000)+`"}`)
	detail := strings.TrimPrefix(got, "ChatGPT /backend-api/transcribe failed: HTTP 502 ")
	if !strings.HasPrefix(detail, `{"detail":"语`) || utf8.RuneCountInString(detail) != 300 || !strings.HasSuffix(detail, "…") {
		t.Fatalf("long body: %q", got)
	}
}
