package inference

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
)

// forgetfulAssets saves images but has lost every one of them on read, as
// after capacity cleanup or a gallery deletion.
type forgetfulAssets struct{ *mediaapp.Service }

func (forgetfulAssets) OpenImage(context.Context, string) (mediadomain.Asset, io.ReadCloser, error) {
	return mediadomain.Asset{}, nil, mediaapp.ErrAssetNotFound
}

func TestResponsesImageToolRendersARemovedResultAsAFailedCall(t *testing.T) {
	f := newImageToolFixture(t)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "assets.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	objects, err := localmedia.NewLocalStore(filepath.Join(t.TempDir(), "objects"))
	if err != nil {
		t.Fatal(err)
	}
	f.gateway.ConfigureMediaAssets(forgetfulAssets{mediaapp.NewService(relational.NewMediaAssetRepository(database), relational.NewMediaJobRepository(database), objects, nil, mediaapp.Config{PublicBaseURL: "https://example.test", MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30, CleanupThresholdPercent: 90, CleanupInterval: time.Hour})})
	f.startWorkers(t)
	removed := func(resp map[string]any) bool {
		output, _ := resp["output"].([]any)
		if len(output) != 1 {
			return false
		}
		call, _ := output[0].(map[string]any)
		problem, _ := call["error"].(map[string]any)
		return resp["status"] == "completed" && call["status"] == "failed" && call["result"] == nil && problem["code"] == "result_unavailable"
	}
	request := `{"model":"gpt-image-2","input":"draw a cat","tools":[{"type":"image_generation"}]}`
	code, created := f.call(t, "POST", "/v1/responses", request)
	if code != 200 || !removed(created) {
		t.Fatalf("create: %d %v", code, created)
	}
	if code, polled := f.call(t, "GET", "/v1/responses/"+created["id"].(string), ""); code != 200 || !removed(polled) {
		t.Fatalf("poll: %d %v", code, polled)
	}

	recorder := httptest.NewRecorder()
	stream := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(strings.Replace(request, `"input"`, `"stream":true,"input"`, 1)))
	stream.Header.Set("Content-Type", "application/json")
	f.router.ServeHTTP(recorder, stream)
	var names []string
	var completed map[string]any
	for _, block := range strings.Split(recorder.Body.String(), "\n\n") {
		name, data := "", ""
		for _, line := range strings.Split(block, "\n") {
			if value, ok := strings.CutPrefix(line, "event: "); ok {
				name = value
			} else if value, ok := strings.CutPrefix(line, "data: "); ok {
				data = value
			}
		}
		if name == "" {
			continue
		}
		names = append(names, name)
		if name == "response.completed" {
			var event map[string]any
			_ = json.Unmarshal([]byte(data), &event)
			completed, _ = event["response"].(map[string]any)
		}
	}
	want := "response.created,response.in_progress,response.output_item.added,response.image_generation_call.in_progress,response.image_generation_call.generating,response.output_item.done,response.completed"
	if strings.Join(names, ",") != want || !removed(completed) {
		t.Fatalf("stream events = %v completed = %v", names, completed)
	}
}

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
