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
	sub, err := c.submitJob(context.Background(), server.URL+"/v2/3p-images/generate-async", map[string]any{"prompt": "x"}, "x")
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
		// 请求级拒绝里出现 "limit exceeded"/"quota" 不能被当成积分耗尽（会耗尽一串账号）。
		{413, `{"message":"Request size limit exceeded"}`, 413, true},
		{422, `{"message":"quota"}`, 422, true},
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
	if classifyUpstream("submit", 429, `{"message":"Rate limit exceeded"}`).quota {
		t.Fatal("a plain rate limit is not exhausted credits")
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
	}{{0, true, true}, {400, false, false}, {500, true, false}, {408, false, false}}
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
		// 408 重铸后仍被拒：问题在 mint 环境，请求级拒绝，不让每个账号都挨一次。
		if tc.submitStatus == 408 && (!errors.Is(err, errSessionRejected) || fake.submits.Load() != 2) {
			t.Fatalf("repeated 408 => %v after %d submits", err, fake.submits.Load())
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

	// 403 等网关不处理的拒绝保持为错误，账号照常降权。
	fake, adapter, credential = newFakeAdobe(t)
	fake.submitStatus = http.StatusForbidden
	if response, err = adapter.GenerateImage(context.Background(), provider.ImageGenerationRequest{Credential: credential, Model: "gpt-image-2", Prompt: "cat"}); err == nil {
		t.Fatalf("403 submit => response %v, want error", response)
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

// --- schema 驱动的目录与请求体 ---

func TestSnapshotCatalogMatchesOfficialModels(t *testing.T) {
	c := mustSnapshot()
	for _, slug := range []string{"nano-banana-pro", "nano-banana-2", "nano-banana-2.1", "gpt-image-1", "gpt-image-1.5", "gpt-image-2",
		"gpt-image-2.5-flare", "gpt-image-2.5-sunburst", "veo-3.1", "veo-3.1-fast", "veo-3.1-lite", "kling-3.0", "kling-3.0-omni",
		"seedance-2.0", "seedance-2.0-fast", "seedance-2.5", "gemini-omni-flash", "ray3.14", "flux-kontext-pro"} {
		if _, ok := c.lookup(slug); !ok {
			t.Errorf("catalog lacks %s", slug)
		}
	}
	for _, v := range c.versions {
		// 下架（Sora、Imagen、Veo 2/3）、增强类（Topaz）、Gemini 2.5、Kling 拆分版本与视频编辑版本都不应出现。
		if v.slug == "nano-banana" || strings.Contains(v.slug, "sora") || strings.Contains(v.slug, "imagen") || strings.Contains(v.slug, "topaz") ||
			strings.Contains(v.version, "_t2v") || strings.Contains(v.version, "v2v") || strings.Contains(v.version, "editing") || v.version == "3.0-generate" {
			t.Errorf("unexpected model %s (%s/%s)", v.slug, v.modelID, v.version)
		}
	}
	caps := map[string][]modeldomain.Capability{}
	for _, route := range c.routes() {
		caps[route.UpstreamModel] = append(caps[route.UpstreamModel], route.Capability)
	}
	if got := caps["flux-1.1-pro-fill"]; len(got) != 1 || got[0] != modeldomain.CapabilityImageEdit {
		t.Errorf("inpainting-only model capabilities = %v, want image_edit only", got)
	}
	if got := caps["humain-image-1"]; len(got) != 1 || got[0] != modeldomain.CapabilityImage {
		t.Errorf("model without input images capabilities = %v, want image only", got)
	}
}

func mustVersion(t *testing.T, slug string) *modelVersion {
	t.Helper()
	v, ok := mustSnapshot().lookup(slug)
	if !ok {
		t.Fatalf("catalog lacks %s", slug)
	}
	return v
}

func get(body map[string]any, path ...string) any {
	var current any = body
	for _, key := range path {
		m, _ := current.(map[string]any)
		current = m[key]
	}
	return current
}

// 复现官网前端对同一组参数拼出的请求体（SPEC-payload 的 worked bodies）。
func TestBuildBodyMatchesOfficialClient(t *testing.T) {
	nano := mustVersion(t, "nano-banana-pro")
	in, err := nano.imagePlan("cat", "", "16:9", "2K", "", "", 1, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	in.references = []string{"img1"}
	body := nano.buildBody(in)
	if body["modelId"] != "gemini-flash" || body["modelVersion"] != "nano-banana-2" || body["resolution"] != "2K" || body["groundSearch"] != false ||
		get(body, "size", "width") != 2752 || get(body, "size", "height") != 1536 ||
		get(body, "generationSettings", "aspectRatio") != "16:9" || get(body, "modelSpecificPayload", "aspectRatio") != "16:9" ||
		get(body, "generationMetadata", "module") != "text2image" {
		t.Fatalf("nano-banana-pro body = %v", body)
	}
	if refs := body["referenceBlobs"].([]any); get(refs[0].(map[string]any), "usage") != "general" {
		t.Fatalf("nano-banana-pro reference usage = %v", refs)
	}

	gpt := mustVersion(t, "gpt-image-2")
	in, err = gpt.imagePlan("cat", "", "auto", "", "high", "", 1, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	in.references = []string{"img1"}
	body = gpt.buildBody(in)
	if _, ok := body["size"]; ok || get(body, "generationSettings", "detailLevel") != 5 || get(body, "generationSettings", "aspectRatio") != nil ||
		get(body["referenceBlobs"].([]any)[0].(map[string]any), "usage") != "subject" {
		t.Fatalf("gpt-image-2 auto body = %v", body)
	}

	veo := mustVersion(t, "veo-3.1")
	in = veo.videoPlan("a cat", "16:9", "1080p", 8, false)
	in.frames = []string{"frame1"}
	body = veo.buildBody(in)
	if body["duration"] != 8 || body["generateAudio"] != true || body["resolution"] != "1080p" || get(body, "size", "width") != 1920 ||
		get(body, "generationSettings", "aspectRatio") != "16:9" || get(body, "generationMetadata", "module") != "text2video" ||
		get(body["referenceBlobs"].([]any)[0].(map[string]any), "usage") != "frame" || body["seeds"] == nil {
		t.Fatalf("veo-3.1 body = %v", body)
	}

	kling := mustVersion(t, "kling-3.0-omni")
	in = kling.videoPlan("run", "16:9", "1080p", 10, false)
	in.frames = []string{"frame1"}
	body = kling.buildBody(in)
	if body["modelVersion"] != "kling_v3_omni" || body["duration"] != 10 || body["resolution"] != "1080p" || get(body, "size", "width") != 1920 ||
		get(body, "generationSettings", "aspectRatio") != "16:9" || get(body, "modelSpecificPayload", "resolution") != "1080p" ||
		get(body, "modelSpecificPayload", "aspect_ratio") != "16:9" || body["seeds"] == nil {
		t.Fatalf("kling-3.0-omni body = %v", body)
	}

	seedance := mustVersion(t, "seedance-2.5")
	in = seedance.videoPlan("wave", "auto", "720p", 8, false)
	body = seedance.buildBody(in)
	if _, ok := body["size"]; ok || body["resolution"] != "720p" || get(body, "generationSettings", "aspectRatio") != "auto" {
		t.Fatalf("seedance-2.5 body = %v", body)
	}
}

// 显式给出但模型不支持的参数直接 400；张数超过 schema 上限也拒绝。
func TestImagePlanRejectsUnsupportedParameters(t *testing.T) {
	nano := mustVersion(t, "nano-banana-pro")
	if _, err := nano.imagePlan("x", "", "8:1", "", "", "", 1, 0, false); err == nil {
		t.Error("nano-banana-pro accepted 8:1")
	}
	if _, err := nano.imagePlan("x", "", "", "512", "", "", 1, 0, false); err == nil {
		t.Error("nano-banana-pro accepted 512")
	}
	if _, err := mustVersion(t, "nano-banana-2").imagePlan("x", "", "8:1", "512", "", "", 1, 0, false); err != nil {
		t.Errorf("nano-banana-2 rejected 8:1@512: %v", err)
	}
	if _, err := mustVersion(t, "gpt-image-2").imagePlan("x", "", "", "", "", "transparent", 1, 0, false); err == nil {
		t.Error("gpt-image-2 accepted a transparent background")
	}
	in, err := mustVersion(t, "gpt-image-2.5-flare").imagePlan("x", "1536x1024", "", "", "", "transparent", 1, 0, false)
	if err != nil || !in.transparent || in.aspect != "3:2" {
		t.Errorf("gpt-image-2.5-flare plan = %+v, %v", in, err)
	}
	if _, err := mustVersion(t, "humain-image-1").imagePlan("x", "", "", "", "", "", 2, 0, false); err == nil {
		t.Error("model without n accepted 2 images")
	}
}

// 视频的首帧/参考图按 schema 能力校验，时长与分辨率就近对齐（网关默认 8 秒、720p）。
func TestVideoInputsFollowSchema(t *testing.T) {
	adapter := NewAdapter(Config{}, nil, nil, nil, nil)
	check := func(model string, hasImage bool, refs int) error {
		return adapter.ValidateVideoRequest(provider.VideoRequestOptions{Operation: provider.VideoOperationGenerate, UpstreamModel: model, HasImage: hasImage, ReferenceImages: refs, Duration: 8, Resolution: "720p"})
	}
	if err := check("kling-3.0", false, 2); err == nil {
		t.Error("kling-3.0 accepted reference images")
	}
	if err := check("kling-3.0-omni", false, 3); err != nil {
		t.Errorf("kling-3.0-omni rejected 3 references: %v", err)
	}
	if err := check("kling-3.0-omni", false, 4); err == nil {
		t.Error("kling-3.0-omni accepted 4 references")
	}
	if err := check("kling-video-2.5-turbo", false, 0); err == nil {
		t.Error("image-to-video model accepted a request without a first frame")
	}
	if err := check("sora2", false, 0); err == nil {
		t.Error("removed model sora2 accepted")
	}
	in := mustVersion(t, "kling-video-2.5-turbo").videoPlan("x", "16:9", "720p", 8, false)
	if in.resolution != "1080p" {
		t.Errorf("1080p-only model resolution = %q", in.resolution)
	}
	if body := mustVersion(t, "gemini-omni-flash").buildBody(mustVersion(t, "gemini-omni-flash").videoPlan("x", "16:9", "1080p", 15, false)); body["duration"] != 10 || body["resolution"] != "720p" {
		t.Errorf("omni-flash duration/resolution = %v / %v", body["duration"], body["resolution"])
	}
}

func TestRefreshCatalogSwapsAndGuardsShrink(t *testing.T) {
	payload := discoverySnapshot
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	previous := discoveryURL
	discoveryURL = server.URL
	t.Cleanup(func() { discoveryURL = previous })
	_, adapter, _ := newFakeAdobe(t)
	routes, err := adapter.RefreshCatalog(context.Background())
	if err != nil || len(routes) == 0 || len(routes) != len(adapter.Routes()) {
		t.Fatalf("refresh = %d routes, %v", len(routes), err)
	}
	payload = []byte(`{"models":[],"commonRequestSchema":{}}`)
	if _, err := adapter.RefreshCatalog(context.Background()); err == nil {
		t.Fatal("an empty discovery must not replace the catalog")
	}
	if len(adapter.Routes()) != len(routes) {
		t.Fatal("a rejected refresh changed the catalog")
	}
}
