package chatgpt

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestImageQuotaRejectionBeforeAcceptanceRemainsHTTPError(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		var submits atomic.Int32
		a, assets := newTestAdapter(t, imageUpstream(t, "/backend-api/f/conversation", http.StatusTooManyRequests, &submits))
		response, err := a.GenerateImage(context.Background(), provider.ImageGenerationRequest{Credential: testCredential(t, a, credentials{AccessToken: "AT"}), Model: "gpt-image-2", Prompt: "fox", Streaming: streaming})
		if err != nil || response == nil {
			t.Errorf("stream=%v: response=%+v err=%v", streaming, response, err)
			continue
		}
		response.Body.Close()
		if response.StatusCode != http.StatusTooManyRequests || submits.Load() != 1 || assets.saved != 0 {
			t.Errorf("stream=%v: status=%d submits=%d images=%d", streaming, response.StatusCode, submits.Load(), assets.saved)
		}
	}
}

func TestCodexStreamingQuotaRejectionRemainsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"usage_limit_reached","message":"Usage limit reached"}}`))
	}))
	defer server.Close()
	a, _ := newTestAdapter(t, server)
	response, err := a.GenerateImage(context.Background(), provider.ImageGenerationRequest{Credential: testCredential(t, a, credentials{AccessToken: "AT"}), Model: "codex-gpt-image-2", Prompt: "fox", Streaming: true})
	if err != nil || response == nil {
		t.Fatalf("response=%+v err=%v", response, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests || response.Header.Get("Retry-After") != "120" {
		t.Fatalf("response=%+v", response)
	}
}

func TestQuotaErrorsInConversationEventsUse429(t *testing.T) {
	for _, raw := range []string{
		`{"detail":"You've reached the limit of messages"}`,
		`{"detail":{"code":"model_cap_exceeded","message":"Message limit reached","clears_in":120}}`,
		`{"error":{"code":"usage_limit_reached","message":"Image generation limit reached","retry_after":120}}`,
	} {
		var value any
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			t.Fatal(err)
		}
		_, err := (&openAIChatState{}).event(value)
		if status := provider.ErrorHTTPStatusOrZero(err); status != http.StatusTooManyRequests {
			t.Errorf("chat status=%d err=%v for %s", status, err, raw)
		}
		if err := openAIImageTerminalError(value); provider.ErrorHTTPStatusOrZero(err) != http.StatusTooManyRequests {
			t.Errorf("image err=%v for %s", err, raw)
		}
		if raw != `{"detail":"You've reached the limit of messages"}` && provider.ErrorRetryAfter(err) != 120*time.Second {
			t.Errorf("retry delay=%v for %s", provider.ErrorRetryAfter(err), raw)
		}
	}
}

func TestQuotaClassifierDoesNotMatchGeneratedTextOrContextLimits(t *testing.T) {
	for _, raw := range []string{
		`{"message":{"author":{"role":"assistant"},"content":{"parts":["You've reached the limit of messages"]}}}`,
		`{"error":{"code":"context_length_exceeded","message":"Too many tokens"}}`,
		`{"error":{"code":"content_policy_violation","message":"Generated video rejected by content moderation."}}`,
	} {
		var value any
		_ = json.Unmarshal([]byte(raw), &value)
		if err := quotaRejection(value); err != nil {
			t.Fatalf("not a quota rejection: %s: %v", raw, err)
		}
	}
}

func TestImageQuotaEventDoesNotLeaveSubmittedCheckpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveWebBasics(w, r) {
			return
		}
		switch r.URL.Path {
		case "/backend-api/f/conversation/prepare":
			_, _ = w.Write([]byte(`{"conduit_token":"conduit"}`))
		case "/backend-api/f/conversation":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"error\":{\"code\":\"usage_limit_reached\",\"message\":\"Image generation limit reached\"}}\n\n"))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	a, _ := newTestAdapter(t, server)
	credential := testCredential(t, a, credentials{AccessToken: "AT"})
	cp, err := a.SubmitImage(context.Background(), provider.ImageJobRequest{Credential: credential, Model: "gpt-image-2", Prompt: "fox"}, func(provider.ImageCheckpoint) error { return nil })
	if cp.Submitted || cp.UpstreamID != "" || provider.ErrorHTTPStatusOrZero(err) != http.StatusTooManyRequests {
		t.Fatalf("cp=%+v err=%v", cp, err)
	}
	for _, streaming := range []bool{false, true} {
		response, err := a.GenerateImage(context.Background(), provider.ImageGenerationRequest{Credential: credential, Model: "gpt-image-2", Prompt: "fox", Streaming: streaming})
		if err != nil || response == nil {
			t.Fatalf("stream=%v response=%+v err=%v", streaming, response, err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("stream=%v status=%d", streaming, response.StatusCode)
		}
	}
}

func TestConversationQuotaEventReturns429BeforeStreamingHeaders(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		payloads := make(chan map[string]any, 1)
		server := conversationUpstream(t, payloads, "data: {\"detail\":{\"code\":\"model_cap_exceeded\",\"message\":\"Message limit reached\",\"clears_in\":120}}\n\n", func(map[string]any) int { return 0 })
		a, _ := newTestAdapter(t, server)
		response, err := a.ForwardResponse(context.Background(), provider.ResponseResourceRequest{Credential: testCredential(t, a, credentials{AccessToken: "AT"}), Method: http.MethodPost, Path: "/responses", Model: "gpt-5", Operation: "responses", Streaming: streaming, Body: []byte(`{"model":"gpt-5","input":"hi"}`)})
		if err != nil || response == nil {
			t.Fatalf("stream=%v response=%+v err=%v", streaming, response, err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusTooManyRequests || response.Header.Get("Retry-After") != "120" {
			t.Fatalf("stream=%v response=%+v", streaming, response)
		}
	}
}

func TestAcceptedImageQuotaFailureDoesNotClearCheckpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveWebBasics(w, r) {
			return
		}
		switch r.URL.Path {
		case "/backend-api/f/conversation/prepare":
			_, _ = w.Write([]byte(`{"conduit_token":"conduit"}`))
		case "/backend-api/f/conversation":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"conversation_id\":\"accepted\"}\n\ndata: {\"error\":{\"code\":\"usage_limit_reached\",\"message\":\"Image generation limit reached\"}}\n\n"))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	a, _ := newTestAdapter(t, server)
	cp, err := a.SubmitImage(context.Background(), provider.ImageJobRequest{Credential: testCredential(t, a, credentials{AccessToken: "AT"}), Model: "gpt-image-2", Prompt: "fox"}, func(provider.ImageCheckpoint) error { return nil })
	if !cp.Submitted || cp.UpstreamID != "accepted" || provider.ErrorHTTPStatusOrZero(err) != http.StatusTooManyRequests {
		t.Fatalf("accepted generation must not be resubmitted: cp=%+v err=%v", cp, err)
	}
}
