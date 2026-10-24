package muse

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type egressRepositoryStub struct{}

func (egressRepositoryStub) ListEgressNodes(context.Context, egressdomain.Scope, repository.SortQuery) ([]egressdomain.Node, error) {
	return nil, nil
}
func (egressRepositoryStub) GetEgressNode(context.Context, uint64) (egressdomain.Node, error) {
	return egressdomain.Node{}, errors.New("not found")
}
func (egressRepositoryStub) CreateEgressNode(context.Context, egressdomain.Node) (egressdomain.Node, error) {
	return egressdomain.Node{}, errors.New("unsupported")
}
func (egressRepositoryStub) UpdateEgressNode(context.Context, egressdomain.Node) (egressdomain.Node, error) {
	return egressdomain.Node{}, errors.New("unsupported")
}
func (egressRepositoryStub) DeleteEgressNode(context.Context, uint64) error {
	return errors.New("unsupported")
}

type fakeEngine struct {
	chat     func(turn chatTurn, onText func(string) error) (string, error)
	generate func(job mediaJob) (mediaFile, error)
	usage    usage
	sessions []session
}

func (f *fakeEngine) Chat(_ context.Context, s session, turn chatTurn, onText func(string) error) (string, error) {
	f.sessions = append(f.sessions, s)
	return f.chat(turn, onText)
}

func (f *fakeEngine) Generate(_ context.Context, s session, job mediaJob) (mediaFile, error) {
	f.sessions = append(f.sessions, s)
	return f.generate(job)
}

func (f *fakeEngine) Usage(_ context.Context, s session) (usage, error) {
	f.sessions = append(f.sessions, s)
	return f.usage, nil
}

type assetStub struct{ videos [][]byte }

func (a *assetStub) SaveImage(context.Context, []byte) (media.Asset, error) {
	return media.Asset{ID: "img_1"}, nil
}
func (a *assetStub) PublicImageURL(id string) string { return "https://gw.test/media/" + id }
func (a *assetStub) SaveVideo(_ context.Context, _, contentType string, body io.Reader) (media.Asset, error) {
	data, _ := io.ReadAll(body)
	a.videos = append(a.videos, data)
	return media.Asset{ID: "vid_1", MIMEType: contentType}, nil
}

var testJar = []cookie{
	{Name: "hatch_sess", Value: "sess-1", Domain: ".muse.ai", Path: "/", HTTPOnly: true, Secure: true},
	{Name: "hatch_vml", Value: "vml-old", Domain: ".muse.ai", Path: "/", Expires: 1},
	{Name: "hatch_native_auth_device", Value: "dev-1", Domain: "muse.ai", HostOnly: true, Path: "/"},
}

func newTestAdapter(t *testing.T, fake *fakeEngine) (*Adapter, account.Credential, *assetStub) {
	t.Helper()
	scopes := egressdomain.ExtensionScopes()
	egressdomain.SetExtensionScopes([]egressdomain.Scope{scope})
	t.Cleanup(func() { egressdomain.SetExtensionScopes(scopes) })
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	assets := &assetStub{}
	adapter := NewAdapter(Config{}, infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, assets, nil)
	adapter.engine = fake
	encrypted, err := adapter.encryptJar(testJar)
	if err != nil {
		t.Fatal(err)
	}
	return adapter, account.Credential{ID: 7, Provider: account.ProviderMuse, AuthType: account.AuthTypeOAuth, EncryptedAccessToken: encrypted, EncryptedRefreshToken: encrypted}, assets
}

func TestParseImportedCredentialsFormats(t *testing.T) {
	adapter := &Adapter{}
	export, _ := json.Marshal([]map[string]any{
		{"domain": ".muse.ai", "name": "hatch_sess", "value": "a", "hostOnly": false, "expirationDate": 1800000000.5},
		{"domain": "muse.ai", "name": "hatch_native_auth_device", "value": "b", "hostOnly": true},
		{"domain": ".example.com", "name": "other", "value": "c"},
	})
	seeds, err := adapter.ParseImportedCredentials(export)
	if err != nil || len(seeds) != 1 {
		t.Fatalf("cookie export: seeds = %d, err = %v", len(seeds), err)
	}
	var jar []cookie
	_ = json.Unmarshal([]byte(seeds[0].AccessToken), &jar)
	if len(jar) != 2 || cookieValue(jar, "hatch_sess") != "a" || jar[0].Expires != 1800000000.5 || !jar[1].HostOnly {
		t.Fatalf("cookie export jar = %#v", jar)
	}
	if seeds[0].AccessToken != seeds[0].RefreshToken || seeds[0].Provider != account.ProviderMuse || !strings.HasPrefix(seeds[0].SourceKey, "muse:") {
		t.Fatalf("seed = %#v", seeds[0])
	}

	lines := "acc-01 | hatch_sess=s1; hatch_vml=v1; hatch_native_auth_device=d1\nCookie: hatch_sess=s2\n# comment\nwd=1\n"
	seeds, err = adapter.ParseImportedCredentials([]byte(lines))
	if err != nil || len(seeds) != 2 || seeds[0].Name != "acc-01" {
		t.Fatalf("header lines: seeds = %#v, err = %v", seeds, err)
	}
	_ = json.Unmarshal([]byte(seeds[0].AccessToken), &jar)
	if device := jar[2]; device.Name != "hatch_native_auth_device" || device.Domain != "muse.ai" || !device.HostOnly || jar[0].Domain != ".muse.ai" {
		t.Fatalf("header jar = %#v", jar)
	}

	exported, err := adapter.MarshalCredentials(seeds)
	if err != nil {
		t.Fatal(err)
	}
	again, err := adapter.ParseImportedCredentials(exported)
	if err != nil || len(again) != 2 || again[0].Name != "acc-01" || again[1].AccessToken != seeds[1].AccessToken {
		t.Fatalf("export round trip: %#v, err = %v", again, err)
	}

	if _, err := adapter.ParseImportedCredentials([]byte("hatch_vml=only")); err == nil {
		t.Fatal("a jar without hatch_sess was accepted")
	}
}

func TestSessionRenewalMergesSetCookie(t *testing.T) {
	status := http.StatusOK
	var gotCookie string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Add("Set-Cookie", "hatch_vml=; Max-Age=0; Path=/; HttpOnly; Secure")
		w.Header().Add("Set-Cookie", "hatch_vml=vml-new; Max-Age=172800; Path=/; Domain=.muse.ai; SameSite=Lax; HttpOnly; Secure")
		_, _ = io.WriteString(w, `{"status":"assigned","vm_id":"vm-42","vm_state":"RUNNING"}`)
	}))
	defer server.Close()
	adapter, credential, _ := newTestAdapter(t, &fakeEngine{})
	adapter.sessionURL = server.URL

	refreshed, err := adapter.RefreshCredential(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotCookie, "hatch_sess=sess-1") {
		t.Fatalf("sent cookie = %q", gotCookie)
	}
	if left := time.Until(refreshed.ExpiresAt); left < 47*time.Hour || left > 49*time.Hour {
		t.Fatalf("expires in %v", left)
	}
	jar, err := adapter.jar(refreshed.EncryptedAccessToken)
	if err != nil || cookieValue(jar, "hatch_vml") != "vml-new" || len(jar) != 3 || refreshed.EncryptedRefreshToken != refreshed.EncryptedAccessToken {
		t.Fatalf("refreshed jar = %#v, err = %v", jar, err)
	}

	seed, err := adapter.PrepareImportedCredential(context.Background(), provider.CredentialSeed{SourceKey: "muse:x", RefreshToken: jarJSON(testJar)})
	if err != nil || seed.UserID != "vm-42" || !strings.Contains(seed.AccessToken, "vml-new") || seed.ExpiresAt.IsZero() {
		t.Fatalf("prepared seed = %#v, err = %v", seed, err)
	}

	status = http.StatusUnauthorized
	_, err = adapter.RefreshCredential(context.Background(), credential)
	var refreshErr *provider.CredentialRefreshError
	if !errors.As(err, &refreshErr) || !refreshErr.Permanent || refreshErr.Status != http.StatusUnauthorized {
		t.Fatalf("401 refresh error = %#v", err)
	}
	status = http.StatusForbidden
	if _, err = adapter.RefreshCredential(context.Background(), credential); !errors.As(err, &refreshErr) || refreshErr.Permanent {
		t.Fatalf("403 must not be permanent: %#v", err)
	}
}

// Each account keeps one browser identity: import picks it, export and a
// re-import keep it, and both the browser and session renewals present it.
func TestAccountKeepsItsBrowserIdentity(t *testing.T) {
	var gotAgent, gotPlatform string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAgent, gotPlatform = r.Header.Get("User-Agent"), r.Header.Get("Sec-Ch-Ua-Platform")
		_, _ = io.WriteString(w, `{"status":"assigned","vm_id":"vm-42"}`)
	}))
	defer server.Close()
	fake := &fakeEngine{chat: func(chatTurn, func(string) error) (string, error) { return "ok", nil }}
	adapter, credential, _ := newTestAdapter(t, fake)
	adapter.sessionURL = server.URL
	ctx := context.Background()

	seed, err := adapter.PrepareImportedCredential(ctx, provider.CredentialSeed{SourceKey: "muse:x", RefreshToken: jarJSON(testJar)})
	identity, ok := identityByKey(seed.State[stateBrowser])
	if err != nil || !ok {
		t.Fatalf("imported identity = %q, err = %v", seed.State[stateBrowser], err)
	}
	if gotAgent != identity.userAgent(defaultChromeVersion) || gotPlatform != `"`+identity.uaPlatform+`"` {
		t.Fatalf("renewal sent %q / %s for %s", gotAgent, gotPlatform, identity.Key)
	}
	exported, err := adapter.MarshalCredentials([]provider.CredentialSeed{seed})
	if err != nil {
		t.Fatal(err)
	}
	again, err := adapter.ParseImportedCredentials(exported)
	if err != nil {
		t.Fatal(err)
	}
	if roundTrip := mustPrepare(t, adapter, ctx, again[0]); roundTrip.State[stateBrowser] != identity.Key {
		t.Fatalf("export round trip identity = %q, want %q", roundTrip.State[stateBrowser], identity.Key)
	}

	other := browserIdentities[0]
	if other.Key == identity.Key {
		other = browserIdentities[1]
	}
	credential.EncryptedState, _ = adapter.cipher.Encrypt(`{"browser":"` + other.Key + `"}`)
	reimported := mustPrepare(t, adapter, infraegress.WithExistingAccount(ctx, credential), provider.CredentialSeed{SourceKey: "muse:x", RefreshToken: jarJSON(testJar)})
	if reimported.State[stateBrowser] != other.Key {
		t.Fatalf("re-import identity = %q, want the stored %q", reimported.State[stateBrowser], other.Key)
	}
	if _, err := adapter.ForwardResponse(ctx, provider.ResponseResourceRequest{Credential: credential, Method: http.MethodPost, Path: "/responses", Model: modelChat, Operation: conversation.OperationResponses, Body: []byte(`{"input":"hi"}`)}); err != nil {
		t.Fatal(err)
	}
	if got := fake.sessions[len(fake.sessions)-1]; got.Identity.Key != other.Key || got.Timezone != usTimezones[credential.ID%uint64(len(usTimezones))] {
		t.Fatalf("browser session identity = %q, timezone = %q", got.Identity.Key, got.Timezone)
	}
	// An account stored without an identity still keeps a stable one.
	credential.EncryptedState = ""
	if adapter.identityOf(credential) != adapter.identityOf(credential) || adapter.identityOf(credential).Key == "" {
		t.Fatal("account without stored identity got none")
	}
}

func mustPrepare(t *testing.T, adapter *Adapter, ctx context.Context, seed provider.CredentialSeed) provider.CredentialSeed {
	t.Helper()
	prepared, err := adapter.PrepareImportedCredential(ctx, seed)
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

// Renewal wakes a cloud VM that is not running, with the renewed cookies;
// a failed wake never fails the renewal.
func TestRenewalWakesASleepingVM(t *testing.T) {
	var state string
	var wakeStatus int
	var wakes []map[string]any
	var wakeCookie string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/wake" {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			wakes, wakeCookie = append(wakes, body), r.Header.Get("Cookie")
			w.WriteHeader(wakeStatus)
			return
		}
		w.Header().Add("Set-Cookie", "hatch_vml=vml-new; Max-Age=172800; Path=/; Domain=.muse.ai")
		_, _ = io.WriteString(w, `{"status":"assigned","vm_id":"vm-42","vm_state":"`+state+`"}`)
	}))
	defer server.Close()
	adapter, credential, _ := newTestAdapter(t, &fakeEngine{})
	adapter.sessionURL, adapter.wakeURL = server.URL+"/session", server.URL+"/wake"
	for _, tc := range []struct {
		state      string
		wakeStatus int
		wakes      int
	}{{"RUNNING", 200, 0}, {"DISABLED", 200, 0}, {"SLEEPING", 200, 1}, {"STOPPED", 500, 1}} {
		state, wakeStatus, wakes = tc.state, tc.wakeStatus, nil
		if _, err := adapter.RefreshCredential(context.Background(), credential); err != nil {
			t.Fatalf("%s: %v", tc.state, err)
		}
		if len(wakes) != tc.wakes {
			t.Fatalf("%s: %d wakes, want %d", tc.state, len(wakes), tc.wakes)
		}
		if tc.wakes > 0 && (wakes[0]["vm_id"] != "vm-42" || wakes[0]["retry_count"] != float64(0) || wakes[0]["connect_attempt_id"] == "" || !strings.Contains(wakeCookie, "hatch_vml=vml-new")) {
			t.Fatalf("%s: wake = %v, cookie = %q", tc.state, wakes[0], wakeCookie)
		}
	}
}

func TestUsageWindow(t *testing.T) {
	reset := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	window := usageWindow(usage{UsedPercent: 2.4, ResetAt: reset}, time.Now())
	if window.Mode != quotaModeWeekly || window.Total != 100 || window.Remaining != 97 || window.UsagePercent != 2.4 || window.ResetAt == nil || !window.ResetAt.Equal(reset) {
		t.Fatalf("window = %#v", window)
	}
	if exhausted := usageWindow(usage{UsedPercent: 100}, time.Now()); exhausted.Remaining != 0 || exhausted.ResetAt != nil {
		t.Fatalf("exhausted window = %#v", exhausted)
	}
}

func TestBuildChatPrompt(t *testing.T) {
	prompt, images, err := buildChatPrompt(responsesRequest{Input: json.RawMessage(`"hello"`)})
	if err != nil || prompt != "hello" || len(images) != 0 {
		t.Fatalf("single turn = %q %v %v", prompt, images, err)
	}
	input := `[
		{"role":"developer","content":"be brief"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"look"},{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a cat"}]},
		{"type":"function_call","name":"lookup","arguments":"{}"},
		{"type":"function_call_output","output":"42"},
		{"role":"user","content":[{"type":"input_text","text":"why"},{"type":"input_image","image_url":{"url":"https://x.test/a.png"}}]}
	]`
	prompt, images, err = buildChatPrompt(responsesRequest{Instructions: "sys", Input: json.RawMessage(input)})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Instructions:\nsys\n\nbe brief", "User:\nlook", "Assistant:\na cat", "Called tool lookup with {}", "Tool result:\n42", "User:\nwhy", "Reply to the last User message."} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt %q lacks %q", prompt, want)
		}
	}
	if len(images) != 1 || images[0] != "https://x.test/a.png" {
		t.Fatalf("images = %v", images)
	}
	if _, _, err := buildChatPrompt(responsesRequest{Input: json.RawMessage(`[{"role":"assistant","content":"hi"}]`)}); err == nil {
		t.Fatal("a conversation ending with the assistant was accepted")
	}
}

func TestReplyStreamPendingHoldsTheLastParagraph(t *testing.T) {
	stream := &replyStream{}
	if got := stream.pending("Hello wor"); got != "" {
		t.Fatalf("partial paragraph streamed: %q", got)
	}
	got := stream.pending("Hello world.\n\nSecond")
	if got != "Hello world.\n\n" {
		t.Fatalf("pending = %q", got)
	}
	stream.sent = got
	if got := stream.pending("Hello world!\n\nSecond\n\nThird"); got != "" {
		t.Fatalf("rewritten text streamed: %q", got)
	}
}

func TestForwardResponse(t *testing.T) {
	fake := &fakeEngine{chat: func(turn chatTurn, onText func(string) error) (string, error) {
		if turn.Prompt != "hi" {
			return "", errors.New("unexpected prompt " + turn.Prompt)
		}
		for _, snapshot := range []string{"Hel", "Hello.\n\nWor", "Hello.\n\nWorld **done**"} {
			if err := onText(snapshot); err != nil {
				return "", err
			}
		}
		return "Hello.\n\nWorld **done**", nil
	}}
	adapter, credential, _ := newTestAdapter(t, fake)
	request := provider.ResponseResourceRequest{Credential: credential, Method: http.MethodPost, Path: "/responses", Model: modelChat, Operation: conversation.OperationResponses, Body: []byte(`{"model":"muse-spark","input":"hi"}`)}

	response, err := adapter.ForwardResponse(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(response.Body)
	var object map[string]any
	_ = json.Unmarshal(data, &object)
	output := object["output"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if response.StatusCode != http.StatusOK || object["status"] != "completed" || output["text"] != "Hello.\n\nWorld **done**" || object["usage"] == nil {
		t.Fatalf("non-stream response = %s", data)
	}

	request.Streaming = true
	response, err = adapter.ForwardResponse(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	data, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	events := string(data)
	if !strings.Contains(events, `"delta":"Hello.\n\n"`) || !strings.Contains(events, `"delta":"World **done**"`) || !strings.Contains(events, "event: response.completed") || strings.Index(events, "response.created") > strings.Index(events, "output_text.delta") {
		t.Fatalf("stream = %s", events)
	}

	chat := request
	chat.Streaming = false
	chat.Operation = conversation.OperationChat
	chat.NormalizeBody = true
	chat.Body = []byte(`{"model":"muse-spark","messages":[{"role":"user","content":"hi"}]}`)
	response, err = adapter.ForwardResponse(context.Background(), chat)
	if err != nil {
		t.Fatal(err)
	}
	data, _ = io.ReadAll(response.Body)
	if !bytes.Contains(data, []byte(`"object":"chat.completion"`)) || !bytes.Contains(data, []byte("World **done**")) {
		t.Fatalf("chat completion = %s", data)
	}

	// A failure before any text reaches the gateway as a status it acts on.
	fake.chat = func(chatTurn, func(string) error) (string, error) { return "", errLoggedOut }
	response, err = adapter.ForwardResponse(context.Background(), request)
	if err != nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("logged out = %#v, err = %v", response, err)
	}
	fake.chat = func(chatTurn, func(string) error) (string, error) { return "", errors.New("socket closed") }
	if response, err = adapter.ForwardResponse(context.Background(), request); err == nil {
		t.Fatalf("unknown failure became a response: %#v", response)
	}
}

func TestDrawImage(t *testing.T) {
	fake := &fakeEngine{generate: func(job mediaJob) (mediaFile, error) {
		if job.Kind != mediaImage || !strings.Contains(job.Prompt, "Aspect ratio: 16:9 (landscape).") || !strings.HasSuffix(job.Prompt, "a boat") {
			return mediaFile{}, errors.New("unexpected job " + job.Prompt)
		}
		_ = job.OnThread("/thread/1")
		return mediaFile{Data: []byte("RIFF....WEBP"), MIME: "image/webp"}, nil
	}}
	adapter, credential, _ := newTestAdapter(t, fake)
	response, err := adapter.GenerateImage(context.Background(), provider.ImageGenerationRequest{Credential: credential, Model: modelImage, Prompt: "a boat", Size: "1792x1008", ResponseFormat: "url"})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(response.Body)
	if !bytes.Contains(data, []byte(`"url":"https://gw.test/media/img_1"`)) || response.QuotaUnits != 1 {
		t.Fatalf("image response = %s", data)
	}

	// Before the prompt is sent a timeout may move to another account; after
	// it the usage is spent and the gateway must not draw again.
	fake.generate = func(mediaJob) (mediaFile, error) { return mediaFile{}, errTimeout }
	if response, err = adapter.GenerateImage(context.Background(), provider.ImageGenerationRequest{Credential: credential, Prompt: "x"}); err != nil || response.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("unsent timeout = %#v, %v", response, err)
	}
	fake.generate = func(job mediaJob) (mediaFile, error) { _ = job.OnThread("/thread/2"); return mediaFile{}, errTimeout }
	if response, err = adapter.GenerateImage(context.Background(), provider.ImageGenerationRequest{Credential: credential, Prompt: "x"}); err == nil {
		t.Fatalf("sent timeout became a response: %#v", response)
	}
	fake.generate = func(job mediaJob) (mediaFile, error) {
		_ = job.OnThread("/thread/3")
		return mediaFile{}, &textOnlyError{Text: "I can't draw that"}
	}
	if response, err = adapter.GenerateImage(context.Background(), provider.ImageGenerationRequest{Credential: credential, Prompt: "x"}); err != nil || response.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("refusal = %#v, %v", response, err)
	}
}

func TestGenerateVideoCheckpoints(t *testing.T) {
	var jobs []mediaJob
	fake := &fakeEngine{generate: func(job mediaJob) (mediaFile, error) {
		jobs = append(jobs, job)
		if job.Thread == "" {
			if err := job.OnThread("/thread/v1"); err != nil {
				return mediaFile{}, err
			}
		}
		return mediaFile{Data: []byte("mp4"), MIME: "video/mp4"}, nil
	}}
	adapter, credential, assets := newTestAdapter(t, fake)
	var saved []string
	request := provider.VideoRequest{Credential: credential, JobID: "job", Prompt: "leaves", Duration: 5, AspectRatio: "9:16", SaveCheckpoint: func(value string) error { saved = append(saved, value); return nil }}
	result, err := adapter.GenerateVideo(context.Background(), request)
	if err != nil || result.AssetID != "vid_1" || len(assets.videos) != 1 {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	if len(saved) != 2 || saved[0] != `{"submitting":true}` || saved[1] != `{"thread":"/thread/v1"}` {
		t.Fatalf("checkpoints = %v", saved)
	}
	if !strings.Contains(jobs[0].Prompt, "9:16 portrait") || !strings.Contains(jobs[0].Prompt, "Duration: 5 seconds.") {
		t.Fatalf("prompt = %q", jobs[0].Prompt)
	}

	request.Checkpoint = saved[1]
	saved = nil
	if _, err := adapter.GenerateVideo(context.Background(), request); err != nil || jobs[1].Thread != "/thread/v1" || len(saved) != 0 {
		t.Fatalf("resume: job = %#v, saved = %v, err = %v", jobs[1], saved, err)
	}

	request.Checkpoint = `{"submitting":true}`
	if _, err := adapter.GenerateVideo(context.Background(), request); err == nil {
		t.Fatal("a sent prompt without a thread was sent again")
	} else if stage, _ := provider.VideoErrorStage(err); stage != provider.VideoStageSubmitted {
		t.Fatalf("stage = %s", stage)
	}
}

func TestBrowserProxy(t *testing.T) {
	for raw, want := range map[string]string{"": "", "http://10.0.0.1:8080": "http://10.0.0.1:8080", "socks5h://warp:1080": "socks5://warp:1080"} {
		if got, err := browserProxy(raw); err != nil || got != want {
			t.Fatalf("%q = %q, %v", raw, got, err)
		}
	}
	for _, raw := range []string{"http://user:pass@proxy:8080", "trojan://secret@host:443", "vless://id@host:443"} {
		if _, err := browserProxy(raw); err == nil {
			t.Fatalf("%q was accepted", raw)
		}
	}
}

func TestAspectLabel(t *testing.T) {
	for size, want := range map[string]string{"1024x1024": "1:1 (square)", "1024x1536": "2:3 (portrait)", "auto": "", "": ""} {
		if got := aspectLabel(size); got != want {
			t.Fatalf("%q = %q, want %q", size, got, want)
		}
	}
}

func jarJSON(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}
