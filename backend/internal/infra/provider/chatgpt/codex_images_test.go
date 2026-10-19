package chatgpt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// codexUpstream answers the Codex Images API and records each request.
func codexUpstream(t *testing.T, requests *[]*http.Request, bodies *[]map[string]any) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		*requests, *bodies = append(*requests, r), append(*bodies, body)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/backend-api/codex/images/generations", "/backend-api/codex/images/edits":
			_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"` + base64.StdEncoding.EncodeToString(testPNG()) + `"}]}`))
		case "/backend-api/wham/usage":
			_, _ = w.Write([]byte(`{"plan_type":"plus","rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":12,"limit_window_seconds":18000,"reset_at":1792417225},"secondary_window":{"used_percent":47.5,"limit_window_seconds":604800,"reset_at":1792924285}}}`))
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestCodexImageGenerationAndEdit(t *testing.T) {
	var requests []*http.Request
	var bodies []map[string]any
	a, assets := newTestAdapter(t, codexUpstream(t, &requests, &bodies))
	credential := testCredential(t, a, credentials{AccessToken: "AT"})
	credential.TeamID = "workspace-1"
	ctx := context.Background()

	response, err := a.GenerateImage(ctx, provider.ImageGenerationRequest{Credential: credential, Model: "codex-gpt-image-2.5", Prompt: "a fox", Size: "1024x1024", Quality: "high", Background: "opaque"})
	if err != nil || response.StatusCode != http.StatusOK || response.QuotaUnits != 1 || assets.saved != 1 {
		t.Fatalf("generation = %+v, %v", response, err)
	}
	if data := decodeBody(t, response)["data"].([]any); len(data) != 1 || data[0].(map[string]any)["b64_json"] == nil {
		t.Fatalf("data = %v", data)
	}
	r, body := requests[0], bodies[0]
	if r.URL.Path != "/backend-api/codex/images/generations" || r.Header.Get("Authorization") != "Bearer AT" || r.Header.Get("Chatgpt-Account-Id") != "workspace-1" || r.Header.Get("Originator") != codexOriginator ||
		body["model"] != "gpt-image-2.5" || body["prompt"] != "a fox" || body["size"] != "1024x1024" || body["quality"] != "high" || body["background"] != "opaque" || body["n"] != float64(1) {
		t.Fatalf("generation request %s %v %v", r.URL.Path, r.Header, body)
	}

	mask := "data:image/png;base64," + base64.StdEncoding.EncodeToString(testPNG())
	response, err = a.EditImage(ctx, provider.ImageEditRequest{Credential: credential, Model: "codex-gpt-image-2", Prompt: "add a hat", ImageURLs: []string{mask}, MaskURL: mask})
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("edit = %+v, %v", response, err)
	}
	body = bodies[1]
	images, _ := body["images"].([]any)
	if requests[1].URL.Path != "/backend-api/codex/images/edits" || body["model"] != "gpt-image-2" || len(images) != 1 ||
		images[0].(map[string]any)["image_url"] != mask || body["mask"].(map[string]any)["image_url"] != mask {
		t.Fatalf("edit request %s %v", requests[1].URL.Path, body)
	}
}

// The Codex usage windows become percentage windows; the tighter one routes
// Codex images, and a reached limit closes it.
func TestCodexQuotaWindows(t *testing.T) {
	var requests []*http.Request
	var bodies []map[string]any
	a, _ := newTestAdapter(t, codexUpstream(t, &requests, &bodies))
	credential := testCredential(t, a, credentials{AccessToken: "AT"})
	windows, err := a.codexQuotaWindows(context.Background(), credential, credentials{AccessToken: "AT"}, time.Now())
	if err != nil || len(windows) != 3 {
		t.Fatalf("windows = %+v, %v", windows, err)
	}
	byMode := map[string]account.QuotaWindow{}
	for _, window := range windows {
		byMode[window.Mode] = window
	}
	if w := byMode[quotaModeCodex5Hour]; w.Remaining != 88 || w.Total != 100 || w.WindowSeconds != 18000 || w.ResetAt == nil {
		t.Fatalf("5h window = %+v", w)
	}
	if w := byMode[quotaModeCodexWeekly]; w.Remaining != 52 || w.UsagePercent != 47.5 {
		t.Fatalf("weekly window = %+v", w)
	}
	if w := byMode[quotaModeCodex]; w.Remaining != 52 || w.WindowSeconds != 604800 {
		t.Fatalf("routing window = %+v", w)
	}

	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"rate_limit":{"allowed":false,"limit_reached":true,"primary_window":{"used_percent":100,"limit_window_seconds":18000,"reset_at":1792417225}}}`))
	}))
	t.Cleanup(limited.Close)
	a.image.BaseURL = limited.URL
	windows, err = a.codexQuotaWindows(context.Background(), credential, credentials{AccessToken: "AT"}, time.Now())
	if err != nil || len(windows) != 2 || windows[1].Mode != quotaModeCodex || windows[1].Remaining != 0 || windows[1].ResetAt == nil {
		t.Fatalf("limited windows = %+v, %v", windows, err)
	}
}

func TestCodexImageRoutes(t *testing.T) {
	a := NewAdapter(Config{}, nil, nil, nil)
	free := a.NormalizeAccountModelCapabilities([]string{"auto"}, nil, account.Credential{ID: 1, Plan: "free"})
	plus := a.NormalizeAccountModelCapabilities([]string{"auto"}, nil, account.Credential{ID: 2, Plan: "plus"})
	if slices.ContainsFunc(free, func(m string) bool { return strings.HasPrefix(m, codexImagePrefix) }) || !slices.Contains(plus, "codex-gpt-image-2.5-flare") {
		t.Fatalf("free = %v, plus = %v", free, plus)
	}
	if a.QuotaMode("codex-gpt-image-2") != quotaModeCodex || a.PricingModel("codex-gpt-image-2.5") != "gpt-image-2.5" || a.PricingModel("gpt-image-2") != "gpt-image-2" {
		t.Fatal("Codex image quota mode or pricing model")
	}
	if _, err := a.SubmitImage(context.Background(), provider.ImageJobRequest{Model: "codex-gpt-image-2", Prompt: "x"}, func(provider.ImageCheckpoint) error { return nil }); err == nil {
		t.Fatal("an image job accepted a Codex image model")
	}
}
