package adobe

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// validCookie 构造一个带完整 Sherlock 四字段的 refreshprofile Cookie。
func validCookie() string {
	token := base64.StdEncoding.EncodeToString([]byte(`{"sid":"s","ark":"a","bfp":"b","ftr":"f"}`))
	return "other=1; sherlockToken=" + token + "; last=2"
}

func TestDefinitionValidatesInRegistry(t *testing.T) {
	adapter := NewAdapter(Config{}, nil, nil, nil, nil)
	if err := adapter.Definition().Validate(); err != nil {
		t.Fatalf("definition invalid: %v", err)
	}
	registry := provider.NewRegistry(adapter)
	if err := registry.Validate(); err != nil {
		t.Fatalf("registry rejected adobe adapter: %v", err)
	}
}

func TestRoutesCoverEveryModel(t *testing.T) {
	routes := Routes()
	if want := len(imageModels)*2 + len(videoModels); len(routes) != want {
		t.Fatalf("routes = %d, want %d", len(routes), want)
	}
	for _, route := range routes {
		if !strings.HasPrefix(route.PublicID, "adobe/") {
			t.Fatalf("route %q is not namespaced", route.PublicID)
		}
		if route.Capability == modeldomain.CapabilityVideo && route.Provider != account.ProviderAdobe {
			t.Fatalf("route %q provider = %s", route.PublicID, route.Provider)
		}
	}
}

func TestParseImportedCredentials(t *testing.T) {
	adapter := NewAdapter(Config{}, nil, nil, nil, nil)
	cookie := validCookie()
	data := cookie + "\n" + `{"cookie":"` + cookie + `","email":"a@b.com"}` + "\n" + "invalid-no-sherlock\n"
	seeds, err := adapter.ParseImportedCredentials([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(seeds) != 2 {
		t.Fatalf("seeds = %d, want 2", len(seeds))
	}
	for _, seed := range seeds {
		if seed.Provider != account.ProviderAdobe || seed.AuthType != account.AuthTypeOAuth || seed.RefreshToken != cookie {
			t.Fatalf("unexpected seed %#v", seed)
		}
	}
	if seeds[1].Email != "a@b.com" {
		t.Fatalf("json record email = %q", seeds[1].Email)
	}
}

func TestBuildImagePayloadsNanoBanana(t *testing.T) {
	in := imagePayloadInput{model: imageModels[0], prompt: "hi", aspectRatio: "16:9", resolution: "2K", count: 1}
	payloads := buildImagePayloads(in)
	if len(payloads) != 1 {
		t.Fatalf("payloads = %d", len(payloads))
	}
	p := payloads[0]
	if p["modelId"] != "gemini-flash" || p["modelVersion"] != "nano-banana-2" {
		t.Fatalf("upstream = %v/%v", p["modelId"], p["modelVersion"])
	}
	if _, ok := p["groundSearch"]; !ok {
		t.Fatal("nano-banana must send groundSearch")
	}
	size := p["size"].(map[string]any)
	if size["width"] != 2048 || size["height"] != 2048 {
		t.Fatalf("nano size = %v", size)
	}
	ms := p["modelSpecificPayload"].(map[string]any)
	if ms["aspectRatio"] != "16:9" {
		t.Fatalf("aspectRatio = %v", ms["aspectRatio"])
	}
}

func TestBuildImagePayloadsGPTImageEditFallbacks(t *testing.T) {
	m, _ := imageModelByID("gpt-image-2")
	in := imagePayloadInput{model: m, prompt: "edit", aspectRatio: "1:1", resolution: "1K", count: 1, sourceImageID: []string{"src1"}, maskID: "mask1"}
	payloads := buildImagePayloads(in)
	if len(payloads) != 3 {
		t.Fatalf("gpt-image edit payloads = %d, want 3", len(payloads))
	}
	for _, p := range payloads {
		meta := p["generationMetadata"].(map[string]any)
		if meta["module"] != "image2image" {
			t.Fatalf("edit module = %v", meta["module"])
		}
		if p["modelId"] != "gpt-image" {
			t.Fatalf("modelId = %v", p["modelId"])
		}
	}
}

func TestBuildVideoPayloadEngines(t *testing.T) {
	sora, _ := videoModelByID("sora2")
	p := buildVideoPayload(videoPayloadInput{model: sora, prompt: "a cat", aspectRatio: "16:9", duration: 8, resolution: "720p"})
	if p["modelId"] != "sora" || p["modelVersion"] != "sora-2" {
		t.Fatalf("sora upstream = %v/%v", p["modelId"], p["modelVersion"])
	}
	if prompt, ok := p["prompt"].(string); !ok || !strings.Contains(prompt, "prompt_text") {
		t.Fatalf("sora prompt must be JSON: %v", p["prompt"])
	}

	kling, _ := videoModelByID("kling3")
	i2v := buildVideoPayload(videoPayloadInput{model: kling, prompt: "move", aspectRatio: "9:16", duration: 5, resolution: "720p", sourceImageID: []string{"img"}})
	if i2v["modelVersion"] != "kling_v3_standard_i2v" {
		t.Fatalf("kling with image must be i2v, got %v", i2v["modelVersion"])
	}
}

func TestClientSubmitPollDownload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/generate-async"):
			if r.Header.Get("x-arp-session-id") == "" {
				t.Error("submit without x-arp-session-id")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"links": map[string]any{"result": "http://" + r.Host + "/status/job1"}})
		case strings.HasPrefix(r.URL.Path, "/status/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "succeeded", "outputs": []any{map[string]any{"image": map[string]any{"presignedUrl": "http://" + r.Host + "/out.png"}}}})
		case r.URL.Path == "/out.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("PNGDATA"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	c := &client{do: server.Client(), rc: requestContext{token: "t", sherlock: "s", apiKey: "k", userAgent: defaultUserAgent}}
	sub, err := c.submitImage(context.Background(), server.URL+"/v2/3p-images/generate-async", []map[string]any{{"prompt": "x"}}, "x")
	if err != nil {
		t.Fatal(err)
	}
	out, err := pollUntilDone(context.Background(), c, sub.pollURL, "image", nil)
	if err != nil || len(out) != 1 {
		t.Fatalf("poll = %v, %v", out, err)
	}
	data, err := c.download(context.Background(), out[0])
	if err != nil || string(data) != "PNGDATA" {
		t.Fatalf("download = %q %v", data, err)
	}
}

func TestClientImsRefreshAndCredits(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/token"):
			if r.Header.Get("Cookie") == "" {
				t.Error("refresh without cookie")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh", "expires_in": 3600})
		case strings.Contains(r.URL.Path, "/credits/balance"):
			_ = json.NewEncoder(w).Encode(map[string]any{"total": map[string]any{"quota": map[string]any{"total": 1000, "available": 400}}})
		}
	}))
	defer server.Close()
	// 测试期间把固定端点指向测试服务器。
	refreshURL, creditsURL = server.URL+"/ims/check/v6/token", server.URL+"/v1/credits/balance"
	defer func() {
		refreshURL = "https://adobeid-na1.services.adobe.com/ims/check/v6/token?jslVersion=v2-v0.48.0-1-g1e322cb"
		creditsURL = "https://firefly.adobe.io/v1/credits/balance"
	}()
	c := &client{do: server.Client(), rc: requestContext{apiKey: "clio-playground-web", userAgent: defaultUserAgent}}
	token, expiresAt, err := c.imsRefresh(context.Background(), validCookie())
	if err != nil || token != "fresh" || expiresAt.IsZero() {
		t.Fatalf("imsRefresh: token=%q exp=%v err=%v", token, expiresAt, err)
	}
	c.rc.token = token
	total, available, err := c.creditBalance(context.Background(), "acct")
	if err != nil || total != 1000 || available != 400 {
		t.Fatalf("creditBalance: total=%d available=%d err=%v", total, available, err)
	}
}

// 网关只看 HTTPStatusCode / ErrUnauthorized / RequestScopedFailure 决策，状态必须如实上报。
func TestClassifyUpstreamReportsGatewayStatus(t *testing.T) {
	cases := []struct {
		status        int
		body          string
		wantStatus    int
		requestScoped bool
	}{
		{401, "", 401, false},
		{402, "", 429, false},
		{400, `{"message":"Insufficient credits"}`, 429, false},
		{403, `{"message":"insufficient permissions"}`, 403, false},
		{400, `{"message":"prompt blocked"}`, 400, true},
		{429, "", 429, false},
		{503, "", 503, false},
	}
	for _, tc := range cases {
		err := classifyUpstream("submit", tc.status, tc.body)
		if got := provider.ErrorHTTPStatusOrZero(err); got != tc.wantStatus || provider.IsRequestScopedError(err) != tc.requestScoped {
			t.Fatalf("status %d body %q => status %d scoped %v", tc.status, tc.body, got, provider.IsRequestScopedError(err))
		}
	}
	if !errors.Is(classifyUpstream("submit", 401, ""), provider.ErrUnauthorized) {
		t.Fatal("401 must match provider.ErrUnauthorized")
	}
	if reason := classifyUpstream("submit", 400, `{"message":"prompt blocked"}`).reason; reason != "prompt blocked" {
		t.Fatalf("reason = %q", reason)
	}
}

func validSherlockToken() string {
	return base64.StdEncoding.EncodeToString([]byte(`{"sid":"s","ark":"a","bfp":"b","ftr":"f"}`))
}

func TestSherlockMintCachesThenForceMintsAgain(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": validSherlockToken()})
	}))
	defer server.Close()
	svc := newSherlockService(func() Config { return Config{SherlockMintAPI: server.URL} })
	if _, err := svc.current(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.current(context.Background()); err != nil { // within refresh window → cached
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("mint hits = %d, want 1 (second call should be cached)", n)
	}
	if _, err := svc.forceMint(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Fatalf("mint hits after forceMint = %d, want 2", n)
	}
}

func TestSherlockMintFallsBackToCacheOnFailure(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{"token": validSherlockToken()})
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	svc := newSherlockService(func() Config { return Config{SherlockMintAPI: server.URL} })
	first, err := svc.current(context.Background())
	if err != nil || first == "" {
		t.Fatalf("first mint: %q %v", first, err)
	}
	// 强制过缓存窗口后再取：mint 失败，应回退到仍在 TTL 内的缓存。
	svc.mu.Lock()
	svc.mintedAt = svc.mintedAt.Add(-10 * time.Minute)
	svc.mu.Unlock()
	again, err := svc.current(context.Background())
	if err != nil || again != first {
		t.Fatalf("stale fallback: got %q err %v", again, err)
	}
}

func TestSherlockNoSourceErrors(t *testing.T) {
	svc := newSherlockService(func() Config { return Config{} })
	if _, err := svc.current(context.Background()); err == nil {
		t.Fatal("no mint API and no manual token must error")
	}
}

// gpt-image 编辑的回退候选里 referenceBlobs 必须是数组（不能是 null），且无蒙版时为空数组。
func TestGPTImageEditReferenceBlobsAreArrays(t *testing.T) {
	m, _ := imageModelByID("gpt-image-2")
	in := imagePayloadInput{model: m, prompt: "edit", aspectRatio: "1:1", resolution: "1K", count: 1, sourceImageID: []string{"src1"}}
	for i, p := range buildImagePayloads(in) {
		encoded, _ := json.Marshal(p["referenceBlobs"])
		if string(encoded) == "null" {
			t.Fatalf("payload %d referenceBlobs serialized to null", i)
		}
	}
}

// kling-o3 带参考图时 module 应为 image2video（与引擎无关，按是否有参考图判定）。
func TestKlingO3ModuleFollowsReference(t *testing.T) {
	m, _ := videoModelByID("kling-o3")
	p := buildVideoPayload(videoPayloadInput{model: m, prompt: "x", aspectRatio: "16:9", duration: 5, resolution: "1080p", sourceImageID: []string{"img"}})
	meta := p["generationMetadata"].(map[string]any)
	if meta["module"] != "image2video" {
		t.Fatalf("kling-o3 with reference image module = %v, want image2video", meta["module"])
	}
	if p["modelVersion"] != "kling_o3_pro_reference_to_video" {
		t.Fatalf("kling-o3 modelVersion = %v", p["modelVersion"])
	}
}

// --- 与网关对接的流程测试 ---

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

// fakeAdobe 是一个可编排的 Adobe + mint 测试服务；submitStatus 为 0 表示接受任务。
type fakeAdobe struct {
	*httptest.Server
	submitStatus int
	submits      atomic.Int32
	mintDown     bool
}

func newFakeAdobe(t *testing.T) (*fakeAdobe, *Adapter, account.Credential) {
	t.Helper()
	fake := &fakeAdobe{}
	fake.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/mint":
			if fake.mintDown {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"token": validSherlockToken()})
		case r.URL.Path == "/generate-async":
			fake.submits.Add(1)
			if fake.submitStatus != 0 {
				w.WriteHeader(fake.submitStatus)
				_ = json.NewEncoder(w).Encode(map[string]any{"message": "prompt blocked"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"links": map[string]any{"result": "http://" + r.Host + "/status/job1"}})
		case r.URL.Path == "/status/job1":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "succeeded", "outputs": []any{map[string]any{"image": map[string]any{"presignedUrl": "http://" + r.Host + "/out.png"}}}})
		case r.URL.Path == "/out.png":
			_, _ = w.Write([]byte("PNGDATA"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(fake.Close)
	previous := endpoints.imageSubmit
	endpoints.imageSubmit = fake.URL + "/generate-async"
	t.Cleanup(func() { endpoints.imageSubmit = previous })
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	token, err := cipher.Encrypt("header.e30.sig")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{SherlockMintAPI: fake.URL}, infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, nil)
	return fake, adapter, account.Credential{ID: 1, Provider: account.ProviderAdobe, AuthType: account.AuthTypeOAuth, EncryptedAccessToken: token}
}

// Submitted 标记只在请求可能已被 Adobe 接受时保留：明确 4xx 清掉（网关可换账号），
// 5xx 保留（结果未知，网关不会重提）。
func TestSubmitImageCheckpointSemantics(t *testing.T) {
	cases := []struct {
		submitStatus  int
		wantSubmitted bool
		wantUpstream  bool
	}{{0, true, true}, {400, false, false}, {500, true, false}}
	for _, tc := range cases {
		fake, adapter, credential := newFakeAdobe(t)
		fake.submitStatus = tc.submitStatus
		var saves []provider.ImageCheckpoint
		cp, err := adapter.SubmitImage(context.Background(), provider.ImageJobRequest{Credential: credential, Model: "gpt-image-2", Prompt: "cat"}, func(cp provider.ImageCheckpoint) error {
			saves = append(saves, cp)
			return nil
		})
		if (err == nil) != (tc.submitStatus == 0) || cp.Submitted != tc.wantSubmitted || (cp.UpstreamID != "") != tc.wantUpstream {
			t.Fatalf("status %d: cp=%+v err=%v", tc.submitStatus, cp, err)
		}
		if len(saves) == 0 || !saves[0].Submitted {
			t.Fatalf("status %d: Submitted must be saved before the send, saves=%+v", tc.submitStatus, saves)
		}
		if tc.submitStatus == 400 && !provider.IsRequestScopedError(err) {
			t.Fatalf("a rejected prompt must be request-scoped: %v", err)
		}
	}
}

// 同步路径：Adobe 接受任务前的拒绝必须是响应（网关据此刷新凭据/换账号），
// mint 不可用时请求根本不发出。
func TestGenerateImageRejectionsBecomeResponses(t *testing.T) {
	fake, adapter, credential := newFakeAdobe(t)
	fake.submitStatus = http.StatusUnauthorized
	response, err := adapter.GenerateImage(context.Background(), provider.ImageGenerationRequest{Credential: credential, Model: "gpt-image-2", Prompt: "cat"})
	if err != nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("401 submit => response %v err %v", response, err)
	}

	fake, adapter, credential = newFakeAdobe(t)
	fake.mintDown = true
	response, err = adapter.GenerateImage(context.Background(), provider.ImageGenerationRequest{Credential: credential, Model: "gpt-image-2", Prompt: "cat"})
	if err != nil || response.StatusCode != http.StatusServiceUnavailable || fake.submits.Load() != 0 {
		t.Fatalf("mint down => response %v err %v submits %d", response, err, fake.submits.Load())
	}

	fake, adapter, credential = newFakeAdobe(t)
	response, err = adapter.GenerateImage(context.Background(), provider.ImageGenerationRequest{Credential: credential, Model: "gpt-image-2", Prompt: "cat"})
	if err != nil || response.StatusCode != http.StatusOK || response.QuotaUnits != 1 {
		t.Fatalf("success => response %+v err %v", response, err)
	}
}

func TestPollToleratesTransientErrorsAndReportsFailure(t *testing.T) {
	previous := pollInterval
	pollInterval = time.Millisecond
	t.Cleanup(func() { pollInterval = previous })
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "FAILED", "message": "content policy"})
	}))
	defer server.Close()
	c := &client{do: server.Client(), rc: requestContext{token: "t"}}
	_, err := pollUntilDone(context.Background(), c, server.URL, "image", nil)
	if err == nil || !strings.Contains(err.Error(), "content policy") || hits.Load() != 3 {
		t.Fatalf("poll err = %v after %d hits", err, hits.Load())
	}
}

func TestRatioAndDurationSnapping(t *testing.T) {
	ratios := map[string]string{"": "1:1", "auto": "1:1", "1024x1024": "1:1", "1536x1024": "3:2", "1024x1536": "2:3", "1792x1024": "16:9", "4096x1024": "21:9"}
	for size, want := range ratios {
		if got := ratioFromSize(size, false); got != want {
			t.Fatalf("ratioFromSize(%q) = %s, want %s", size, got, want)
		}
	}
	if got := ratioFromSize("1024x4096", true); got != "1:4" {
		t.Fatalf("nano tall ratio = %s", got)
	}
	if got := nearestRatio("1:1", []string{"16:9", "9:16"}, "16:9"); got != "16:9" {
		t.Fatalf("nearestRatio = %s", got)
	}
	if got := nearestRatio("2:3", []string{"16:9", "9:16"}, "16:9"); got != "9:16" {
		t.Fatalf("portrait nearestRatio = %s", got)
	}
	if got := nearestDuration(8, []int{5, 10, 15}, 5); got != 10 {
		t.Fatalf("nearestDuration = %d", got)
	}
	// 网关对未填 duration 默认 8 秒：kling 不能因此被拒。
	if err := NewAdapter(Config{}, nil, nil, nil, nil).ValidateVideoRequest(provider.VideoRequestOptions{Operation: provider.VideoOperationGenerate, UpstreamModel: "kling3", Duration: 8, Resolution: "720p"}); err != nil {
		t.Fatalf("kling3 default duration rejected: %v", err)
	}
}

// 积分接口拿不到数值时不能写出"余额 0"窗口（落库会被当成上游耗尽）。
func TestSyncQuotaModeNeverInventsEmptyWindow(t *testing.T) {
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	previous := creditsURL
	creditsURL = server.URL
	t.Cleanup(func() { creditsURL = previous })
	_, adapter, credential := newFakeAdobe(t)
	if _, err := adapter.SyncQuotaMode(context.Background(), credential, quotaModeCredits); !errors.Is(err, account.ErrQuotaNotLimited) {
		t.Fatalf("no balance => %v, want ErrQuotaNotLimited", err)
	}
	status = http.StatusInternalServerError
	if window, err := adapter.SyncQuotaMode(context.Background(), credential, quotaModeCredits); err == nil {
		t.Fatalf("credits 500 => window %+v without error", window)
	}
}
