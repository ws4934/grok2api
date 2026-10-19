package adobe

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// validCookie 构造一个带完整 Sherlock 四字段的 refreshprofile Cookie。
func validCookie() string {
	token := base64.StdEncoding.EncodeToString([]byte(`{"sid":"s","ark":"a","bfp":"b","ftr":"f"}`))
	return "other=1; sherlockToken=" + token + "; last=2"
}

func TestDefinitionValidatesInRegistry(t *testing.T) {
	adapter := NewAdapter(Config{}, nil, nil, nil)
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

func TestExtractSherlock(t *testing.T) {
	if got := extractSherlock(validCookie()); got == "" || !validSherlock(got) {
		t.Fatalf("valid cookie yielded %q", got)
	}
	bad := "sherlockToken=" + base64.StdEncoding.EncodeToString([]byte(`{"sid":"s"}`))
	if got := extractSherlock(bad); got != "" {
		t.Fatalf("incomplete sherlock accepted: %q", got)
	}
	if got := extractSherlock("nocookie=1"); got != "" {
		t.Fatalf("missing sherlock accepted: %q", got)
	}
}

func TestParseImportedCredentials(t *testing.T) {
	adapter := NewAdapter(Config{}, nil, nil, nil)
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
	if err != nil {
		t.Fatal(err)
	}
	data, mime, err := c.download(context.Background(), out)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "PNGDATA" || !strings.HasPrefix(mime, "image/png") {
		t.Fatalf("download = %q %q", data, mime)
	}
}

func TestClientSubmitRequiresSherlock(t *testing.T) {
	c := &client{do: http.DefaultClient, rc: requestContext{token: "t", apiKey: "k"}}
	if _, err := c.submitImage(context.Background(), "http://127.0.0.1:0/x", []map[string]any{{}}, "p"); err == nil {
		t.Fatal("submit without sherlock must fail")
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

func TestClassifyUpstream(t *testing.T) {
	cases := []struct {
		status                  int
		body                    string
		permanent, quota, retry bool
	}{
		{401, "", true, false, false},
		{403, "", true, false, false},
		{402, "", false, true, false},
		{200, "insufficient credits", false, true, false},
		{429, "", false, false, true},
		{503, "", false, false, true},
	}
	for _, tc := range cases {
		err := classifyUpstream("submit", tc.status, tc.body)
		if err.permanent != tc.permanent || err.quota != tc.quota || err.retryable != tc.retry {
			t.Fatalf("status %d body %q => %+v", tc.status, tc.body, err)
		}
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
