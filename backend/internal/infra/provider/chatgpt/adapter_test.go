package chatgpt

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type egressStub struct{}

func (egressStub) ListEgressNodes(context.Context, egressdomain.Scope, repository.SortQuery) ([]egressdomain.Node, error) {
	return nil, nil
}
func (egressStub) GetEgressNode(context.Context, uint64) (egressdomain.Node, error) {
	return egressdomain.Node{}, errors.New("not found")
}
func (egressStub) CreateEgressNode(context.Context, egressdomain.Node) (egressdomain.Node, error) {
	return egressdomain.Node{}, errors.New("unsupported")
}
func (egressStub) UpdateEgressNode(context.Context, egressdomain.Node) (egressdomain.Node, error) {
	return egressdomain.Node{}, errors.New("unsupported")
}
func (egressStub) DeleteEgressNode(context.Context, uint64) error { return errors.New("unsupported") }

type assetStub struct {
	mu    sync.Mutex
	saved int
}

func (s *assetStub) SaveImage(_ context.Context, data []byte) (media.Asset, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saved++
	return media.Asset{ID: fmt.Sprintf("asset-%d", s.saved), MIMEType: http.DetectContentType(data)}, nil
}
func (s *assetStub) PublicImageURL(id string) string { return "https://media.example/" + id }

func newTestAdapter(t *testing.T, server *httptest.Server) (*Adapter, *assetStub) {
	t.Helper()
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	assets := &assetStub{}
	a := NewAdapter(Config{ImageConversationModel: "gpt-5", OutputTokenLimit: 16384, VisionTokens: 256}, infraegress.NewManager(egressStub{}, cipher), cipher, assets)
	a.image.BaseURL, a.image.Client, a.oauth = server.URL, server.Client(), server.Client()
	a.authURL, a.tokenURL = server.URL+"/authorize", server.URL+"/token"
	t.Cleanup(func() { _ = a.Close() })
	return a, assets
}

func testCredential(t *testing.T, a *Adapter, c credentials) account.Credential {
	t.Helper()
	accessToken, err := a.cipher.Encrypt(c.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	refreshToken, err := a.cipher.Encrypt(c.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	state, err := a.encryptState(c)
	if err != nil {
		t.Fatal(err)
	}
	return account.Credential{ID: 7, Provider: account.ProviderChatGPT, AuthType: account.AuthTypeOAuth, EncryptedAccessToken: accessToken, EncryptedRefreshToken: refreshToken, EncryptedState: state}
}

func testJWT(claims map[string]any) string {
	payload, _ := json.Marshal(claims)
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// serveWebBasics answers the bootstrap and Sentinel endpoints of every Web call.
func serveWebBasics(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/":
		_, _ = w.Write([]byte(`<html data-build="fixture"></html>`))
	case "/backend-api/sentinel/chat-requirements/prepare":
		_, _ = w.Write([]byte(`{"prepare_token":"prepare"}`))
	case "/backend-api/sentinel/chat-requirements/finalize":
		_, _ = w.Write([]byte(`{"token":"sentinel"}`))
	default:
		return false
	}
	return true
}

const replyStream = `data: {"message":{"author":{"role":"assistant"},"content":{"content_type":"thoughts","thoughts":[{"summary":"Plan"}]}}}

data: {"message":{"author":{"role":"assistant"},"channel":"final","content":{"content_type":"text","parts":["Hello"]}}}

data: {"p":"/message/content/parts/0","o":"append","v":" world"}

data: [DONE]

`

// chatUpstream serves replyStream and records each conversation payload.
func chatUpstream(t *testing.T, payloads chan<- map[string]any) *httptest.Server {
	return answerUpstream(t, payloads, replyStream)
}

// answerUpstream serves stream as every conversation reply.
func answerUpstream(t *testing.T, payloads chan<- map[string]any, stream string) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveWebBasics(w, r) {
			return
		}
		if r.URL.Path != "/backend-api/conversation" {
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if payloads != nil {
			payloads <- payload
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(stream))
	}))
	t.Cleanup(server.Close)
	return server
}

func forward(t *testing.T, a *Adapter, credential account.Credential, operation, body string, stream bool) *provider.Response {
	t.Helper()
	response, err := a.ForwardResponse(context.Background(), provider.ResponseResourceRequest{Credential: credential, Method: http.MethodPost, Path: "/responses", Model: "gpt-5", Body: []byte(body), Streaming: stream, NormalizeBody: true, Operation: operation})
	if err != nil {
		t.Fatal(err)
	}
	return response
}

// events decodes the data lines of an SSE body.
func events(t *testing.T, body io.ReadCloser) []map[string]any {
	t.Helper()
	defer body.Close()
	var out []map[string]any
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	for scanner.Scan() {
		line, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok || line == "[DONE]" {
			continue
		}
		var value map[string]any
		if err := json.Unmarshal([]byte(line), &value); err != nil {
			t.Fatalf("event %q: %v", line, err)
		}
		out = append(out, value)
	}
	return out
}

func decodeBody(t *testing.T, response *provider.Response) map[string]any {
	t.Helper()
	defer response.Body.Close()
	var value map[string]any
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestResponsesStreamFollowsOfficialEventSequence(t *testing.T) {
	payloads := make(chan map[string]any, 1)
	a, _ := newTestAdapter(t, chatUpstream(t, payloads))
	credential := testCredential(t, a, credentials{AccessToken: "AT"})
	metadata := &provider.NormalizedRequestMetadata{}
	response, err := a.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Credential: credential, Method: http.MethodPost, Path: "/responses", Model: "gpt-5", Streaming: true, NormalizeBody: true, Operation: "responses", NormalizedMetadata: metadata,
		Body: []byte(`{"model":"gpt-5","stream":true,"temperature":0.3,"reasoning":{"effort":"xhigh"},"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`),
	})
	if err != nil || response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("response = %+v, %v", response, err)
	}
	var types []string
	var completed map[string]any
	text := ""
	for index, event := range events(t, response.Body) {
		types = append(types, event["type"].(string))
		if event["sequence_number"] != float64(index) {
			t.Fatalf("sequence_number %v at %d", event["sequence_number"], index)
		}
		if event["type"] == "response.output_text.delta" {
			text += event["delta"].(string)
		}
		if event["type"] == "response.completed" {
			completed = event["response"].(map[string]any)
		}
	}
	want := []string{
		"response.created", "response.in_progress",
		"response.output_item.added", "response.reasoning_summary_part.added", "response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done", "response.reasoning_summary_part.done", "response.output_item.done",
		"response.output_item.added", "response.content_part.added", "response.output_text.delta", "response.output_text.delta",
		"response.output_text.done", "response.content_part.done", "response.output_item.done", "response.completed",
	}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v", types)
	}
	if text != "Hello world" || completed["status"] != "completed" {
		t.Fatalf("text %q, completed %v", text, completed)
	}
	usage := completed["usage"].(map[string]any)
	reasoning := usage["output_tokens_details"].(map[string]any)["reasoning_tokens"].(float64)
	if usage["input_tokens"].(float64) < 8 || reasoning < 1 || usage["output_tokens"].(float64) <= reasoning {
		t.Fatalf("usage = %v", usage)
	}
	if payload := <-payloads; payload["thinking_effort"] != "extended" || metadata.ReasoningEffort != "extended" {
		t.Fatalf("effort payload %v metadata %q", payload["thinking_effort"], metadata.ReasoningEffort)
	}
}

func TestResponsesJSONAndConvertedProtocols(t *testing.T) {
	a, _ := newTestAdapter(t, chatUpstream(t, nil))
	credential := testCredential(t, a, credentials{AccessToken: "AT"})

	object := decodeBody(t, forward(t, a, credential, "responses", `{"model":"gpt-5","input":"hi"}`, false))
	output := object["output"].([]any)
	message := output[len(output)-1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if object["object"] != "response" || object["status"] != "completed" || output[0].(map[string]any)["type"] != "reasoning" || message["text"] != "Hello world" || object["usage"] == nil {
		t.Fatalf("responses object = %v", object)
	}

	chat := decodeBody(t, forward(t, a, credential, "chat", `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`, false))
	choice := chat["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if chat["object"] != "chat.completion" || choice["content"] != "Hello world" {
		t.Fatalf("chat completion = %v", chat)
	}
	if usage := chat["usage"].(map[string]any); usage["prompt_tokens"] == nil || usage["cost_in_usd_ticks"] != nil || usage["context_details"] != nil || usage["num_sources_used"] != nil {
		t.Fatalf("chat usage carries xAI extensions: %v", usage)
	}

	chunks := events(t, forward(t, a, credential, "chat", `{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`, true).Body)
	text := ""
	for _, chunk := range chunks {
		choices, _ := chunk["choices"].([]any)
		for _, raw := range choices {
			delta, _ := raw.(map[string]any)["delta"].(map[string]any)
			content, _ := delta["content"].(string)
			text += content
		}
	}
	if text != "Hello world" {
		t.Fatalf("chat stream text = %q in %v", text, chunks)
	}

	stream := events(t, forward(t, a, credential, "messages", `{"model":"gpt-5","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, true).Body)
	text = ""
	for _, event := range stream {
		if delta, ok := event["delta"].(map[string]any); ok && delta["type"] == "text_delta" {
			text += delta["text"].(string)
		}
	}
	if stream[0]["type"] != "message_start" || stream[len(stream)-1]["type"] != "message_stop" || text != "Hello world" {
		t.Fatalf("messages stream = %v", stream)
	}
	for _, event := range stream {
		if usage, ok := event["usage"].(map[string]any); ok && (usage["cost_in_usd_ticks"] != nil || usage["context_details"] != nil) {
			t.Fatalf("messages usage carries xAI extensions: %v", usage)
		}
	}
}

func TestOutputLimitAndInputEstimate(t *testing.T) {
	a, _ := newTestAdapter(t, chatUpstream(t, nil))
	credential := testCredential(t, a, credentials{AccessToken: "AT"})
	object := decodeBody(t, forward(t, a, credential, "responses", `{"model":"gpt-5","max_output_tokens":2,"input":"hi"}`, false))
	usage := object["usage"].(map[string]any)
	details, _ := object["incomplete_details"].(map[string]any)
	if object["status"] != "incomplete" || details["reason"] != "max_output_tokens" || usage["output_tokens"].(float64) > 2 {
		t.Fatalf("truncated object = %v", object)
	}

	text, _ := countTokens("hello")
	got, err := estimateInput([]turnMessage{{Role: "user", Parts: []turnPart{{Text: "hello"}, {Image: "data:image/png;base64,AA=="}}}}, 256)
	if err != nil || got != 3+4+text+256 {
		t.Fatalf("estimate = %d, %v", got, err)
	}
}

// The input count is taken after the reply; it still covers the whole parsed turn.
func TestUsageInputTokensCountParsedTurn(t *testing.T) {
	a, _ := newTestAdapter(t, chatUpstream(t, nil))
	credential := testCredential(t, a, credentials{AccessToken: "AT"})
	body := `{"model":"gpt-5","instructions":"Be brief.","input":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"output_text","text":"Hello there"}]},{"role":"user","content":[{"type":"input_text","text":"And again?"}]}]}`
	in, err := parseTurn([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	want, err := estimateInput(in.Messages, 256)
	if err != nil {
		t.Fatal(err)
	}
	object := decodeBody(t, forward(t, a, credential, "responses", body, false))
	if got := object["usage"].(map[string]any)["input_tokens"]; got != float64(want) {
		t.Fatalf("JSON input_tokens = %v, want %d", got, want)
	}
	var got any
	for _, event := range events(t, forward(t, a, credential, "responses", body, true).Body) {
		if event["type"] == "response.completed" {
			got = event["response"].(map[string]any)["usage"].(map[string]any)["input_tokens"]
		}
	}
	if got != float64(want) {
		t.Fatalf("stream input_tokens = %v, want %d", got, want)
	}
}

func TestToolsThatCannotRunDegradeToTextWithNotice(t *testing.T) {
	payloads := make(chan map[string]any, 1)
	a, _ := newTestAdapter(t, chatUpstream(t, payloads))
	credential := testCredential(t, a, credentials{AccessToken: "AT"})
	body := `{"model":"gpt-5","tools":[{"type":"code_interpreter","container":{"type":"auto"}}],"input":[
		{"role":"user","content":"find it"},
		{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"found"},
		{"type":"reasoning","summary":[]}]}`
	_ = decodeBody(t, forward(t, a, credential, "responses", body, false))
	messages := (<-payloads)["messages"].([]any)
	parts := func(i int) []any {
		return messages[i].(map[string]any)["content"].(map[string]any)["parts"].([]any)
	}
	if role := messages[0].(map[string]any)["author"].(map[string]any)["role"]; role != "system" || parts(0)[0] != plainMarkdown+"\n\n"+toolsUnavailable {
		t.Fatalf("first message = %v", messages[0])
	}
	if len(messages) != 4 || !strings.Contains(parts(2)[0].(string), "Tool call lookup (c1): {}") || parts(3)[1] != "found" {
		t.Fatalf("tool history = %v", messages)
	}
}

func TestFailuresBeforeOutputKeepUpstreamStatus(t *testing.T) {
	tests := []struct {
		name, operation string
		status          int
		header          http.Header
		body            string
		check           func(*testing.T, *provider.Response)
	}{
		{name: "unauthorized", operation: "responses", status: 401, body: `{"detail":"expired"}`, check: func(t *testing.T, r *provider.Response) {
			if r.StatusCode != 401 || decodeBody(t, r)["error"].(map[string]any)["type"] != "authentication_error" {
				t.Fatalf("401 = %+v", r)
			}
		}},
		{name: "rate limited", operation: "messages", status: 429, header: http.Header{"Retry-After": {"30"}}, body: `{}`, check: func(t *testing.T, r *provider.Response) {
			body := decodeBody(t, r)
			if r.StatusCode != 429 || r.Header.Get("Retry-After") != "30" || body["type"] != "error" || body["error"].(map[string]any)["type"] != "rate_limit_error" {
				t.Fatalf("429 = %+v %v", r, body)
			}
		}},
		{name: "model rejected", operation: "chat", status: 404, body: `{"detail":"Model not found"}`, check: func(t *testing.T, r *provider.Response) {
			if r.StatusCode != 404 || !r.ModelCatalogChanged {
				t.Fatalf("404 = %+v", r)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if serveWebBasics(w, r) {
					return
				}
				for key, values := range test.header {
					w.Header()[key] = values
				}
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			t.Cleanup(server.Close)
			a, _ := newTestAdapter(t, server)
			body := `{"model":"gpt-5","input":"hi"}`
			if test.operation == "chat" {
				body = `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`
			} else if test.operation == "messages" {
				body = `{"model":"gpt-5","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
			}
			test.check(t, forward(t, a, testCredential(t, a, credentials{AccessToken: "AT"}), test.operation, body, true))
		})
	}
}

func dropConnection(w http.ResponseWriter) {
	conn, _, _ := w.(http.Hijacker).Hijack()
	_ = conn.Close()
}

func TestEgressFailureIsRetriedOnceBeforeSend(t *testing.T) {
	var bootstraps, conversations atomic.Int32
	var challenge, dropConversation atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/" && challenge.Load():
			bootstraps.Add(1)
			w.Header().Set("cf-mitigated", "challenge")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("<title>Just a moment...</title>"))
			return
		case r.URL.Path == "/" && bootstraps.Add(1) == 1:
			dropConnection(w)
			return
		case r.URL.Path == "/backend-api/conversation":
			if conversations.Add(1); dropConversation.Load() {
				dropConnection(w)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(replyStream))
			return
		}
		serveWebBasics(w, r)
	}))
	t.Cleanup(server.Close)
	a, _ := newTestAdapter(t, server)
	credential := testCredential(t, a, credentials{AccessToken: "AT"})
	if response := forward(t, a, credential, "responses", `{"model":"gpt-5","input":"hi"}`, false); response.StatusCode != 200 || bootstraps.Load() != 2 {
		t.Fatalf("transport retry: status %d, bootstraps %d", response.StatusCode, bootstraps.Load())
	}

	challenge.Store(true)
	bootstraps.Store(0)
	response := forward(t, a, credential, "responses", `{"model":"gpt-5","input":"hi"}`, true)
	if response.StatusCode != 403 || decodeBody(t, response)["error"].(map[string]any)["code"] != "cloudflare_challenge" || bootstraps.Load() != 2 {
		t.Fatalf("challenge: status %d, bootstraps %d", response.StatusCode, bootstraps.Load())
	}

	challenge.Store(false)
	dropConversation.Store(true)
	bootstraps.Store(1)
	conversations.Store(0)
	_, err := a.ForwardResponse(context.Background(), provider.ResponseResourceRequest{Credential: credential, Method: http.MethodPost, Path: "/responses", Model: "gpt-5", Body: []byte(`{"input":"hi"}`), Operation: "responses"})
	var transport *transportError
	if !errors.As(err, &transport) || conversations.Load() != 1 {
		t.Fatalf("sent request was retried or not an error: %v, conversations %d", err, conversations.Load())
	}
}

func TestRefreshRotatesTokensAndState(t *testing.T) {
	var calls atomic.Int32
	var rejected atomic.Bool
	expiry := time.Now().Add(time.Hour).Unix()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["client_id"] != oauthClientID || body["grant_type"] != "refresh_token" || body["refresh_token"] != "RT" {
			t.Errorf("refresh body = %v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		if rejected.Load() {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"revoked"}`))
			return
		}
		_, _ = fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"RT2","id_token":"ID2"}`, testJWT(map[string]any{"exp": expiry}))
	}))
	t.Cleanup(server.Close)
	a, _ := newTestAdapter(t, server)
	credential := testCredential(t, a, credentials{AccessToken: "AT", RefreshToken: "RT", IDToken: "ID1", ClientProfile: oauthProfile, DeviceID: "device"})

	refreshed, err := a.RefreshCredential(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	next, err := a.decrypt(account.Credential{EncryptedAccessToken: refreshed.EncryptedAccessToken, EncryptedRefreshToken: refreshed.EncryptedRefreshToken, EncryptedState: refreshed.EncryptedState})
	if err != nil || next.RefreshToken != "RT2" || next.IDToken != "ID2" || next.DeviceID != "device" || next.ClientProfile != oauthProfile || !refreshed.RefreshTokenRotated || refreshed.ExpiresAt.Unix() != expiry {
		t.Fatalf("refreshed = %+v %+v %v", refreshed, next, err)
	}

	rejected.Store(true)
	_, err = a.RefreshCredential(context.Background(), credential)
	var refreshErr *provider.CredentialRefreshError
	if !errors.As(err, &refreshErr) || refreshErr.Code != "invalid_grant" || !refreshErr.Permanent || refreshErr.Status != 400 {
		t.Fatalf("invalid_grant = %v", err)
	}

	calls.Store(0)
	foreign := testCredential(t, a, credentials{AccessToken: "AT", RefreshToken: "RT", ClientProfile: "codex"})
	if _, err := a.RefreshCredential(context.Background(), foreign); !errors.As(err, &refreshErr) || !refreshErr.Permanent || calls.Load() != 0 {
		t.Fatalf("foreign client profile refreshed: %v, calls %d", err, calls.Load())
	}
}

// accountUpstream serves the OAuth token endpoint and the identity endpoints.
func accountUpstream(t *testing.T, tokenCalls *atomic.Int32) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			tokenCalls.Add(1)
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["grant_type"] == "authorization_code" && (body["code_verifier"] == "" || body["code"] != "abc") {
				t.Errorf("code exchange body = %v", body)
			}
			_, _ = w.Write([]byte(`{"access_token":"fresh-AT","refresh_token":"fresh-RT","id_token":"fresh-ID","expires_in":3600}`))
		case "/backend-api/me":
			_, _ = w.Write([]byte(`{"id":"user-1","email":"a@example.com"}`))
		case "/backend-api/accounts/check/v4-2023-04-27":
			_, _ = w.Write([]byte(`{"accounts":{"default":{"account":{"account_id":"ws-1","plan_type":"plus"}}}}`))
		case "/backend-api/models":
			_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5"},{"slug":"gpt-5-thinking"}]}`))
		case "/backend-api/conversation/init":
			_, _ = w.Write([]byte(`{"limits_progress":[{"feature_name":"reason","remaining":0,"reset_after":"2030-01-02T03:04:05.302546+00:00"},{"feature_name":"image_gen","remaining":5},{"feature_name":"deep_research","remaining":1}]}`))
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestInteractivePKCELogin(t *testing.T) {
	var tokenCalls atomic.Int32
	a, _ := newTestAdapter(t, accountUpstream(t, &tokenCalls))
	ctx := context.Background()
	challenge, err := a.StartAuthorization(ctx)
	if err != nil || challenge.Kind != provider.AuthorizationRedirect || !strings.Contains(challenge.AuthorizeURL, "code_challenge_method=S256") || challenge.ExpiresIn != 10*time.Minute {
		t.Fatalf("challenge = %+v, %v", challenge, err)
	}
	var session oauthSession
	_ = json.Unmarshal([]byte(challenge.State), &session)
	if session.Verifier == "" || strings.Contains(challenge.AuthorizeURL, session.Verifier) {
		t.Fatal("verifier missing or leaked into the authorize URL")
	}
	for _, callback := range []string{"bare-code", oauthRedirect + "?code=abc", oauthRedirect + "?state=wrong&code=abc", "https://attacker.example/auth/callback?state=" + session.State + "&code=abc", oauthRedirect + "?state=" + session.State + "&state=" + session.State + "&code=abc"} {
		if _, err := a.CompleteAuthorization(ctx, challenge, callback); err == nil {
			t.Fatalf("invalid callback accepted: %s", callback)
		}
	}
	for _, callback := range []string{oauthRedirect + "?state=wrong&code=abc", oauthRedirect + "?state=" + session.State + "&error=access_denied"} {
		if _, err := a.CompleteAuthorization(ctx, challenge, callback); !errors.Is(err, provider.ErrAuthorizationDenied) {
			t.Fatalf("%s: %v", callback, err)
		}
	}
	if tokenCalls.Load() != 0 {
		t.Fatal("invalid callback reached the token endpoint")
	}
	seed, err := a.CompleteAuthorization(ctx, challenge, oauthRedirect+"?state="+session.State+"&code=abc")
	if err != nil || seed.AccessToken != "fresh-AT" || seed.RefreshToken != "fresh-RT" || seed.UserID != "user-1" || seed.TeamID != "ws-1" || seed.Plan != "plus" || seed.Name != "a@example.com" || seed.Provider != account.ProviderChatGPT {
		t.Fatalf("seed = %+v, %v", seed, err)
	}
	if seed.State["client_profile"] != oauthProfile || seed.State["id_token"] != "fresh-ID" || seed.State["device_id"] == "" || seed.ExpiresAt.Before(time.Now().Add(50*time.Minute)) {
		t.Fatalf("seed state = %+v", seed)
	}
}

func TestAccountIdentityModelsAndQuotaWindows(t *testing.T) {
	var tokenCalls atomic.Int32
	a, _ := newTestAdapter(t, accountUpstream(t, &tokenCalls))
	credential := testCredential(t, a, credentials{AccessToken: "AT"})
	ctx := context.Background()
	identity, err := a.SyncAccountIdentity(ctx, credential)
	if err != nil || identity != (provider.AccountIdentity{UserID: "user-1", Email: "a@example.com", TeamID: "ws-1", Plan: "plus"}) {
		t.Fatalf("identity = %+v, %v", identity, err)
	}
	models, err := a.ListModels(ctx, credential)
	if err != nil || strings.Join(models, ",") != "gpt-5,gpt-5-thinking" {
		t.Fatalf("models = %v, %v", models, err)
	}
	if got := a.NormalizeAccountModelCapabilities(models, nil, credential); strings.Join(got[len(models):], ",") != "chatgpt-tts,chatgpt-transcribe,gpt-image-2,gpt-image-2.5,gpt-image-2.5-flare,gpt-image-2.5-sunburst" {
		t.Fatalf("image routes not added: %v", got)
	}
	if got := a.NormalizeAccountModelCapabilities([]string{"gpt-4o"}, nil, credential); strings.Join(got, ",") != "gpt-4o,chatgpt-tts,chatgpt-transcribe" {
		t.Fatalf("image route added without the conversation model: %v", got)
	}
	if a.QuotaMode("gpt-5") != quotaModeReason || a.QuotaMode("gpt-image-2") != quotaModeImage {
		t.Fatal("quota modes")
	}
	snapshot, err := a.SyncQuota(ctx, credential)
	if err != nil || len(snapshot.Windows) != 2 {
		t.Fatalf("snapshot = %+v, %v", snapshot, err)
	}
	reason := snapshot.Windows[0]
	if reason.Mode != quotaModeReason || reason.Remaining != 0 || reason.UsagePercent != 100 || reason.ResetAt == nil || !reason.ResetAt.Equal(time.Date(2030, 1, 2, 3, 4, 5, 302546000, time.UTC)) || reason.AccountID != 7 || snapshot.Windows[1].Remaining != 5 {
		t.Fatalf("windows = %+v", snapshot.Windows)
	}
	if _, err := a.SyncQuotaMode(ctx, credential, quotaModeUpload); err == nil {
		t.Fatal("unreported file_upload limit produced a window")
	}
}

func TestImportFormatsAndPreparation(t *testing.T) {
	var tokenCalls atomic.Int32
	a, _ := newTestAdapter(t, accountUpstream(t, &tokenCalls))
	token := testJWT(map[string]any{"exp": float64(time.Now().Add(time.Hour).Unix()), "https://api.openai.com/auth": map[string]any{"chatgpt_user_id": "user-9", "chatgpt_account_id": "ws-9", "chatgpt_plan_type": "pro"}, "https://api.openai.com/profile": map[string]any{"email": "z@example.com"}})
	seeds, err := a.ParseImportedCredentials([]byte(`[{"access_token":"` + token + `","browser_profile":"` + "" + `"},{"refresh_token":"RT","client_profile":"platform_pkce_v1"},{"name":"old","credentials":{"version":1,"access_token":"AT2","device_id":"d2"}}]`))
	if err != nil || len(seeds) != 3 {
		t.Fatalf("seeds = %+v, %v", seeds, err)
	}
	if s := seeds[0]; s.Provider != account.ProviderChatGPT || s.UserID != "user-9" || s.TeamID != "ws-9" || s.Plan != "pro" || s.Name != "z@example.com" || s.ExpiresAt.IsZero() || s.State["device_id"] == "" || s.State["session_id"] == "" {
		t.Fatalf("claims seed = %+v", s)
	}
	if seeds[2].Name != "old" || seeds[2].State["device_id"] != "d2" || seeds[0].SourceKey == seeds[1].SourceKey {
		t.Fatalf("channel export seed = %+v", seeds[2])
	}
	if _, err := a.ParseImportedCredentials([]byte("{\"access_token\":\"AT\"}\n{\"name\":\"empty\"}")); err == nil || !strings.Contains(err.Error(), "record 2") {
		t.Fatalf("record error = %v", err)
	}
	exported, err := a.MarshalCredentials(seeds)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := a.ParseImportedCredentials(exported); err != nil || again[2].State["device_id"] != "d2" || again[1].State["client_profile"] != oauthProfile {
		t.Fatalf("export round trip = %+v, %v", again, err)
	}

	prepared, err := a.PrepareImportedCredential(context.Background(), seeds[1])
	if err != nil || prepared.AccessToken != "fresh-AT" || prepared.RefreshToken != "fresh-RT" || prepared.UserID != "user-1" || prepared.Plan != "plus" || prepared.Name != "a@example.com" || tokenCalls.Load() != 1 {
		t.Fatalf("prepared = %+v, %v", prepared, err)
	}
	seeds[1].State["client_profile"] = ""
	if _, err := a.PrepareImportedCredential(context.Background(), seeds[1]); err == nil || tokenCalls.Load() != 1 {
		t.Fatalf("refresh token without client profile was exchanged: %v", err)
	}
}

func testPNG() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for y := 0; y < 48; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.RGBA{R: 200, A: 255})
		}
	}
	var out bytes.Buffer
	_ = png.Encode(&out, img)
	return out.Bytes()
}

// imageUpstream serves the image conversation; status, when set, rejects the
// endpoint named by path.
func imageUpstream(t *testing.T, path string, status int, submits *atomic.Int32) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == path {
			if r.URL.Path == "/backend-api/f/conversation" {
				submits.Add(1)
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		if serveWebBasics(w, r) {
			return
		}
		switch r.URL.Path {
		case "/backend-api/f/conversation/prepare":
			_, _ = w.Write([]byte(`{"conduit_token":"conduit"}`))
		case "/backend-api/f/conversation":
			submits.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"conversation_id\":\"conv1\"}\n\ndata: [DONE]\n\n"))
		case "/backend-api/conversation/conv1":
			_, _ = w.Write([]byte(`{"conversation_id":"conv1","mapping":{"m1":{"message":{"author":{"role":"tool"},"create_time":1,"content":{"content_type":"multimodal_text","parts":[{"content_type":"image_asset_pointer","asset_pointer":"file-service://file_abc123456"}]}}}}}`))
		case "/backend-api/files/file_abc123456/download":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(testPNG())
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestImageGenerationSyncAndStream(t *testing.T) {
	var submits atomic.Int32
	a, assets := newTestAdapter(t, imageUpstream(t, "", 0, &submits))
	credential := testCredential(t, a, credentials{AccessToken: "AT"})
	ctx := context.Background()

	response, err := a.GenerateImage(ctx, provider.ImageGenerationRequest{Credential: credential, Model: "gpt-image-2", Prompt: "a fox", Count: 2, Quality: "high"})
	if err != nil {
		t.Fatal(err)
	}
	body := decodeBody(t, response)
	data, _ := body["data"].([]any)
	if response.StatusCode != 200 || response.QuotaUnits != 2 || len(data) != 2 || data[0].(map[string]any)["b64_json"] == nil || body["size"] != "64x48" || body["quality"] != "high" || body["output_format"] != "png" || submits.Load() != 2 || assets.saved != 2 {
		t.Fatalf("images = %+v %v (submits %d)", response, body, submits.Load())
	}
	if usage := body["usage"].(map[string]any); usage["output_tokens"].(float64) != 2*float64(outputImageTokens(64, 48, "", "high")) || usage["input_tokens"].(float64) < 1 {
		t.Fatalf("usage = %v", usage)
	}

	response, err = a.GenerateImage(ctx, provider.ImageGenerationRequest{Credential: credential, Model: "gpt-image-2", Prompt: "a fox", ResponseFormat: "url", OutputFormat: "jpeg"})
	if err != nil {
		t.Fatal(err)
	}
	if item := decodeBody(t, response)["data"].([]any)[0].(map[string]any); item["url"] != "https://media.example/asset-3" {
		t.Fatalf("url item = %v", item)
	}

	imageKeepAlive = 10 * time.Millisecond
	t.Cleanup(func() { imageKeepAlive = 15 * time.Second })
	response, err = a.GenerateImage(ctx, provider.ImageGenerationRequest{Credential: credential, Model: "gpt-image-2", Prompt: "a fox", Streaming: true})
	if err != nil || response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream = %+v, %v", response, err)
	}
	stream := events(t, response.Body)
	if len(stream) != 1 || stream[0]["type"] != "image_generation.completed" || stream[0]["b64_json"] == nil || stream[0]["usage"] == nil {
		t.Fatalf("stream events = %v", stream)
	}
}

func TestImageFailuresBeforeAndAfterSubmit(t *testing.T) {
	var submits atomic.Int32
	a, _ := newTestAdapter(t, imageUpstream(t, "/backend-api/f/conversation/prepare", http.StatusTooManyRequests, &submits))
	credential := testCredential(t, a, credentials{AccessToken: "AT"})
	if response, err := a.GenerateImage(context.Background(), provider.ImageGenerationRequest{Credential: credential, Model: "gpt-image-2", Prompt: "a fox"}); err != nil || response.StatusCode != 429 {
		t.Fatalf("pre-submit rate limit = %+v, %v", response, err)
	}

	b, _ := newTestAdapter(t, imageUpstream(t, "/backend-api/f/conversation", http.StatusBadGateway, &submits))
	credential = testCredential(t, b, credentials{AccessToken: "AT"})
	if response, err := b.GenerateImage(context.Background(), provider.ImageGenerationRequest{Credential: credential, Model: "gpt-image-2", Prompt: "a fox"}); err == nil || response != nil || submits.Load() != 1 {
		t.Fatalf("post-submit failure = %+v, %v (submits %d)", response, err, submits.Load())
	}
	response, err := b.GenerateImage(context.Background(), provider.ImageGenerationRequest{Credential: credential, Model: "gpt-image-2", Prompt: "a fox", Streaming: true})
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("submitted stream = %+v, %v", response, err)
	}
	if stream := events(t, response.Body); len(stream) != 1 || stream[0]["type"] != "error" || submits.Load() != 2 {
		t.Fatalf("submitted stream failure = %v (submits %d)", stream, submits.Load())
	}

	if response, err := b.GenerateImage(context.Background(), provider.ImageGenerationRequest{Credential: credential, Model: "gpt-6", Prompt: "a fox"}); err != nil || response.StatusCode != 404 {
		t.Fatalf("unknown image model = %+v, %v", response, err)
	}
}

func TestRegistryAcceptsChatGPT(t *testing.T) {
	a := NewAdapter(Config{}, nil, nil, nil)
	registry := provider.NewRegistry(a)
	if err := registry.Validate(); err != nil {
		t.Fatal(err)
	}
	id := account.ProviderChatGPT
	_, interactive := registry.InteractiveAuth(id)
	_, identity := registry.AccountIdentity(id)
	_, validator := registry.ImageRequestValidator(id)
	_, preparer := any(a).(provider.CredentialImportPreparer)
	_, normalizer := any(a).(provider.AccountModelCapabilityNormalizer)
	if !interactive || !identity || !validator || !preparer || !normalizer || !registry.TierExempt(id) || registry.QuotaMode(id, "gpt-image-2") != quotaModeImage {
		t.Fatal("ChatGPT capability surface is incomplete")
	}
}

func TestValidateImageRequest(t *testing.T) {
	a := &Adapter{}
	valid := []provider.ImageRequestOptions{
		{}, {Count: 10, Size: "1536x1024", Quality: "high", ResponseFormat: "url", OutputFormat: "webp", Background: "transparent", Streaming: true},
		{Edit: true, InputImages: 1, HasMask: true, Quality: "auto", Size: "auto"}, {Edit: true, InputImages: 8, PartialImages: 3},
	}
	for _, o := range valid {
		if err := a.ValidateImageRequest(o); err != nil {
			t.Fatalf("%+v rejected: %v", o, err)
		}
	}
	invalid := []provider.ImageRequestOptions{
		{Count: 11}, {Size: "1024"}, {Size: "100x100"}, {Quality: "hd"}, {ResponseFormat: "base64"}, {AspectRatio: "16:9"},
		{Edit: true}, {Edit: true, InputImages: 9}, {Edit: true, InputImages: 2, HasMask: true}, {HasMask: true},
		{OutputFormat: "gif"}, {Background: "transparent", OutputFormat: "jpeg"}, {PartialImages: 4},
	}
	for _, o := range invalid {
		var scoped provider.RequestScopedError
		if err := a.ValidateImageRequest(o); !errors.As(err, &scoped) {
			t.Fatalf("%+v accepted: %v", o, err)
		}
	}
}

func TestImageRoutesFollowTheImageToolOfEachAccount(t *testing.T) {
	a := NewAdapter(Config{}, nil, nil, nil)
	// A free catalog: every model has the image tool, but there is no gpt-5-3.
	a.rememberImageCatalog(1, map[string]bool{"auto": true, "gpt-6": true, "gpt-5-3-mini": true})
	got := a.NormalizeAccountModelCapabilities([]string{"auto", "gpt-6", "gpt-5-3-mini"}, nil, account.Credential{ID: 1})
	if strings.Join(got, ",") != "auto,gpt-6,gpt-5-3-mini,chatgpt-tts,chatgpt-transcribe,gpt-image-2.5,gpt-image-2.5-flare,gpt-image-2.5-sunburst" {
		t.Fatalf("free catalog image routes = %v", got)
	}
	// gpt-5-3 without the image tool does not open gpt-image-2.
	a.rememberImageCatalog(2, map[string]bool{"gpt-5-3": false, "auto": true})
	if got := a.NormalizeAccountModelCapabilities([]string{"gpt-5-3", "auto"}, nil, account.Credential{ID: 2}); slices.Contains(got, "gpt-image-2") {
		t.Fatalf("gpt-image-2 opened without the image tool: %v", got)
	}
	a.rememberImageCatalog(3, map[string]bool{"gpt-5-3": true})
	if got := a.NormalizeAccountModelCapabilities([]string{"gpt-5-3"}, nil, account.Credential{ID: 3}); !slices.Contains(got, "gpt-image-2") || slices.Contains(got, "gpt-image-2.5") {
		t.Fatalf("gpt-5-3 catalog image routes = %v", got)
	}
	// gpt-6 is preferred; auto is the fallback when gpt-6 cannot draw.
	if pickImageModel(a.imageCandidates("gpt-image-2.5"), map[string]bool{"gpt-6": true, "auto": true}) != "gpt-6" ||
		pickImageModel(a.imageCandidates("gpt-image-2.5"), map[string]bool{"gpt-6": false, "auto": true}) != "auto" ||
		a.imageCandidates("gpt-6") != nil {
		t.Fatal("conversation model preference")
	}
	a.UpdateConfig(Config{ImageConversationModel: "gpt-5-6"})
	if got := a.imageCandidates("gpt-image-2.5"); len(got) != 1 || got[0] != "gpt-5-6" || a.QuotaMode("gpt-image-2.5-flare") != quotaModeImage {
		t.Fatalf("configured override = %v", got)
	}
	if len(Routes()) != 18 {
		t.Fatalf("routes = %d", len(Routes()))
	}
}

func TestImageModelResolvesFromTheLiveCatalog(t *testing.T) {
	var catalogs atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/models" {
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			return
		}
		catalogs.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-6","enabled_tools":["tools","search"]},{"slug":"auto","enabled_tools":["tools","image_gen_tool_enabled"]}]}`))
	}))
	t.Cleanup(server.Close)
	a, _ := newTestAdapter(t, server)
	a.UpdateConfig(Config{})
	credential := testCredential(t, a, credentials{AccessToken: "AT"})
	credential.ID = 9
	for range 2 {
		model, err := a.resolveImageModel(context.Background(), credential, "gpt-image-2.5")
		if err != nil || model != "auto" {
			t.Fatalf("model = %q, err = %v", model, err)
		}
	}
	if catalogs.Load() != 1 {
		t.Fatalf("catalog fetched %d times, want 1 (cached)", catalogs.Load())
	}
	if _, err := a.resolveImageModel(context.Background(), credential, "gpt-image-2"); err == nil {
		t.Fatal("gpt-image-2 resolved without gpt-5-3")
	}
}

func TestSubmittedImageFinishesAfterClientDisconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var submits atomic.Int32
	inner := imageUpstream(t, "", 0, &submits)
	target, _ := url.Parse(inner.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/backend-api/conversation/conv1" {
			cancel() // the client goes away while the image is being drawn
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.Close)
	a, assets := newTestAdapter(t, upstream)
	credential := testCredential(t, a, credentials{AccessToken: "AT"})

	response, err := a.GenerateImage(ctx, provider.ImageGenerationRequest{Credential: credential, Model: "gpt-image-2", Prompt: "a fox"})
	if err != nil || response.StatusCode != http.StatusOK || submits.Load() != 1 || assets.saved != 1 {
		t.Fatalf("response = %+v, err = %v, submits = %d, saved = %d", response, err, submits.Load(), assets.saved)
	}
}

func TestSedimentImageDownloadUsesAccountSession(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveWebBasics(w, r) {
			return
		}
		switch r.URL.Path {
		case "/backend-api/f/conversation/prepare":
			_, _ = w.Write([]byte(`{"conduit_token":"conduit"}`))
		case "/backend-api/f/conversation":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"conversation_id\":\"conv1\"}\n\ndata: [DONE]\n\n"))
		case "/backend-api/conversation/conv1":
			_, _ = w.Write([]byte(`{"conversation_id":"conv1","mapping":{"m1":{"message":{"author":{"role":"tool"},"create_time":1,"content":{"content_type":"multimodal_text","parts":[{"content_type":"image_asset_pointer","asset_pointer":"sediment://file_00000000ae14"}]}}}}}`))
		case "/backend-api/conversation/conv1/attachment/file_00000000ae14/download":
			_, _ = w.Write([]byte(`{"download_url":"` + server.URL + `/backend-api/estuary/content?id=file_00000000ae14&sig=s"}`))
		case "/backend-api/estuary/content":
			if r.Header.Get("Authorization") != "Bearer AT" || r.URL.Query().Get("sig") != "s" {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"detail":"File stream access denied."}`))
				return
			}
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(testPNG())
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	a, assets := newTestAdapter(t, server)
	credential := testCredential(t, a, credentials{AccessToken: "AT"})

	response, err := a.GenerateImage(context.Background(), provider.ImageGenerationRequest{Credential: credential, Model: "gpt-image-2", Prompt: "a fox"})
	if err != nil || response.StatusCode != http.StatusOK || assets.saved != 1 {
		t.Fatalf("response = %+v, err = %v, saved = %d", response, err, assets.saved)
	}
}
