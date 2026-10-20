package adobe

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bogdanfinn/tls-client/profiles"
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
	c := &client{do: server.Client(), rc: requestContext{token: "t", sherlock: "s", apiKey: "k", userAgent: infraegress.DefaultUserAgent}}
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
	c := &client{do: server.Client(), rc: requestContext{apiKey: "clio-playground-web", userAgent: infraegress.DefaultUserAgent}}
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

	mu         sync.Mutex
	uploads    []string       // 上传路径与 Content-Type
	videoBody  map[string]any // 最近一次视频提交的请求体
	videoNonce string         // 最近一次视频提交的 x-nonce
	agents     []string       // 发往 Adobe（mint 除外）的每个请求的 User-Agent 与 sec-ch-ua
}

func newFakeAdobe(t *testing.T) (*fakeAdobe, *Adapter, account.Credential) {
	t.Helper()
	fake := &fakeAdobe{}
	fake.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mint" {
			fake.mu.Lock()
			fake.agents = append(fake.agents, r.Header.Get("User-Agent")+" | "+r.Header.Get("sec-ch-ua"))
			fake.mu.Unlock()
		}
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
		case r.URL.Path == "/ims/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "header.e30.sig", "expires_in": 3600})
		case r.URL.Path == "/ims/profile":
			_ = json.NewEncoder(w).Encode(map[string]any{"email": "a@example.com", "userId": "u1"})
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
			fake.videoBody, fake.videoNonce = body, r.Header.Get("x-nonce")
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
	// 与生产一致：adobe 是扩展出口作用域，客户端在第一次请求时才建。
	scopes := egressdomain.ExtensionScopes()
	egressdomain.SetExtensionScopes([]egressdomain.Scope{scope})
	t.Cleanup(func() { egressdomain.SetExtensionScopes(scopes) })
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

// Adobe 以 408 拒绝唯一一次视频提交、重铸又失败时上游没有任务：按提交前失败结束并清掉
// 提交标记，而不是记成可能已提交（结果未知、无法续跑）。
func TestVideoRemintFailureCreatesNothing(t *testing.T) {
	fake, adapter, credential := newFakeAdobe(t)
	fake.mintDown, fake.submitStatus = true, http.StatusRequestTimeout
	endpoints.videoSubmit = fake.URL + "/generate-async"
	adapter.sherlock.token, adapter.sherlock.mintedAt = validSherlockToken(), time.Now() // 首次提交用缓存
	var saved []string
	_, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{
		Credential: credential, Model: "veo/3.1-generate", Prompt: "cat",
		SaveCheckpoint: func(value string) error { saved = append(saved, value); return nil },
	})
	stage, _ := provider.VideoErrorStage(err)
	if stage != provider.VideoStagePrepare || !errors.Is(err, errMintUnavailable) || fake.submits.Load() != 1 || len(saved) == 0 || saved[len(saved)-1] != "" {
		t.Fatalf("stage=%s submits=%d checkpoints=%q err=%v", stage, fake.submits.Load(), saved, err)
	}
}

// 下载失败的错误文本会成为任务的错误信息：不能带出预签名链接里的签名。
func TestDownloadVideoErrorOmitsSignedURL(t *testing.T) {
	_, adapter, credential := newFakeAdobe(t)
	_, _, _, err := adapter.DownloadVideo(context.Background(), credential, "http://127.0.0.1:1/out.mp4?X-Amz-Signature=SECRETSIG")
	if err == nil || strings.Contains(err.Error(), "SECRETSIG") {
		t.Fatalf("err = %v", err)
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

// 每个发往 Adobe 的请求都带账号自己的浏览器身份（UA 与对应的客户端提示），出口按同一个
// UA 选 TLS 指纹；凭据状态里没有身份的账号用池里第一个。
func TestRequestsCarryTheAccountUserAgent(t *testing.T) {
	fake, adapter, credential := newFakeAdobe(t)
	state, err := adapter.cipher.Encrypt(`{"user_agent":"` + browserUserAgents[3] + `"}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ state, agent, version string }{{"", browserUserAgents[0], "146"}, {state, browserUserAgents[3], "144"}} {
		credential.EncryptedState = tc.state
		fake.mu.Lock()
		fake.agents = nil
		fake.mu.Unlock()
		if _, err := adapter.GenerateImage(context.Background(), provider.ImageGenerationRequest{Credential: credential, Model: "gpt-image/2", Prompt: "cat"}); err != nil {
			t.Fatal(err)
		}
		fake.mu.Lock()
		want := tc.agent + fmt.Sprintf(` | "Chromium";v="%s", "Google Chrome";v="%s", "Not/A)Brand";v="99"`, tc.version, tc.version)
		if len(fake.agents) < 3 || slices.ContainsFunc(fake.agents, func(got string) bool { return got != want }) {
			t.Errorf("request agents = %q, want all %q", fake.agents, want)
		}
		fake.mu.Unlock()
	}
}

// 身份池里每个 UA 的 Chrome 版本都有对应的 tls-client 指纹，出口不会退回别的版本。
func TestBrowserUserAgentsHaveTLSProfiles(t *testing.T) {
	for _, agent := range browserUserAgents {
		major := regexp.MustCompile(`Chrome/(\d+)`).FindStringSubmatch(agent)
		if len(major) != 2 {
			t.Fatalf("not a Chrome user agent: %q", agent)
		}
		if _, ok := profiles.MappedTLSClients["chrome_"+major[1]]; !ok {
			t.Errorf("no tls-client profile for %q", agent)
		}
	}
}

// 导入时给新账号分一个身份并存进凭据状态；导出再导入、重新导入已有账号都保持原身份。
func TestImportKeepsAccountBrowserIdentity(t *testing.T) {
	fake, adapter, existing := newFakeAdobe(t)
	previousRefresh, previousProfiles := refreshURL, profileURLs
	refreshURL, profileURLs = fake.URL+"/ims/token", []string{fake.URL + "/ims/profile"}
	t.Cleanup(func() { refreshURL, profileURLs = previousRefresh, previousProfiles })
	ctx := context.Background()
	seeds, err := adapter.ParseImportedCredentials([]byte(validCookie()))
	if err != nil {
		t.Fatal(err)
	}
	seed, err := adapter.PrepareImportedCredential(ctx, seeds[0])
	agent := seed.State[stateUserAgent]
	if err != nil || !slices.Contains(browserUserAgents, agent) {
		t.Fatalf("new account identity = %q, %v", agent, err)
	}
	fake.mu.Lock()
	for _, got := range fake.agents {
		if !strings.HasPrefix(got, agent+" | ") {
			t.Errorf("import request agent = %q, want %q", got, agent)
		}
	}
	fake.mu.Unlock()

	exported, err := adapter.MarshalCredentials([]provider.CredentialSeed{seed})
	if err != nil {
		t.Fatal(err)
	}
	again, err := adapter.ParseImportedCredentials(exported)
	if err != nil {
		t.Fatal(err)
	}
	if roundTrip, err := adapter.PrepareImportedCredential(ctx, again[0]); err != nil || roundTrip.State[stateUserAgent] != agent {
		t.Fatalf("export/import identity = %q, %v, want %q", roundTrip.State[stateUserAgent], err, agent)
	}

	existing.EncryptedState, err = adapter.cipher.Encrypt(`{"user_agent":"` + browserUserAgents[2] + `"}`)
	if err != nil {
		t.Fatal(err)
	}
	reimported, err := adapter.PrepareImportedCredential(infraegress.WithExistingAccount(ctx, existing), seeds[0])
	if err != nil || reimported.State[stateUserAgent] != browserUserAgents[2] {
		t.Fatalf("re-import identity = %q, %v", reimported.State[stateUserAgent], err)
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

// 多张图的任务在 x-task-status 仍为 IN_PROGRESS 时就会带回先画好的几张，要等到 COMPLETED。
func TestPollWaitsForCompletedWhileOutputsArrive(t *testing.T) {
	previous := pollInterval
	pollInterval = time.Millisecond
	t.Cleanup(func() { pollInterval = previous })
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status, outputs := "IN_PROGRESS", []any{map[string]any{"image": map[string]any{"presignedUrl": "https://x/1.png"}}}
		if hits.Add(1) > 1 {
			status = "COMPLETED"
			outputs = append(outputs, map[string]any{"image": map[string]any{"presignedUrl": "https://x/2.png"}})
		}
		w.Header().Set("x-task-status", status)
		_ = json.NewEncoder(w).Encode(map[string]any{"outputs": outputs})
	}))
	defer server.Close()
	c := &client{do: server.Client(), rc: requestContext{token: "t"}}
	urls, err := pollUntilDone(context.Background(), c, server.URL, "image", nil)
	if err != nil || len(urls) != 2 || hits.Load() != 2 {
		t.Fatalf("poll = %v, %v after %d hits", urls, err, hits.Load())
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

// --- 目录与请求体 ---

// 手写目录：用户要的 15 个模型，路由的上游是 modelId/version；图像模型都能文生图与改图。
func TestCatalogModels(t *testing.T) {
	var slugs []string
	for _, v := range models {
		slugs = append(slugs, v.slug)
	}
	want := []string{"veo-3.1", "veo-3.1-fast", "veo-3.1-lite", "seedance-2.0", "seedance-2.0-fast", "seedance-2.5",
		"kling-3.0", "kling-3.0-omni", "kling-video-2.5-turbo", "nano-banana-pro", "nano-banana-2", "nano-banana-2.1",
		"gpt-image-2", "gpt-image-2.5-flare", "gpt-image-2.5-sunburst"}
	if !slices.Equal(slugs, want) {
		t.Fatalf("catalog = %q", slugs)
	}
	caps := map[string][]modeldomain.Capability{}
	for _, route := range Routes() {
		slug := strings.TrimPrefix(route.PublicID, "adobe/")
		caps[slug] = append(caps[slug], route.Capability)
		if v, ok := lookup(route.UpstreamModel); !ok || v.slug != slug {
			t.Errorf("route %s upstream %q does not resolve", route.PublicID, route.UpstreamModel)
		}
	}
	for _, v := range models {
		want := []modeldomain.Capability{modeldomain.CapabilityImage, modeldomain.CapabilityImageEdit}
		if v.video {
			want = []modeldomain.Capability{modeldomain.CapabilityVideo}
		}
		if !slices.Equal(caps[v.slug], want) {
			t.Errorf("%s capabilities = %v, want %v", v.slug, caps[v.slug], want)
		}
	}
}

// 没传的参数用模型默认值，传了不支持的就近对齐；参考图模式与首帧的强制规则照官网。
func TestNormalizeVideoRequest(t *testing.T) {
	a := &Adapter{}
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
		// 编辑的时长跟随源视频；Seedance 编辑、延长只有 auto 比例与 480p/720p。
		{"seedance edit", provider.VideoRequestOptions{Operation: provider.VideoOperationEdit, UpstreamModel: "seedance/seedance_2.5", AspectRatio: "9:16", Resolution: "1080p"}, "auto", "720p", 0},
		{"seedance extend default", provider.VideoRequestOptions{Operation: provider.VideoOperationExtend, UpstreamModel: "seedance/seedance_2.5"}, "auto", "720p", 4},
		{"seedance extend snaps", provider.VideoRequestOptions{Operation: provider.VideoOperationExtend, UpstreamModel: "seedance/seedance_2.5", Resolution: "480p", Duration: 40}, "auto", "480p", 30},
		{"kling omni edit", provider.VideoRequestOptions{Operation: provider.VideoOperationEdit, UpstreamModel: "kling/kling_v3_omni", AspectRatio: "9:16", Resolution: "1080p"}, "9:16", "1080p", 0},
	} {
		if test.in.Operation == "" {
			test.in.Operation = provider.VideoOperationGenerate
		}
		got, err := a.NormalizeVideoRequest(test.in)
		if err != nil || got.AspectRatio != test.aspect || got.Resolution != test.tier || got.Duration != test.duration {
			t.Errorf("%s = %s %s %ds (%v), want %s %s %ds", test.name, got.AspectRatio, got.Resolution, got.Duration, err, test.aspect, test.tier, test.duration)
		}
	}
}

// 编辑、延长只开放给表里挂了对应版本的模型；编辑传时长直接拒绝。
func TestVideoEditAndExtendSupport(t *testing.T) {
	a := &Adapter{}
	for _, test := range []struct {
		upstream  string
		operation provider.VideoOperation
		supported bool
	}{
		{"seedance/seedance_2.5", provider.VideoOperationEdit, true},
		{"seedance/seedance_2.5", provider.VideoOperationExtend, true},
		{"kling/kling_v3_omni", provider.VideoOperationEdit, true},
		{"kling/kling_v3_omni", provider.VideoOperationExtend, false},
		{"kling/kling_v3", provider.VideoOperationEdit, false},
		{"seedance/seedance_2.0", provider.VideoOperationEdit, false},
		{"veo/3.1-generate", provider.VideoOperationExtend, false},
	} {
		err := a.ValidateVideoRequest(provider.VideoRequestOptions{Operation: test.operation, UpstreamModel: test.upstream})
		if test.supported != (err == nil) || (!test.supported && !errors.Is(err, provider.ErrVideoOperationUnsupported)) {
			t.Errorf("%s %s: err = %v, want supported=%v", test.upstream, test.operation, err, test.supported)
		}
	}
	if _, err := a.NormalizeVideoRequest(provider.VideoRequestOptions{Operation: provider.VideoOperationEdit, UpstreamModel: "seedance/seedance_2.5", Duration: 5}); !errors.Is(err, provider.ErrVideoParameterInvalid) {
		t.Fatalf("edit with duration err = %v", err)
	}
}

func mustVersion(t *testing.T, slug string) *modelVersion {
	t.Helper()
	for _, v := range models {
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

	// 编辑：源视频以 source 用途引用，submodule 换成 ff-video-editor，不发时长。
	in = seedance.edit.videoPlan("make it snow", "", "", 0, false, false)
	in.videos = []string{"video1"}
	body = seedance.edit.buildBody(in)
	if body["modelVersion"] != "seedance_2.5_editing" || body["resolution"] != "720p" || get(body, "generationSettings", "aspectRatio") != "auto" ||
		get(body, "generationMetadata", "submodule") != "ff-video-editor" || get(body, "generationMetadata", "module") != "text2video" ||
		get(body["referenceBlobs"].([]any)[0].(map[string]any), "usage") != "source" || body["seeds"] == nil {
		t.Fatalf("seedance-2.5 edit body = %v", body)
	}
	if _, ok := body["duration"]; ok {
		t.Fatalf("seedance-2.5 edit sends duration: %v", body)
	}
	in = seedance.extend.videoPlan("keep going", "", "", 12, false, false)
	in.videos = []string{"video1"}
	if body = seedance.extend.buildBody(in); body["modelVersion"] != "seedance_2.5_extension" || body["duration"] != 12 {
		t.Fatalf("seedance-2.5 extend body = %v", body)
	}

	// Kling Omni 编辑：size 与 modelSpecificPayload 同生成，保留原音轨，不发时长、generateAudio、generationSettings。
	in = kling.edit.videoPlan("make it night", "9:16", "720p", 0, false, false)
	in.videos = []string{"video1"}
	body = kling.edit.buildBody(in)
	if body["modelVersion"] != "kling_v3_omni_v2v_edit" || body["keepAudio"] != true || body["resolution"] != "720p" ||
		get(body, "size", "width") != 720 || get(body, "modelSpecificPayload", "aspect_ratio") != "9:16" ||
		get(body, "generationMetadata", "submodule") != "ff-video-editor" || body["seeds"] == nil {
		t.Fatalf("kling-3.0-omni edit body = %v", body)
	}
	for _, key := range []string{"duration", "generateAudio", "generationSettings"} {
		if _, ok := body[key]; ok {
			t.Fatalf("kling-3.0-omni edit sends %s: %v", key, body)
		}
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
}

// 视频的首帧/参考图按模型能力校验，分辨率就近对齐。
func TestVideoInputsFollowSchema(t *testing.T) {
	adapter := NewAdapter(Config{}, nil, nil, nil, nil)
	check := func(slug string, hasImage bool, refs int) error {
		upstream := slug
		for _, v := range models {
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
	// 2K 取模型的最高档，不是回落到默认的 720p。
	if in := mustVersion(t, "veo-3.1").videoPlan("x", "16:9", "2K", 8, false, false); in.resolution != "1080p" {
		t.Errorf("veo-3.1 2K resolution = %q, want 1080p", in.resolution)
	}
}

// 官网约束：Veo 参考图规则；gpt-image-2.5 尺寸上限；音频默认值；Seedance 2.5 带首帧时比例为 auto。
func TestOfficialConstraintsFromReview(t *testing.T) {
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
}

func intOf2(v any) int {
	n, _ := v.(int)
	return n
}

// 尾帧：frame 上限 ≥2 的模型才收，作为 order 2 的 frame 发送。
func TestVideoLastFrame(t *testing.T) {
	for _, slug := range []string{"veo-3.1", "veo-3.1-lite", "kling-3.0", "seedance-2.0"} {
		if err := mustVersion(t, slug).checkVideoInputs(true, true, 0, 0); err != nil {
			t.Errorf("%s rejected a last frame: %v", slug, err)
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

// 输入视频只收官网接受的 MP4 与 QuickTime。
func TestReadInputVideoTypes(t *testing.T) {
	a := &Adapter{}
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
		_, got, err := a.readInput(context.Background(), "data:video/mp4;base64,"+base64.StdEncoding.EncodeToString(tc.data), "video")
		if got != tc.want || (err == nil) != (tc.want != "") {
			t.Errorf("%s: readInput = %q, %v; want %q", name, got, err, tc.want)
		}
	}
}

// 参考视频：只有 Seedance 接受，按数量上限与参考素材总数上限校验。
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
		{"veo-3.1", false, 0, 1, false},
	}
	for _, tc := range cases {
		err := mustVersion(t, tc.slug).checkVideoInputs(tc.frame, false, tc.refs, tc.clips)
		if (err == nil) != tc.ok {
			t.Errorf("%s frame=%v refs=%d videos=%d: err=%v, want ok=%v", tc.slug, tc.frame, tc.refs, tc.clips, err, tc.ok)
		}
	}
}

// 端到端：编辑、延长的源视频上传后以 source 用途提交到对应版本；Seedance 编辑不另生成音轨。
func TestGenerateVideoEditSubmitsSourceVideo(t *testing.T) {
	previous := pollInterval
	pollInterval = time.Millisecond
	t.Cleanup(func() { pollInterval = previous })
	clip := "data:video/mp4;base64," + base64.StdEncoding.EncodeToString(mp4Header())
	for _, test := range []struct {
		model, version, aspect, resolution string
		operation                          provider.VideoOperation
		duration                           int
		audio                              any
	}{
		{"seedance/seedance_2.5", "seedance_2.5_editing", "auto", "480p", provider.VideoOperationEdit, 0, false},
		{"seedance/seedance_2.5", "seedance_2.5_extension", "auto", "720p", provider.VideoOperationExtend, 12, true},
		{"kling/kling_v3_omni", "kling_v3_omni_v2v_edit", "9:16", "1080p", provider.VideoOperationEdit, 0, nil},
	} {
		fake, adapter, credential := newFakeAdobe(t)
		result, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{
			Credential: credential, Model: test.model, Operation: test.operation, Prompt: "make it snow",
			Duration: test.duration, AspectRatio: test.aspect, Resolution: test.resolution, VideoURL: clip,
		})
		if err != nil || !strings.HasSuffix(result.URL, "/out.mp4") {
			t.Fatalf("%s GenerateVideo = %+v, %v", test.version, result, err)
		}
		fake.mu.Lock()
		body := fake.videoBody
		refs, _ := body["referenceBlobs"].([]any)
		if len(fake.uploads) != 1 || fake.uploads[0] != "/storage/video video/mp4" || len(refs) != 1 || get(refs[0].(map[string]any), "usage") != "source" ||
			body["modelVersion"] != test.version || body["resolution"] != test.resolution || body["generateAudio"] != test.audio {
			t.Errorf("%s: uploads = %v, submitted body = %v", test.version, fake.uploads, body)
		}
		if duration, ok := body["duration"].(float64); ok != (test.duration > 0) || int(duration) != test.duration {
			t.Errorf("%s: duration = %v, want %d", test.version, body["duration"], test.duration)
		}
		fake.mu.Unlock()
	}
}

// 视频提交同样带官网的 x-nonce：sha256(`${userId}-${prompt.slice(0, 256)}`)，按 UTF-16 码元截取。
func TestVideoSubmitSendsTheWebNonce(t *testing.T) {
	previous := pollInterval
	pollInterval = time.Millisecond
	t.Cleanup(func() { pollInterval = previous })
	fake, adapter, credential := newFakeAdobe(t)
	claims, _ := json.Marshal(map[string]any{"user_id": "u1@AdobeID"})
	token, err := adapter.cipher.Encrypt("h." + base64.RawURLEncoding.EncodeToString(claims) + ".s")
	if err != nil {
		t.Fatal(err)
	}
	credential.EncryptedAccessToken = token
	// 第 256 个码元是 😀 的高位代理：官网把它切开，TextEncoder 编成 U+FFFD。
	prompt := strings.Repeat("猫", 255) + "😀尾"
	if _, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{Credential: credential, Model: "veo/3.1-generate", Prompt: prompt}); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("u1@AdobeID-" + strings.Repeat("猫", 255) + "�"))
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.videoNonce != hex.EncodeToString(sum[:]) {
		t.Fatalf("x-nonce = %q", fake.videoNonce)
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
