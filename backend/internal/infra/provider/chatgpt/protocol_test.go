package chatgpt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/channel"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/chatgpt/browserprofile"
)

func TestImportRecordsAreIndependent(t *testing.T) {
	a := NewAdapter()
	defer a.Close()
	for _, raw := range []string{`[{"access_token":"AT"},{"name":"missing"},{"refresh_token":"RT","client_profile":"platform_pkce_v1"}]`, "{\"access_token\":\"AT\"}\n{broken}\n{\"refresh_token\":\"RT\",\"client_profile\":\"platform_pkce_v1\"}"} {
		rows, e := a.ParseImport([]byte(raw))
		if e != nil || len(rows) != 3 {
			t.Fatalf("parse: %v %+v", e, rows)
		}
		if rows[0].Error != "" || rows[1].Error == "" || rows[2].Error != "" {
			t.Fatalf("per-record errors: %+v", rows)
		}
		if rows[0].Credentials.DeviceID == "" || rows[0].Credentials.SessionID == "" {
			t.Fatal("device context not saved")
		}
	}
	profile, _ := json.Marshal(browserprofile.Default())
	rows, e := a.ParseImport([]byte(`{"accounts":[{"access_token":"AT","browser_profile":` + string(profile) + `}]} `))
	if e != nil || len(rows) != 1 || rows[0].Error != "" {
		t.Fatalf("profile import: %v %+v", e, rows)
	}
	w, e := wire(rows[0].Credentials)
	if e != nil {
		t.Fatal(e)
	}
	p, e := accountBrowserProfile(w.Fields)
	if e != nil || p.Name != browserprofile.Default().Name {
		t.Fatal("profile lost")
	}
}
func TestOAuthPKCEAndStrictCallback(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["client_id"] != oauthClientID || (body["grant_type"] == "authorization_code" && body["code_verifier"] == "") {
			t.Error("incorrect PKCE exchange")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fresh-AT","refresh_token":"rotated-RT","expires_in":3600}`))
	}))
	defer server.Close()
	a := newAdapter(server.URL, server.URL+"/authorize", server.URL+"/token", server.Client())
	defer a.Close()
	ctx := context.Background()
	start, e := a.StartOAuth(ctx)
	if e != nil {
		t.Fatal(e)
	}
	u, _ := url.Parse(start.URL)
	if u.Query().Get("code_challenge_method") != "S256" || start.State == "" || start.Verifier == "" {
		t.Fatal("PKCE missing")
	}
	for _, callback := range []string{"bare-code", oauthRedirect + "?code=a", oauthRedirect + "?state=wrong&code=a", "https://attacker.example/auth/callback?state=" + start.State + "&code=a", oauthRedirect + "?state=" + start.State + "&state=" + start.State + "&code=a"} {
		if _, e := a.ExchangeOAuth(ctx, start, callback); e == nil {
			t.Fatalf("invalid callback accepted: %s", callback)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid callback reached token endpoint")
	}
	c, e := a.ExchangeOAuth(ctx, start, oauthRedirect+"?state="+start.State+"&code=a")
	if e != nil || c.AccessToken != "fresh-AT" || c.RefreshToken != "rotated-RT" || c.ClientProfile != oauthProfile {
		t.Fatalf("exchange: %v", e)
	}
	c.ClientProfile = "unknown-client"
	if _, e := a.Refresh(ctx, c); e == nil {
		t.Fatal("RT sent to wrong OAuth client")
	}
	if calls.Load() != 1 {
		t.Fatal("wrong-profile RT reached endpoint")
	}
}
func TestSentinelProofCancellationAndBudget(t *testing.T) {
	p := browserprofile.Default()
	zone := openAITimezoneSnapshot{at: time.Now(), location: time.UTC}
	if token := buildProofToken(context.Background(), "seed", "ff", p, nil, "build", zone); !strings.HasPrefix(token, "gAAAAAB") {
		t.Fatal("proof failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if token := buildProofToken(ctx, "seed", "00", p, nil, "build", zone); token != "" {
		t.Fatal("canceled proof returned token")
	}
	if token := buildProofToken(context.Background(), "seed", "invalid", p, nil, "build", zone); token != "" {
		t.Fatal("malformed difficulty accepted")
	}
}
func TestSentinelUnknownAndGlobalStepBudget(t *testing.T) {
	encode := func(ops []any) string {
		raw, _ := json.Marshal(ops)
		return base64.StdEncoding.EncodeToString([]byte(xorSentinelString(string(raw), "key")))
	}
	if _, e := solveSentinelTurnstileToken(context.Background(), encode([]any{[]any{999}}), "key"); e == nil {
		t.Fatal("unknown opcode accepted")
	}
	vm := &sentinelVM{ctx: context.Background(), values: map[any]any{}}
	vm.install()
	vm.steps = 19999
	vm.values[float64(9)] = []any{[]any{float64(2), "x", "one"}}
	if e := vm.runQueue(20000); e != nil {
		t.Fatal(e)
	}
	vm.values[float64(9)] = []any{[]any{float64(2), "x", "two"}}
	if e := vm.runQueue(20000); e == nil {
		t.Fatal("nested execution reset global budget")
	}
}
func TestChatStreamPreservesHistoryAndRejectsPrematureEOF(t *testing.T) {
	stream := `data: {"message":{"author":{"role":"assistant"},"channel":"analysis","content":{"parts":["hidden"]}}}

data: {"message":{"author":{"role":"assistant"},"channel":"final","content":{"parts":["你好"]}}}

data: {"p":"/message/content/parts/0","o":"append","v":" world"}

data: [DONE]
`
	var text string
	e := scanOpenAIChat(strings.NewReader(stream), func(ev OpenAIChatEvent) error { text += ev.Text; return nil })
	if e != nil || text != "你好 world" {
		t.Fatalf("stream: %q %v", text, e)
	}
	if e := scanOpenAIChat(strings.NewReader(strings.ReplaceAll(stream, "data: [DONE]", "")), func(OpenAIChatEvent) error { return nil }); e == nil {
		t.Fatal("truncated stream treated as complete")
	}
	a := NewAdapter()
	defer a.Close()
	if f := a.Classify(&upstreamError{Status: 403}); f.Kind == channel.FailureAuth {
		t.Fatal("403 incorrectly requires login")
	}
}

func TestImageSubmissionReusesUploadedCheckpointAndPreservesIt(t *testing.T) {
	var uploads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/":
			_, _ = w.Write([]byte(`<html data-build="fixture"></html>`))
		case "/backend-api/sentinel/chat-requirements/prepare":
			_, _ = w.Write([]byte(`{"prepare_token":"prepare"}`))
		case "/backend-api/sentinel/chat-requirements/finalize":
			_, _ = w.Write([]byte(`{"token":"sentinel"}`))
		case "/backend-api/f/conversation/prepare":
			_, _ = w.Write([]byte(`{"conduit_token":"conduit"}`))
		case "/backend-api/f/conversation":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"conversation_id\":\"conversation\"}\n\ndata: [DONE]\n\n"))
		case "/backend-api/files":
			uploads.Add(1)
			w.WriteHeader(500)
		default:
			t.Errorf("unexpected endpoint: %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	a := newAdapter(server.URL, server.URL, server.URL, server.Client())
	defer a.Close()
	checkpoint := channel.Checkpoint{Version: 1, Uploads: []channel.UploadedImage{{Index: 0, FileID: "file_reference", MIME: "image/png", Width: 1, Height: 1}}}
	var saved []channel.Checkpoint
	result, e := a.SubmitImage(context.Background(), channel.Credentials{AccessToken: "fixture-AT"}, channel.PreparedImage{Request: channel.ImageRequest{ConversationModel: "m", Prompt: "edit", Size: "auto"}, Inputs: []channel.Blob{{Data: []byte("not reread during upload recovery")}}, Checkpoint: checkpoint}, func(cp channel.Checkpoint) error { saved = append(saved, cp); return nil })
	if e != nil {
		t.Fatal(e)
	}
	if uploads.Load() != 0 || result.ConversationID != "conversation" || len(result.Uploads) != 1 {
		t.Fatalf("checkpoint not reused: %+v", result)
	}
	for _, cp := range saved {
		if len(cp.Uploads) != 1 {
			t.Fatal("submission marker discarded uploaded file references")
		}
	}
}
func TestOnlyExplicitReasoningSummaryIsEmitted(t *testing.T) {
	var state openAIChatState
	event, e := state.event(map[string]any{"message": map[string]any{"author": map[string]any{"role": "assistant"}, "channel": "analysis", "content": map[string]any{"content_type": "thoughts", "thoughts": []any{map[string]any{"summary": "可见摘要", "content": "raw analysis must not be emitted"}}}}})
	if e != nil || event.Thinking != "可见摘要" || event.Text != "" {
		t.Fatalf("summary extraction: %+v %v", event, e)
	}
}
