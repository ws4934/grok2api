package inference

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
)

func TestResponsesImageToolKeyedJobSurvivesDisconnect(t *testing.T) {
	f := newImageToolFixture(t)
	body := `{"model":"gpt-image-2","input":"draw a cat","tools":[{"type":"image_generation"}]}`
	// disconnect sends the request and drops the connection while the job is
	// still queued (no worker runs yet).
	disconnect := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		request := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)).WithContext(ctx)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", "client-retry-key-1")
		f.router.ServeHTTP(httptest.NewRecorder(), request)
	}
	disconnect()
	// A request already waiting on the job is not cancelled by a duplicate that disconnects.
	waiting := make(chan map[string]any, 1)
	go func() {
		_, resp := f.call(t, "POST", "/v1/responses", body, "Idempotency-Key", "client-retry-key-1")
		waiting <- resp
	}()
	time.Sleep(100 * time.Millisecond)
	disconnect()
	f.startWorkers(t)
	if resp := <-waiting; resp["status"] != "completed" {
		t.Fatalf("waiting request got %v", resp)
	}
	if code, retried := f.call(t, "POST", "/v1/responses", body, "Idempotency-Key", "client-retry-key-1"); code != 200 || retried["status"] != "completed" {
		t.Fatalf("retry with the same Idempotency-Key: %d %v", code, retried)
	}
}

func TestResponsesImageToolKeepsChatRequestsOnTheConversationPath(t *testing.T) {
	f := newImageToolFixture(t)
	f.startWorkers(t)
	f.gateway.UpdateDefaultImageModel("gpt-image-2")
	conversation := map[string]string{
		"extra tool":      `{"model":"gpt-5","input":"what is 2+2?","tools":[{"type":"image_generation"},{"type":"web_search"}]}`,
		"function tool":   `{"model":"gpt-5","input":"what is 2+2?","tools":[{"type":"image_generation"},{"type":"function","name":"f","parameters":{"type":"object"}}]}`,
		"multi-turn":      `{"model":"gpt-5","input":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"thanks"}],"tools":[{"type":"image_generation"}]}`,
		"reasoning":       `{"model":"gpt-5","input":"hi","reasoning":{"effort":"low"},"tools":[{"type":"image_generation"}]}`,
		"store false":     `{"model":"gpt-5","input":"hi","store":false,"tools":[{"type":"image_generation"}]}`,
		"tool model only": `{"model":"gpt-5","input":"hi","tools":[{"type":"image_generation","model":"gpt-image-2"},{"type":"web_search"}]}`,
	}
	for name, body := range conversation {
		if code, resp := f.call(t, "POST", "/v1/responses", body); code != 200 || resp["id"] != "resp_text" {
			t.Errorf("%s: %d %v", name, code, resp)
		}
	}
	isImage := func(resp map[string]any) bool {
		id, _ := resp["id"].(string)
		return strings.HasPrefix(id, gateway.ImageJobIDPrefix) && resp["status"] == "completed"
	}
	images := map[string]string{
		"image request":           `{"model":"gpt-5","input":"draw a cat","tools":[{"type":"image_generation"}]}`,
		"forced store false":      `{"model":"gpt-5","input":"draw a cat","store":false,"tool_choice":{"type":"image_generation"},"tools":[{"type":"image_generation"}]}`,
		"required single tool":    `{"model":"gpt-5","input":"draw a cat","store":false,"tool_choice":"required","tools":[{"type":"image_generation"}]}`,
		"image model store false": `{"model":"gpt-image-2","input":"draw a cat","store":false,"tools":[{"type":"image_generation"}]}`,
	}
	for name, body := range images {
		if code, resp := f.call(t, "POST", "/v1/responses", body); code != 200 || !isImage(resp) {
			t.Errorf("%s: %d %v", name, code, resp)
		}
	}
	// A forced image tool the image path cannot serve is rejected, not answered in text.
	code, resp := f.call(t, "POST", "/v1/responses", `{"model":"gpt-5","input":"draw","tool_choice":{"type":"image_generation"},"tools":[{"type":"image_generation"},{"type":"web_search"}]}`)
	if problem, _ := resp["error"].(map[string]any); code != 400 || problem["code"] != "unsupported_parameter" {
		t.Fatalf("forced with an extra tool: %d %v", code, resp)
	}
}
