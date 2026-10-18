package channelhttp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image/png"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestImagesGenerationDefaultsUsageAndLeniency(t *testing.T) {
	s, r := responsesFixture(t)
	startWorker(t, s)
	// Ignored compatibility options are accepted; response_format defaults to b64_json.
	code, body := call(t, r, "POST", "/v1/images/generations", `{"model":"img","prompt":"draw a cat","quality":"high","user":"u","style":"vivid","moderation":"low","output_compression":50,"partial_images":2,"stream":false}`)
	data, _ := body["data"].([]any)
	if code != 200 || len(data) != 1 {
		t.Fatalf("generation: %d %v", code, body)
	}
	item := data[0].(map[string]any)
	raw, e := base64.StdEncoding.DecodeString(item["b64_json"].(string))
	if e != nil || item["url"] != nil {
		t.Fatalf("default response_format: %v", item)
	}
	if _, e := png.Decode(bytes.NewReader(raw)); e != nil {
		t.Fatal(e)
	}
	if body["size"] != "1x1" || body["quality"] != "high" || body["output_format"] != "png" || body["background"] != "auto" || body["job_id"] == nil {
		t.Fatalf("top-level fields: %v", body)
	}
	text := float64(s.EstimateTextTokens("draw a cat"))
	usage := body["usage"].(map[string]any)
	details := usage["input_tokens_details"].(map[string]any)
	// 1x1 output at quality high: one patch * 65/16, rounded up.
	if text == 0 || usage["input_tokens"] != text || details["text_tokens"] != text || details["image_tokens"] != float64(0) || usage["output_tokens"] != float64(5) || usage["total_tokens"] != text+5 {
		t.Fatalf("usage: %v", usage)
	}
	for _, body := range []string{
		`{"model":"img","prompt":"x","output_compression":101}`,
		`{"model":"img","prompt":"x","partial_images":4}`,
		`{"model":"img","prompt":"x","moderation":"strict"}`,
		`{"model":"img","prompt":"x","response_format":"png"}`,
		`{"model":"img","prompt":"x","n":0}`,
		`{"model":"img","prompt":"x","input_fidelity":"high"}`,
	} {
		if code, out := call(t, r, "POST", "/v1/images/generations", body); code != 400 {
			t.Errorf("accepted %s: %d %v", body, code, out)
		}
	}
}

func TestImagesEditInputForms(t *testing.T) {
	s, r := responsesFixture(t)
	startWorker(t, s)
	uri := "data:image/png;base64," + onePixelPNG
	inputTokens := func(body map[string]any) any {
		usage, _ := body["usage"].(map[string]any)
		details, _ := usage["input_tokens_details"].(map[string]any)
		return details["image_tokens"]
	}
	// Every JSON form at once: 1 + 6 + 1 = 8 inputs, 256 vision tokens each.
	all := `{"model":"img-edit","prompt":"edit","image":"` + uri + `","images":["` + onePixelPNG + `",{"image_url":"` + uri + `"},{"image_url":{"url":"` + uri + `"}},{"url":"` + uri + `"},{"b64_json":"` + onePixelPNG + `","mime_type":"image/png"},{"base64":"` + onePixelPNG + `"}],"image_url":{"url":"` + uri + `"}}`
	if code, body := call(t, r, "POST", "/v1/images/edits", all); code != 200 || inputTokens(body) != float64(8*256) {
		t.Fatalf("JSON forms: %d %v", code, body)
	}

	// Multipart: image, image[] and images[] files plus repeated image_url text (plain and JSON).
	var form bytes.Buffer
	w := multipart.NewWriter(&form)
	raw, _ := base64.StdEncoding.DecodeString(onePixelPNG)
	for _, name := range []string{"image", "image[]", "images[]", "images[]"} {
		f, _ := w.CreateFormFile(name, "a.png")
		_, _ = f.Write(raw)
	}
	_ = w.WriteField("model", "img-edit")
	_ = w.WriteField("prompt", "edit")
	_ = w.WriteField("image_url", uri)
	_ = w.WriteField("image_url", `[{"image_url":"`+uri+`"}]`)
	_ = w.Close()
	request := httptest.NewRequest("POST", "/v1/images/edits", &form)
	request.Header.Set("Content-Type", w.FormDataContentType())
	recorder := httptest.NewRecorder()
	r.ServeHTTP(recorder, request)
	var body map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &body)
	if recorder.Code != 200 || inputTokens(body) != float64(6*256) {
		t.Fatalf("multipart forms: %d %v", recorder.Code, body)
	}

	for name, tc := range map[string]struct {
		body, code string
	}{
		"file_id":             {`{"model":"img-edit","prompt":"x","images":[{"file_id":"file-1"}]}`, "unsupported_image_reference"},
		"nested array":        {`{"model":"img-edit","prompt":"x","images":[["` + uri + `"]]}`, "invalid_image"},
		"empty object":        {`{"model":"img-edit","prompt":"x","image":{}}`, "invalid_image"},
		"loopback URL":        {`{"model":"img-edit","prompt":"x","image_url":"http://127.0.0.1/a.png"}`, "invalid_image"},
		"invalid base64":      {`{"model":"img-edit","prompt":"x","image":"not base64!"}`, "invalid_image"},
		"two masks":           {`{"model":"img-edit","prompt":"x","image":"` + uri + `","mask":["` + uri + `","` + uri + `"]}`, "invalid_mask"},
		"nine images":         {`{"model":"img-edit","prompt":"x","images":["` + strings.Repeat(uri+`","`, 8) + uri + `"]}`, "too_many_images"},
		"mask file_id":        {`{"model":"img-edit","prompt":"x","image":"` + uri + `","mask":{"file_id":"file-1"}}`, "unsupported_image_reference"},
		"unknown edit option": {`{"model":"img-edit","prompt":"x","image":"` + uri + `","input_fidelity":"high"}`, "unsupported_parameter"},
	} {
		code, body := call(t, r, "POST", "/v1/images/edits", tc.body)
		problem, _ := body["error"].(map[string]any)
		if code != 400 || problem["code"] != tc.code {
			t.Errorf("%s: %d %v", name, code, body)
		}
		if name == "file_id" && !strings.Contains(problem["message"].(string), "file_id image references are not supported") {
			t.Errorf("file_id message: %v", problem)
		}
	}
}

func TestImagesStream(t *testing.T) {
	saved := imageKeepAlive
	imageKeepAlive = 10 * time.Millisecond
	t.Cleanup(func() { imageKeepAlive = saved })
	s, r := responsesFixture(t)
	startWorker(t, s)

	events, comments := stream(t, r, "/v1/images/generations", `{"model":"img","prompt":"draw a cat","stream":true}`)
	if comments == 0 || len(events) != 1 || events[0].name != "image_generation.completed" {
		t.Fatalf("generation stream: %d comments, %v", comments, events)
	}
	done := events[0].data
	raw, e := base64.StdEncoding.DecodeString(done["b64_json"].(string))
	if e != nil || done["type"] != "image_generation.completed" || done["created_at"] == nil || done["size"] != "1x1" || done["quality"] != "auto" || done["background"] != "auto" || done["output_format"] != "png" {
		t.Fatalf("image_generation.completed: %v", done)
	}
	if _, e := png.Decode(bytes.NewReader(raw)); e != nil {
		t.Fatal(e)
	}
	if usage, _ := done["usage"].(map[string]any); usage == nil || usage["output_tokens"] != float64(2) {
		t.Fatalf("stream usage: %v", done["usage"])
	}

	uri := "data:image/png;base64," + onePixelPNG
	events, _ = stream(t, r, "/v1/images/edits", `{"model":"img-edit","prompt":"edit","image":{"image_url":"`+uri+`"},"mask":{"b64_json":"`+onePixelPNG+`"},"response_format":"url","stream":true}`)
	if len(events) != 1 || events[0].name != "image_edit.completed" || events[0].data["type"] != "image_edit.completed" || !strings.HasPrefix(events[0].data["url"].(string), "https://example.test/") {
		t.Fatalf("edit stream: %v", events)
	}

	events, _ = stream(t, r, "/v1/images/generations", `{"model":"img","prompt":"fail","stream":true}`)
	if len(events) != 1 || events[0].name != "error" {
		t.Fatalf("failed stream: %v", events)
	}
	if problem, _ := events[0].data["error"].(map[string]any); problem["code"] != "image_generation_failed" || problem["type"] != "server_error" || problem["message"] == "" {
		t.Fatalf("error event: %v", events[0].data)
	}
}
