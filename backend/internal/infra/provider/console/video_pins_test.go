package console

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestConsoleVideo15SendsPinnedFramesAndAudioChoice(t *testing.T) {
	muted, audible := false, true
	for _, tc := range []struct {
		name  string
		last  bool
		pins  int
		audio *bool
		all   bool
	}{
		{"last-alone", true, 0, nil, false},
		{"keyframes-alone", false, 2, nil, false},
		{"silent-text", false, 0, &muted, false},
		{"audible-text", false, 0, &audible, false},
		{"all-twenty-silent", true, 4, &muted, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if serveTestDPoPToken(t, w, r) {
					return
				}
				verifyTestDPoPProof(t, r)
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodPost && r.URL.Path == "/v1/videos/generations" {
					posts++
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					audio, exists := body["generate_audio"]
					if exists != (tc.audio != nil) || (exists && audio != *tc.audio) {
						t.Errorf("audio=%v exists=%v", audio, exists)
					}
					last, exists := body["last_frame"].(map[string]any)
					if exists != tc.last || (tc.last && last["url"] != "data:image/png;base64,cGlu") {
						t.Errorf("last=%v", body["last_frame"])
					}
					pins, _ := body["keyframes"].([]any)
					if len(pins) != tc.pins {
						t.Errorf("pins=%v", pins)
					}
					for i, raw := range pins {
						pin := raw.(map[string]any)
						image := pin["image"].(map[string]any)
						if pin["timestamp_s"] != float64(i+1) || image["url"] != "data:image/png;base64,cGlu" {
							t.Errorf("pin=%v", pin)
						}
					}
					if tc.all {
						refs, _ := body["reference_images"].([]any)
						if len(refs) != 14 || body["image"] == nil {
							t.Errorf("full inputs=%v", body)
						}
					}
					if (tc.last || tc.pins > 0) && !tc.all {
						if _, exists := body["prompt"]; exists {
							t.Error("prompt was required/injected for a pinned-only request")
						}
					}
					_, _ = w.Write([]byte(`{"request_id":"pins"}`))
					return
				}
				_, _ = w.Write([]byte(`{"status":"done","video":{"url":"https://vidgen.x.ai/pins.mp4"}}`))
			}))
			t.Cleanup(server.Close)
			a, credential := newConsoleTestAdapter(t, server.URL)
			request := provider.VideoRequest{Credential: credential, Model: "grok-imagine-video-1.5", Duration: 8, Resolution: "720p", GenerateAudio: tc.audio}
			if tc.last {
				request.LastFrameURL = media.InputReference("input_" + strings.Repeat("A", 32))
			}
			for i := range tc.pins {
				request.Keyframes = append(request.Keyframes, media.VideoKeyframe{ImageURL: media.InputReference("input_" + strings.Repeat("A", 32)), TimestampSeconds: float64(i + 1)})
			}
			if tc.all {
				request.ImageURL = "https://example.com/first.png"
				for range 14 {
					request.ReferenceURLs = append(request.ReferenceURLs, "https://example.com/ref.png")
				}
				request.ReferenceAudios = []string{"eve"}
			}
			if !tc.last && tc.pins == 0 {
				request.Prompt = "landscape"
			}
			ctx := provider.WithInputOpener(context.Background(), func(context.Context, string) (provider.InputFile, error) {
				return provider.InputFile{Body: io.NopCloser(strings.NewReader("pin")), MIMEType: "image/png", Size: 3}, nil
			})
			result, err := a.GenerateVideo(ctx, request)
			if err != nil || posts != 1 || result.URL == "" {
				t.Fatalf("posts=%d result=%+v err=%v", posts, result, err)
			}
		})
	}
}

func TestConsoleVideoPinsAreModelSpecificAndFit720p(t *testing.T) {
	a, _ := newConsoleTestAdapter(t, "https://console.example")
	for _, options := range []provider.VideoRequestOptions{{HasLastFrame: true}, {Keyframes: 1}} {
		options.Operation = provider.VideoOperationGenerate
		options.UpstreamModel = "grok-imagine-video"
		options.Duration = 8
		options.Resolution = "1080p"
		if err := a.ValidateVideoRequest(options); err == nil {
			t.Fatal("classic accepted pinned frames")
		}
		options.UpstreamModel = "grok-imagine-video-1.5"
		got, err := a.NormalizeVideoRequest(options)
		if err != nil || got.Resolution != "720p" {
			t.Fatalf("options=%+v err=%v", got, err)
		}
	}
}
