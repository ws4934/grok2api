package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	fhttp "github.com/bogdanfinn/fhttp"
	fhttptest "github.com/bogdanfinn/fhttp/httptest"
	"github.com/bogdanfinn/websocket"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestImagineModeratedResultsAreExplicitPolicyFailures(t *testing.T) {
	for _, model := range []string{"grok-imagine-image-quality", "grok-imagine-image-2.0"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", model, stream), func(t *testing.T) {
				var generations atomic.Int32
				server := fhttptest.NewServer(fhttp.HandlerFunc(func(w fhttp.ResponseWriter, r *fhttp.Request) {
					if r.URL.Path != "/ws/imagine/listen" {
						fhttp.NotFound(w, r)
						return
					}
					connection, err := (&websocket.Upgrader{CheckOrigin: func(*fhttp.Request) bool { return true }}).Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer connection.Close()
					var reset, request map[string]any
					if err := connection.ReadJSON(&reset); err != nil {
						t.Error(err)
						return
					}
					if err := connection.ReadJSON(&request); err != nil {
						t.Error(err)
						return
					}
					generations.Add(1)
					item := request["item"].(map[string]any)
					content := item["content"].([]any)[0].(map[string]any)
					requestID := content["requestId"].(string)
					// Even a final payload must never be returned when its completion
					// explicitly says it was moderated.
					_ = connection.WriteJSON(map[string]any{"type": "image", "id": "blocked", "requestId": requestID, "blob": "YmxvY2tlZA==", "percentage_complete": 100})
					_ = connection.WriteJSON(map[string]any{"type": "json", "image_id": "blocked", "requestId": requestID, "current_status": "completed", "moderated": true})
				}))
				defer server.Close()
				adapter, credential := testMediaAdapter(t, server.URL)
				response, err := adapter.GenerateImage(context.Background(), provider.ImageGenerationRequest{
					Credential: credential, Model: model, Prompt: "test", Count: 1, Streaming: stream, ResponseFormat: "b64_json",
				})
				if !stream {
					if response != nil {
						_ = response.Body.Close()
					}
					status, _ := provider.ErrorHTTPStatus(err)
					message, _ := provider.ErrorPublicMessage(err)
					if response != nil || status != http.StatusBadRequest || message != provider.ContentPolicyMessage || !provider.IsRequestScopedError(err) {
						t.Fatalf("moderation must not become a generic transport 502: response=%+v err=%v status=%d public=%q", response, err, status, message)
					}
					if !strings.Contains(err.Error(), "moderated=1") || !strings.Contains(err.Error(), "/ws/imagine/listen") {
						t.Fatalf("missing moderation evidence: %v", err)
					}
				} else {
					if err != nil || response == nil || response.StatusCode != http.StatusOK || !strings.HasSuffix(response.UpstreamURL, "/ws/imagine/listen") {
						t.Fatalf("stream response=%+v err=%v", response, err)
					}
					defer response.Body.Close()
					body, readErr := io.ReadAll(response.Body)
					if readErr != nil || !strings.Contains(string(body), "event: error\n") {
						t.Fatalf("stream must deliver a protocol error, not an empty interrupted 200: body=%q err=%v", body, readErr)
					}
					var event struct {
						Type  string `json:"type"`
						Error struct {
							Code, Type, Message string
						} `json:"error"`
					}
					_, data, ok := strings.Cut(string(body), "data: ")
					if !ok {
						t.Fatalf("missing image error data: %s", body)
					}
					data, _, _ = strings.Cut(data, "\n")
					if err := json.Unmarshal([]byte(data), &event); err != nil || event.Type != "error" || event.Error.Code != provider.ContentPolicyViolation || event.Error.Type != "invalid_request_error" || event.Error.Message != provider.ContentPolicyMessage {
						t.Fatalf("invalid image protocol error: %s err=%v", body, err)
					}
					if strings.Contains(string(body), "image_generation.completed") || strings.Contains(string(body), "YmxvY2tlZA==") {
						t.Fatalf("moderated output leaked: %s", body)
					}
				}
				if generations.Load() != 1 {
					t.Fatalf("moderation retried generation %d times", generations.Load())
				}
			})
		}
	}
}

func TestImagineModerationRequiresEveryCompletedResultToBeBlocked(t *testing.T) {
	collector := newImagineCollector()
	if collector.moderationError() != nil {
		t.Fatal("an empty response is not moderation evidence")
	}
	collector.Accept(map[string]any{"type": "json", "image_id": "blocked", "current_status": "completed", "moderated": true})
	if collector.moderationError() == nil {
		t.Fatal("moderation does not require a blocked image payload")
	}
	collector.Accept(map[string]any{"type": "json", "image_id": "allowed", "current_status": "completed", "moderated": false})
	if collector.moderationError() != nil || collector.Done(2) {
		t.Fatal("a delayed unmoderated payload must not be classified as an entirely blocked batch")
	}
	collector.Accept(map[string]any{"type": "image", "id": "allowed", "blob": "YWxsb3dlZA==", "percentage_complete": 100})
	if !collector.Done(2) || collector.moderationError() != nil || len(collector.Images()) != 1 || collector.Images()[0].ID != "allowed" {
		t.Fatalf("mixed result lost its allowed output: %+v", collector.Images())
	}
}

func TestImagineReturnsSuccessfulImagesFromAnIncompleteBatch(t *testing.T) {
	for _, ending := range []string{"moderated", "disconnect", "error"} {
		for _, format := range []string{"url", "b64_json"} {
			t.Run(ending+"/"+format, func(t *testing.T) {
				var generations atomic.Int32
				server := fhttptest.NewServer(fhttp.HandlerFunc(func(w fhttp.ResponseWriter, r *fhttp.Request) {
					connection, err := (&websocket.Upgrader{CheckOrigin: func(*fhttp.Request) bool { return true }}).Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer connection.Close()
					var reset, request map[string]any
					if err := connection.ReadJSON(&reset); err != nil {
						t.Error(err)
						return
					}
					if err := connection.ReadJSON(&request); err != nil {
						t.Error(err)
						return
					}
					generations.Add(1)
					requestID := request["item"].(map[string]any)["content"].([]any)[0].(map[string]any)["requestId"].(string)
					for _, id := range []string{"allowed-a", "allowed-b"} {
						_ = connection.WriteJSON(map[string]any{"type": "image", "id": id, "requestId": requestID, "blob": "YWxsb3dlZA==", "percentage_complete": 100})
						_ = connection.WriteJSON(map[string]any{"type": "json", "image_id": id, "requestId": requestID, "current_status": "completed", "moderated": false})
					}
					switch ending {
					case "moderated":
						_ = connection.WriteJSON(map[string]any{"type": "json", "image_id": "blocked", "requestId": requestID, "current_status": "completed", "moderated": true})
					case "error":
						_ = connection.WriteJSON(map[string]any{"type": "error"})
					}
				}))
				defer server.Close()
				adapter, credential := testMediaAdapter(t, server.URL)
				response, err := adapter.GenerateImage(context.Background(), provider.ImageGenerationRequest{Credential: credential, Model: "grok-imagine-image-2.0", Prompt: "test", Count: 3, ResponseFormat: format})
				if err != nil || response == nil {
					t.Fatalf("partial results discarded: response=%+v err=%v", response, err)
				}
				defer response.Body.Close()
				raw, err := io.ReadAll(response.Body)
				var body struct{ Data []map[string]string }
				if err != nil || json.Unmarshal(raw, &body) != nil || len(body.Data) != 2 || response.StatusCode != http.StatusOK || response.QuotaUnits != 2 || generations.Load() != 1 {
					t.Fatalf("response=%+v body=%s err=%v submissions=%d", response, raw, err, generations.Load())
				}
				for _, item := range body.Data {
					if item[format] == "" {
						t.Fatalf("missing %s: %s", format, raw)
					}
				}
			})
		}
	}
}
