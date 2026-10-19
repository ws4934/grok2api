package adobe

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
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

	mu        sync.Mutex
	uploads   []string       // 上传路径与 Content-Type
	videoBody map[string]any // 最近一次视频提交的请求体
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
		case strings.HasPrefix(r.URL.Path, "/storage/"):
			fake.mu.Lock()
			fake.uploads = append(fake.uploads, r.URL.Path+" "+r.Header.Get("Content-Type"))
			n := len(fake.uploads)
			fake.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"id": fmt.Sprintf("asset%d", n)})
		case r.URL.Path == "/generate-video":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			fake.mu.Lock()
			fake.videoBody = body
			fake.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"links": map[string]any{"result": "http://" + r.Host + "/status/video1"}})
		case r.URL.Path == "/status/video1":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "succeeded", "outputs": []any{map[string]any{"video": map[string]any{"presignedUrl": "http://" + r.Host + "/out.mp4"}}}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(fake.Close)
	previous := endpoints
	endpoints.imageSubmit = fake.URL + "/generate-async"
	endpoints.videoSubmit = fake.URL + "/generate-video"
	endpoints.imageUpload = fake.URL + "/storage/image"
	endpoints.videoUpload = fake.URL + "/storage/video"
	t.Cleanup(func() { endpoints = previous })
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
		cp, err := adapter.SubmitImage(context.Background(), provider.ImageJobRequest{Credential: credential, Model: "gpt-image/2", Prompt: "cat"}, func(cp provider.ImageCheckpoint) error {
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
	response, err := adapter.GenerateImage(context.Background(), provider.ImageGenerationRequest{Credential: credential, Model: "gpt-image/2", Prompt: "cat"})
	if err != nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("401 submit => response %v err %v", response, err)
	}

	// 403 等网关不处理的拒绝保持为错误，账号照常降权。
	fake, adapter, credential = newFakeAdobe(t)
	fake.submitStatus = http.StatusForbidden
	if response, err = adapter.GenerateImage(context.Background(), provider.ImageGenerationRequest{Credential: credential, Model: "gpt-image/2", Prompt: "cat"}); err == nil {
		t.Fatalf("403 submit => response %v, want error", response)
	}

	fake, adapter, credential = newFakeAdobe(t)
	fake.mintDown = true
	response, err = adapter.GenerateImage(context.Background(), provider.ImageGenerationRequest{Credential: credential, Model: "gpt-image/2", Prompt: "cat"})
	if err != nil || response.StatusCode != http.StatusServiceUnavailable || fake.submits.Load() != 0 {
		t.Fatalf("mint down => response %v err %v submits %d", response, err, fake.submits.Load())
	}

	fake, adapter, credential = newFakeAdobe(t)
	response, err = adapter.GenerateImage(context.Background(), provider.ImageGenerationRequest{Credential: credential, Model: "gpt-image/2", Prompt: "cat"})
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
	slugs := map[string]bool{}
	for _, v := range c.versions {
		slugs[v.slug] = true
	}
	for _, slug := range []string{"nano-banana-pro", "nano-banana-2", "nano-banana-2.1", "gpt-image-1", "gpt-image-1.5", "gpt-image-2",
		"gpt-image-2.5-flare", "gpt-image-2.5-sunburst", "veo-3.1", "veo-3.1-fast", "veo-3.1-lite", "kling-3.0", "kling-3.0-omni",
		"seedance-2.0", "seedance-2.0-fast", "seedance-2.5", "gemini-omni-flash", "ray3.14", "flux-kontext-pro"} {
		if !slugs[slug] {
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
		caps[strings.TrimPrefix(route.PublicID, "adobe/")] = append(caps[strings.TrimPrefix(route.PublicID, "adobe/")], route.Capability)
		if route.UpstreamModel == "" || !strings.Contains(route.UpstreamModel, "/") {
			t.Errorf("route %s upstream %q is not modelId/version", route.PublicID, route.UpstreamModel)
		}
	}
	if got := caps["flux-1.1-pro-fill"]; len(got) != 1 || got[0] != modeldomain.CapabilityImageEdit {
		t.Errorf("inpainting-only model capabilities = %v, want image_edit only", got)
	}
	if got := caps["humain-image-1"]; len(got) != 1 || got[0] != modeldomain.CapabilityImage {
		t.Errorf("model without input images capabilities = %v, want image only", got)
	}
}

// 没传的参数用模型默认值，传了不支持的就近对齐；参考图模式与首帧的强制规则照官网。
func TestNormalizeVideoRequest(t *testing.T) {
	a := &Adapter{}
	a.catalog.Store(mustSnapshot())
	for _, test := range []struct {
		name     string
		in       provider.VideoRequestOptions
		aspect   string
		tier     string
		duration int
	}{
		{"kling 2.5 defaults", provider.VideoRequestOptions{UpstreamModel: "kling/kling_v2_5_turbo_pro_i2v", HasImage: true}, "16:9", "1080p", 5},
		{"veo snaps", provider.VideoRequestOptions{UpstreamModel: "veo/3.1-generate", AspectRatio: "21:9", Resolution: "4k", Duration: 10}, "16:9", "1080p", 8},
		{"veo references", provider.VideoRequestOptions{UpstreamModel: "veo/3.1-generate", ReferenceImages: 1, AspectRatio: "9:16", Duration: 4}, "16:9", "720p", 8},
		{"seedance 2.5 frame", provider.VideoRequestOptions{UpstreamModel: "seedance/seedance_2.5", HasImage: true, AspectRatio: "16:9", Duration: 30}, "auto", "720p", 30},
		{"seedance 2.0 auto", provider.VideoRequestOptions{UpstreamModel: "seedance/seedance_2.0", AspectRatio: "auto"}, "auto", "720p", 4},
	} {
		test.in.Operation = provider.VideoOperationGenerate
		got, err := a.NormalizeVideoRequest(test.in)
		if err != nil || got.AspectRatio != test.aspect || got.Resolution != test.tier || got.Duration != test.duration {
			t.Errorf("%s = %s %s %ds (%v), want %s %s %ds", test.name, got.AspectRatio, got.Resolution, got.Duration, err, test.aspect, test.tier, test.duration)
		}
	}
}

// 更新 discovery.json 后这里会失败：先对照官网核对新增或变化的模型（patches.go 里的字段
// 放置、参考图用途、尺寸表），再同步清单。线上新出现的模型会记 adobe_catalog_unreviewed_models。
func TestSnapshotModelsAreReviewed(t *testing.T) {
	reviewed := []string{
		"flux/1.1", "flux/2", "flux/flex-2", "flux/fluxFillPro", "flux/fluxKontextMax", "flux/fluxKontextPro", "flux/fluxPro-2", "flux/fluxUltra",
		"flux2-klein/klein_4b", "gemini-flash/nano-banana-2", "gemini-flash/nano-banana-2026-09", "gemini-flash/nano-banana-3",
		"gemini-omni/omni-flash", "gemini-omni/omni-flash-1.1", "gpt-4o-image/default", "gpt-image/1.5", "gpt-image/2",
		"gpt-image/gpt-image-2.5-flare", "gpt-image/gpt-image-2.5-prism", "humain/humain_image",
		"kling/kling_v2_5_turbo_pro_i2v", "kling/kling_v3", "kling/kling_v3_omni", "ltx/ltx-2.3-fast",
		"luma/2.0-ray", "luma/2.0-ray-flash", "luma/3.0-ray", "luma/3.0-ray-hdr", "luma/3.14-ray", "luma/3.14-ray-hdr",
		"runway/gen4.5", "runway/gen4_image", "runway/gen4_turbo", "seedance/seedance_2.0", "seedance/seedance_2.0_fast", "seedance/seedance_2.5",
		"veo/3.1-fast-generate", "veo/3.1-generate", "veo/3.1-lite-generate",
	}
	var got []string
	for _, v := range mustSnapshot().versions {
		got = append(got, v.upstream())
	}
	added := slices.DeleteFunc(slices.Clone(got), func(s string) bool { return slices.Contains(reviewed, s) })
	removed := slices.DeleteFunc(slices.Clone(reviewed), func(s string) bool { return slices.Contains(got, s) })
	if len(added)+len(removed) > 0 {
		t.Fatalf("快照模型有变化，先对照官网核对 patches.go 再同步清单：新增 %q，移除 %q", added, removed)
	}
}

func TestWarnUnreviewedModels(t *testing.T) {
	var logs bytes.Buffer
	a := &Adapter{logger: slog.New(slog.NewTextHandler(&logs, nil))}
	a.warnUnreviewed(mustSnapshot())
	if logs.Len() != 0 {
		t.Fatalf("snapshot models logged as unreviewed: %s", logs.String())
	}
	next := mustSnapshot()
	next.versions = append(next.versions, &modelVersion{modelID: "brand-new", version: "1"})
	a.warnUnreviewed(next)
	if !strings.Contains(logs.String(), "adobe_catalog_unreviewed_models") || !strings.Contains(logs.String(), "brand-new/1") || strings.Contains(logs.String(), "veo/") {
		t.Fatalf("log = %s", logs.String())
	}
}

func mustVersion(t *testing.T, slug string) *modelVersion {
	t.Helper()
	for _, v := range mustSnapshot().versions {
		if v.slug == slug {
			return v
		}
	}
	t.Fatalf("catalog lacks %s", slug)
	return nil
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
		get(body["referenceBlobs"].([]any)[0].(map[string]any), "usage") != "source" {
		t.Fatalf("gpt-image-2 auto body = %v", body)
	}

	veo := mustVersion(t, "veo-3.1")
	in = veo.videoPlan("a cat", "16:9", "1080p", 8, true, false)
	in.frames = []string{"frame1"}
	body = veo.buildBody(in)
	if body["duration"] != 8 || body["generateAudio"] != true || body["resolution"] != "1080p" || get(body, "size", "width") != 1920 ||
		get(body, "generationSettings", "aspectRatio") != "16:9" || get(body, "generationMetadata", "module") != "text2video" ||
		get(body["referenceBlobs"].([]any)[0].(map[string]any), "usage") != "frame" || body["seeds"] == nil {
		t.Fatalf("veo-3.1 body = %v", body)
	}

	kling := mustVersion(t, "kling-3.0-omni")
	in = kling.videoPlan("run", "16:9", "1080p", 10, true, false)
	in.frames = []string{"frame1"}
	body = kling.buildBody(in)
	if body["modelVersion"] != "kling_v3_omni" || body["duration"] != 10 || body["resolution"] != "1080p" || get(body, "size", "width") != 1920 ||
		get(body, "generationSettings", "aspectRatio") != "16:9" || get(body, "modelSpecificPayload", "resolution") != "1080p" ||
		get(body, "modelSpecificPayload", "aspect_ratio") != "16:9" || body["seeds"] == nil {
		t.Fatalf("kling-3.0-omni body = %v", body)
	}

	seedance := mustVersion(t, "seedance-2.5")
	in = seedance.videoPlan("wave", "auto", "720p", 8, false, false)
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
	check := func(slug string, hasImage bool, refs int) error {
		upstream := slug
		for _, v := range mustSnapshot().versions {
			if v.slug == slug {
				upstream = v.upstream()
			}
		}
		return adapter.ValidateVideoRequest(provider.VideoRequestOptions{Operation: provider.VideoOperationGenerate, UpstreamModel: upstream, HasImage: hasImage, ReferenceImages: refs, Duration: 8, Resolution: "720p"})
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
	if err := adapter.ValidateVideoRequest(provider.VideoRequestOptions{Operation: provider.VideoOperationGenerate, UpstreamModel: "veo/3.1-generate", ReferenceAudios: 1, Duration: 8}); err == nil {
		t.Error("Adobe accepted reference_audios at validation time")
	}
	in := mustVersion(t, "kling-video-2.5-turbo").videoPlan("x", "16:9", "720p", 8, true, false)
	if in.resolution != "1080p" {
		t.Errorf("1080p-only model resolution = %q", in.resolution)
	}
	if body := mustVersion(t, "gemini-omni-flash").buildBody(mustVersion(t, "gemini-omni-flash").videoPlan("x", "16:9", "1080p", 15, false, false)); body["duration"] != 10 || body["resolution"] != "720p" {
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
	if err != nil || len(routes) == 0 || len(routes) != len(adapter.catalog.Load().routes()) {
		t.Fatalf("refresh = %d routes, %v", len(routes), err)
	}
	payload = []byte(`{"models":[],"commonRequestSchema":{}}`)
	if _, err := adapter.RefreshCatalog(context.Background()); err == nil {
		t.Fatal("an empty discovery must not replace the catalog")
	}
	if len(adapter.catalog.Load().routes()) != len(routes) {
		t.Fatal("a rejected refresh changed the catalog")
	}
}

// 审查修复：deprecated 用途走 replacement；Veo 参考图约束；gpt-image-2.5 尺寸上限；
// 音频默认值跟随 schema；Seedance 2.5 带首帧时比例为 auto。
func TestOfficialConstraintsFromReview(t *testing.T) {
	if usage, _ := mustVersion(t, "runway-gen-4-video").referenceUsage(); usage != "" {
		t.Errorf("runway general is deprecated (now frame), reference usage = %q", usage)
	}
	if usage, _ := mustVersion(t, "flux-1.1-pro").referenceUsage(); usage != "style" {
		t.Errorf("flux-1.1-pro reference usage = %q, want style", usage)
	}
	if err := mustVersion(t, "veo-3.1-fast").checkVideoInputs(false, false, 1, 0); err == nil {
		t.Error("veo-3.1-fast accepted reference images")
	}
	if in := mustVersion(t, "veo-3.1").videoPlan("x", "9:16", "720p", 4, false, true); in.aspect != "16:9" || in.duration != 8 {
		t.Errorf("veo-3.1 with references = %s %ds, want 16:9 8s", in.aspect, in.duration)
	}
	// 官网公式在 21:9@2K 得到 4784 宽，超出 3840：按比例缩进上限而不是发越界尺寸。
	flare := mustVersion(t, "gpt-image-2.5-flare")
	if in, err := flare.imagePlan("x", "", "21:9", "2K", "", "", 1, 0, false); err != nil {
		t.Error(err)
	} else if size, _ := flare.pickSize(in.aspect, in.resolution); size != (pixels{3840, 1632}) {
		t.Errorf("gpt-image-2.5 21:9@2K size = %v, want 3840x1632", size)
	}
	kling := mustVersion(t, "kling-3.0")
	if body := kling.buildBody(kling.videoPlan("x", "16:9", "720p", 5, false, false)); body["generateAudio"] != false {
		t.Errorf("kling-3.0 generateAudio = %v, want schema default false", body["generateAudio"])
	}
	veo := mustVersion(t, "veo-3.1")
	if body := veo.buildBody(veo.videoPlan("x", "16:9", "720p", 8, false, false)); body["generateAudio"] != true {
		t.Errorf("veo-3.1 generateAudio = %v, want true", body["generateAudio"])
	}
	if in := mustVersion(t, "seedance-2.5").videoPlan("x", "16:9", "720p", 5, true, false); in.aspect != "auto" {
		t.Errorf("seedance-2.5 with a first frame aspect = %q, want auto", in.aspect)
	}
}

// 4K：Nano Banana 用 schema 的 4K 尺寸；gpt-image-2/2.5 取 schema 上限内的最大尺寸，
// gpt-image-2 的 1K 仍用官网尺寸表。
func TestImage4K(t *testing.T) {
	cases := []struct {
		slug, aspect, resolution string
		want                     pixels
	}{
		{"nano-banana-pro", "16:9", "4K", pixels{5504, 3072}},
		{"nano-banana-2", "1:1", "4K", pixels{4096, 4096}},
		{"gpt-image-2", "16:9", "4K", pixels{3840, 2160}},
		{"gpt-image-2", "1:1", "4K", pixels{2880, 2880}},
		{"gpt-image-2", "16:9", "2K", pixels{3648, 2048}},
		{"gpt-image-2", "16:9", "1K", pixels{1376, 768}},
		{"gpt-image-2.5-sunburst", "9:16", "4K", pixels{2160, 3840}},
		{"gpt-image-2.5-sunburst", "3:2", "4K", pixels{3520, 2336}},
	}
	for _, tc := range cases {
		v := mustVersion(t, tc.slug)
		in, err := v.imagePlan("x", "", tc.aspect, tc.resolution, "", "", 1, 0, false)
		if err != nil {
			t.Errorf("%s %s@%s: %v", tc.slug, tc.aspect, tc.resolution, err)
			continue
		}
		body := v.buildBody(in)
		if got := (pixels{intOf2(get(body, "size", "width")), intOf2(get(body, "size", "height"))}); got != tc.want {
			t.Errorf("%s %s@%s size = %v, want %v", tc.slug, tc.aspect, tc.resolution, got, tc.want)
		}
	}
	if _, err := mustVersion(t, "flux-2-pro").imagePlan("x", "", "", "4K", "", "", 1, 0, false); err == nil {
		t.Error("flux-2-pro (max 1920) accepted 4K")
	}
}

func intOf2(v any) int {
	n, _ := v.(int)
	return n
}

// 尾帧：frame 上限 ≥2 的模型才收，作为 order 2 的 frame 发送。
func TestVideoLastFrame(t *testing.T) {
	// Luma Ray 的参考素材总数上限是 1 且帧计入，只能单帧。
	for slug, ok := range map[string]bool{"veo-3.1": true, "veo-3.1-lite": true, "kling-3.0": true, "seedance-2.0": true, "gemini-omni-flash": false, "runway-gen-4.5": false, "ray2": false, "ray3.14": false} {
		if err := mustVersion(t, slug).checkVideoInputs(true, true, 0, 0); (err == nil) != ok {
			t.Errorf("%s last frame accepted = %v, want %v (%v)", slug, err == nil, ok, err)
		}
	}
	veo := mustVersion(t, "veo-3.1")
	in := veo.videoPlan("x", "16:9", "720p", 8, true, false)
	in.frames = []string{"first", "last"}
	refs, _ := veo.buildBody(in)["referenceBlobs"].([]any)
	if len(refs) != 2 || get(refs[1].(map[string]any), "order") != 2 || get(refs[1].(map[string]any), "usage") != "frame" {
		t.Fatalf("frames = %v", refs)
	}
}

// mp4Header 是一个最小的 MP4 ftyp 盒（isom 主品牌，兼容 mp41），后面补些数据。
func mp4Header() []byte {
	box := []byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm', 0, 0, 2, 0, 'i', 's', 'o', 'm', 'm', 'p', '4', '1'}
	return append(box, make([]byte, 64)...)
}

func TestVideoMIME(t *testing.T) {
	quicktime := append([]byte{0, 0, 0, 20, 'f', 't', 'y', 'p', 'q', 't', ' ', ' ', 0, 0, 0, 0, 'q', 't', ' ', ' '}, make([]byte, 32)...)
	for name, tc := range map[string]struct {
		data []byte
		want string
	}{
		"mp4": {mp4Header(), "video/mp4"}, "quicktime": {quicktime, "video/quicktime"}, "png": {[]byte("\x89PNG\r\n\x1a\n0000"), ""},
		// HEIC 同样以 ftyp 开头，不能当成 MP4。
		"heic": {append([]byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'h', 'e', 'i', 'c', 0, 0, 0, 0, 'm', 'i', 'f', '1', 'h', 'e', 'i', 'c'}, make([]byte, 32)...), ""},
		"webm": {append([]byte{0x1a, 0x45, 0xdf, 0xa3}, []byte("....webm............")...), ""},
	} {
		if got := videoMIME(tc.data); got != tc.want {
			t.Errorf("%s: videoMIME = %q, want %q", name, got, tc.want)
		}
	}
}

// 参考视频：只有 schema 带 video:source 且补丁确认是生成参考的模型接受，按数量上限与
// 参考素材总数上限校验。
func TestReferenceVideoLimits(t *testing.T) {
	cases := []struct {
		slug        string
		frame       bool
		refs, clips int
		ok          bool
	}{
		{"seedance-2.0", false, 0, 3, true},
		{"seedance-2.0", false, 0, 4, false},
		{"seedance-2.0", true, 0, 1, false}, // Seedance 的首尾帧与参考素材互斥（官网同样拦截）
		{"seedance-2.5", false, 9, 3, true}, // 官网 UI 只开放 9 张图、3 个视频
		{"seedance-2.5", false, 0, 4, false},
		{"seedance-2.5", false, 10, 0, false},
		{"gemini-omni-flash", false, 3, 1, true},
		{"gemini-omni-flash", true, 3, 1, true},   // Omni 的首帧不计入参考素材池
		{"gemini-omni-flash", false, 4, 1, false}, // 官网总数上限 4
		{"gemini-omni-flash", false, 0, 2, false},
		{"gemini-omni-flash-1.1", true, 8, 2, false}, // schema 总数上限 10，首帧计入
		{"veo-3.1", false, 0, 1, false},
		{"ray3", false, 0, 1, false}, // Luma 的源视频是编辑用途
	}
	for _, tc := range cases {
		err := mustVersion(t, tc.slug).checkVideoInputs(tc.frame, false, tc.refs, tc.clips)
		if (err == nil) != tc.ok {
			t.Errorf("%s frame=%v refs=%d videos=%d: err=%v, want ok=%v", tc.slug, tc.frame, tc.refs, tc.clips, err, tc.ok)
		}
	}
}

// 端到端：参考视频上传到 /v2/storage/video，并以 source 用途进入提交的请求体。
func TestGenerateVideoUploadsReferenceVideos(t *testing.T) {
	previous := pollInterval
	pollInterval = time.Millisecond
	t.Cleanup(func() { pollInterval = previous })
	fake, adapter, credential := newFakeAdobe(t)
	clip := "data:video/mp4;base64," + base64.StdEncoding.EncodeToString(mp4Header())
	upstream := mustVersion(t, "seedance-2.0").upstream()
	result, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{
		Credential: credential, Model: upstream, Prompt: "dance like the clip", Duration: 5, AspectRatio: "16:9", Resolution: "720p",
		ReferenceVideoURLs: []string{clip},
	})
	if err != nil || !strings.HasSuffix(result.URL, "/out.mp4") {
		t.Fatalf("GenerateVideo = %+v, %v", result, err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.uploads) != 1 || fake.uploads[0] != "/storage/video video/mp4" {
		t.Fatalf("uploads = %v", fake.uploads)
	}
	refs, _ := fake.videoBody["referenceBlobs"].([]any)
	if len(refs) != 1 || get(refs[0].(map[string]any), "usage") != "source" || get(refs[0].(map[string]any), "id") != "asset1" ||
		fake.videoBody["modelVersion"] != "seedance_2.0" {
		t.Fatalf("submitted body = %v", fake.videoBody)
	}
}
