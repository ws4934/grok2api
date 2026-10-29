package web

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	fhttp "github.com/bogdanfinn/fhttp"
	fhttptest "github.com/bogdanfinn/fhttp/httptest"
	"github.com/bogdanfinn/websocket"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/pkg/requestparams"
)

func TestImagineNSFWHotReloadIsSentAndRecordedWithoutDisablingModeration(t *testing.T) {
	for _, model := range []string{"grok-imagine-image-quality", "grok-imagine-image-2.0"} {
		t.Run(model, func(t *testing.T) {
			seen := make(chan bool, 2)
			server := fhttptest.NewServer(fhttp.HandlerFunc(func(w fhttp.ResponseWriter, r *fhttp.Request) {
				if r.URL.Path != "/ws/imagine/listen" {
					t.Errorf("unexpected account preference call: %s", r.URL.Path)
					fhttp.NotFound(w, r)
					return
				}
				conn, err := (&websocket.Upgrader{CheckOrigin: func(*fhttp.Request) bool { return true }}).Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				var reset, message map[string]any
				if err := conn.ReadJSON(&reset); err != nil {
					t.Error(err)
					return
				}
				if err := conn.ReadJSON(&message); err != nil {
					t.Error(err)
					return
				}
				content := message["item"].(map[string]any)["content"].([]any)[0].(map[string]any)
				properties := content["properties"].(map[string]any)
				flag, ok := properties["enable_nsfw"].(bool)
				if !ok {
					t.Errorf("missing flag: %+v", properties)
					return
				}
				seen <- flag
				id := content["requestId"].(string)
				_ = conn.WriteJSON(map[string]any{"type": "image", "id": "image", "requestId": id, "blob": "YWxsb3dlZA==", "percentage_complete": 100})
				_ = conn.WriteJSON(map[string]any{"type": "json", "image_id": "image", "requestId": id, "current_status": "completed", "moderated": flag})
			}))
			defer server.Close()
			a, credential := testMediaAdapter(t, server.URL)
			for _, want := range []bool{false, true} {
				cfg := a.config()
				cfg.AllowNSFW = want
				a.UpdateConfig(cfg)
				ctx, parameters := requestparams.Start(context.Background(), map[string]any{"model": model, "n": 1})
				response, err := a.GenerateImage(ctx, provider.ImageGenerationRequest{Credential: credential, Model: model, Prompt: "a lighthouse", Count: 1, ResponseFormat: "b64_json"})
				if want {
					status, _ := provider.ErrorHTTPStatus(err)
					if status != 400 || !provider.IsRequestScopedError(err) {
						t.Fatalf("NSFW flag bypassed moderation: response=%+v err=%v", response, err)
					}
				} else {
					if err != nil || response == nil {
						t.Fatalf("unmoderated output failed: %v", err)
					}
					_, readErr := io.ReadAll(response.Body)
					_ = response.Body.Close()
					if readErr != nil {
						t.Fatal(readErr)
					}
				}
				if got := <-seen; got != want {
					t.Fatalf("hot-reloaded flag=%v want=%v", got, want)
				}
				snapshot := parameters.Data().Upstream
				if snapshot == nil {
					t.Fatal("failed request lost its upstream parameters")
				}
				var recorded map[string]any
				if err := json.Unmarshal(snapshot.Values, &recorded); err != nil {
					t.Fatal(err)
				}
				text := string(snapshot.Values)
				flagText := `"enable_nsfw":false`
				if want {
					flagText = `"enable_nsfw":true`
				}
				if !strings.Contains(text, flagText) {
					t.Fatalf("wrong recorded flag: %s", text)
				}
			}
		})
	}
}
