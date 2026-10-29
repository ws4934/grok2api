package inference

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestVideoPinnedFramesHTTPShapeAndAudioBoolean(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	NewHandler(nil, nil, 1<<20).Register(router.Group("/v1"))
	for _, tc := range []struct {
		name, path, fields string
		status             int
	}{
		{"tail-only", "generations", `"last_frame":{"url":"https://example.com/last.png"}`, 401},
		{"key-only", "generations", `"keyframes":[{"image":{"file_id":"input_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},"timestamp_s":0.333}],"generate_audio":false`, 401},
		{"first-and-references-without-prompt", "generations", `"image":{"url":"https://example.com/first.png"},"reference_images":[{"url":"https://example.com/ref.png"}]`, 401},
		{"bad-boolean", "generations", `"prompt":"x","generate_audio":"false"`, 400},
		{"bad-image", "generations", `"keyframes":[{"timestamp_s":2}]`, 400},
		{"bad-time-type", "generations", `"keyframes":[{"image":{"url":"https://example.com/key.png"},"timestamp_s":"2"}]`, 400},
		{"too-many", "generations", `"keyframes":[{},{},{},{},{}]`, 400},
		{"edit-audio", "edits", `"prompt":"x","video":{"url":"https://example.com/source.mp4"},"generate_audio":false`, 400},
		{"extend-pins", "extensions", `"prompt":"x","video":{"url":"https://example.com/source.mp4"},"keyframes":[{"image":{"url":"https://example.com/key.png"},"timestamp_s":2}]`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/videos/"+tc.path, strings.NewReader(`{"model":"video15",`+tc.fields+`}`))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			var envelope struct {
				Error struct {
					Message, Type, Code string
					Param               any
				}
			}
			if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil || envelope.Error.Message == "" || envelope.Error.Type == "" || envelope.Error.Code == "" {
				t.Fatalf("non-protocol error: %s err=%v", w.Body.String(), err)
			}
		})
	}
	for _, value := range []string{"false", "true"} {
		var dto videoGenerationRequest
		if err := json.Unmarshal([]byte(`{"generate_audio":`+value+`}`), &dto); err != nil || dto.GenerateAudio == nil || *dto.GenerateAudio != (value == "true") {
			t.Fatalf("audio=%+v err=%v", dto.GenerateAudio, err)
		}
	}
}
