package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestConsoleClassicVideoDropsReferenceVoices(t *testing.T) {
	for _, model := range []string{"", "grok-imagine-video"} {
		for _, frame := range []string{"", "https://example.com/first.png"} {
			t.Run(model+"/"+frame, func(t *testing.T) {
				posts := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if serveTestDPoPToken(t, w, r) {
						return
					}
					verifyTestDPoPProof(t, r)
					w.Header().Set("Content-Type", "application/json")
					if r.Method == http.MethodPost && r.URL.Path == "/v1/videos/generations" {
						posts++
						var payload map[string]any
						if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
							t.Error(err)
						}
						if _, exists := payload["reference_audios"]; exists {
							t.Errorf("classic forwarded unsupported voices: %v", payload)
						}
						if payload["model"] != "grok-imagine-video" {
							t.Errorf("model=%v", payload["model"])
						}
						_, _ = w.Write([]byte(`{"request_id":"classic"}`))
						return
					}
					_, _ = w.Write([]byte(`{"status":"done","progress":100,"video":{"url":"https://vidgen.x.ai/classic.mp4","duration":8}}`))
				}))
				t.Cleanup(server.Close)
				adapter, credential := newConsoleTestAdapter(t, server.URL)
				_, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{
					Credential: credential, Model: model, Prompt: "animate", Duration: 8,
					ImageURL: frame, ReferenceAudios: []string{"eve", "ara"}, Resolution: "720p",
				})
				if err != nil || posts != 1 {
					t.Fatalf("posts=%d err=%v", posts, err)
				}
			})
		}
	}
}
