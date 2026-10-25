package inference

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII="

const onePixelDataURI = "data:image/png;base64," + onePixelPNG

func readImageEditForTest(t *testing.T, request *http.Request) (imageEditJSONRequest, []string, []string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = request
	parsed, images, masks, ok := readImageEditRequest(c)
	if !ok {
		t.Fatalf("request rejected: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	return parsed, images, masks
}

func TestImageEditNormalizesJSONInputForms(t *testing.T) {
	body := `{"model":"m","prompt":"p",
		"image":"` + onePixelDataURI + `",
		"images":["` + onePixelPNG + `",{"image_url":"https://example.com/a.png"},{"image_url":{"url":"` + onePixelDataURI + `"}},{"url":"https://example.com/b.png"},{"b64_json":"` + onePixelPNG + `"},{"base64":"` + onePixelPNG + `"}],
		"image_url":{"url":"https://example.com/c.png"},
		"mask":{"b64_json":"` + onePixelPNG + `"},
		"output_format":"webp","background":"transparent","moderation":"low","output_compression":50}`
	request := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	parsed, images, masks := readImageEditForTest(t, request)
	want := []string{onePixelDataURI, onePixelDataURI, "https://example.com/a.png", onePixelDataURI, "https://example.com/b.png", onePixelDataURI, onePixelDataURI, "https://example.com/c.png"}
	if !slices.Equal(images, want) || !slices.Equal(masks, []string{onePixelDataURI}) {
		t.Fatalf("images = %#v masks = %#v", images, masks)
	}
	if parsed.OutputFormat != "webp" || parsed.Background != "transparent" {
		t.Fatalf("parsed = %#v", parsed)
	}
}

func TestImageEditNormalizesMultipartInputs(t *testing.T) {
	raw, _ := base64.StdEncoding.DecodeString(onePixelPNG)
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	for _, name := range []string{"image", "image[]", "images[]", "mask"} {
		file, _ := writer.CreateFormFile(name, "a.png")
		_, _ = file.Write(raw)
	}
	for name, value := range map[string]string{"model": "m", "prompt": "p", "n": "2", "stream": "true", "partial_images": "1", "output_format": "png", "background": "opaque", "quality": "high"} {
		_ = writer.WriteField(name, value)
	}
	_ = writer.WriteField("image_url", "https://example.com/a.png")
	_ = writer.WriteField("image_url", `[{"b64_json":"`+onePixelPNG+`"}]`)
	_ = writer.Close()
	request := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &form)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	parsed, images, masks := readImageEditForTest(t, request)
	want := []string{onePixelDataURI, onePixelDataURI, onePixelDataURI, "https://example.com/a.png", onePixelDataURI}
	if !slices.Equal(images, want) || !slices.Equal(masks, []string{onePixelDataURI}) {
		t.Fatalf("images = %#v masks = %#v", images, masks)
	}
	if parsed.Model != "m" || parsed.Prompt != "p" || parsed.Count == nil || *parsed.Count != 2 || !parsed.Stream ||
		parsed.PartialImages == nil || *parsed.PartialImages != 1 || parsed.OutputFormat != "png" || parsed.Background != "opaque" || parsed.Quality != "high" {
		t.Fatalf("parsed = %#v", parsed)
	}
}

func TestImageEditRejectsUnsupportedInputs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	NewHandler(nil, nil, 1<<20).Register(router.Group("/v1"))
	serve := func(request *http.Request) (int, map[string]any) {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		var payload struct {
			Error map[string]any `json:"error"`
		}
		_ = json.Unmarshal(recorder.Body.Bytes(), &payload)
		return recorder.Code, payload.Error
	}
	for name, test := range map[string]struct{ body, code string }{
		"file_id":        {`{"model":"m","prompt":"p","images":[{"file_id":"file-1"}]}`, "unsupported_parameter"},
		"mask file_id":   {`{"model":"m","prompt":"p","image":"` + onePixelDataURI + `","mask":{"file_id":"file-1"}}`, "unsupported_parameter"},
		"nested array":   {`{"model":"m","prompt":"p","images":[["` + onePixelDataURI + `"]]}`, "invalid_request"},
		"empty object":   {`{"model":"m","prompt":"p","image":{}}`, "invalid_request"},
		"http URL":       {`{"model":"m","prompt":"p","image_url":"http://example.com/a.png"}`, "invalid_request"},
		"invalid base64": {`{"model":"m","prompt":"p","image":"not base64!"}`, "invalid_request"},
		"not an image":   {`{"model":"m","prompt":"p","image":"aGVsbG8="}`, "invalid_request"},
		"text data URI":  {`{"model":"m","prompt":"p","image":"data:text/plain;base64,aGVsbG8="}`, "invalid_request"},
	} {
		request := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(test.body))
		request.Header.Set("Content-Type", "application/json")
		if status, problem := serve(request); status != http.StatusBadRequest || problem["code"] != test.code {
			t.Errorf("%s: status=%d error=%v", name, status, problem)
		}
	}
	// Extra masks and images beyond eight are dropped, not refused.
	for name, body := range map[string]string{
		"two masks":   `{"model":"m","prompt":"p","image":"` + onePixelDataURI + `","mask":["` + onePixelDataURI + `","` + onePixelDataURI + `"]}`,
		"nine images": `{"model":"m","prompt":"p","images":["` + strings.Repeat(onePixelDataURI+`","`, 8) + onePixelDataURI + `"]}`,
	} {
		request := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		if status, problem := serve(request); status != http.StatusUnauthorized {
			t.Errorf("%s: status=%d error=%v", name, status, problem)
		}
	}

	multipartRequest := func(fields map[string]string, fileBytes int) *http.Request {
		var form bytes.Buffer
		writer := multipart.NewWriter(&form)
		for name, value := range fields {
			_ = writer.WriteField(name, value)
		}
		if fileBytes > 0 {
			file, _ := writer.CreateFormFile("image", "a.png")
			_, _ = file.Write(bytes.Repeat([]byte{0x89}, fileBytes))
		}
		_ = writer.Close()
		request := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &form)
		request.Header.Set("Content-Type", writer.FormDataContentType())
		return request
	}
	if status, problem := serve(multipartRequest(map[string]string{"model": "m", "prompt": "p", "image_url": `{"file_id":"file-1"}`}, 0)); status != http.StatusBadRequest || problem["code"] != "unsupported_parameter" {
		t.Errorf("multipart file_id: status=%d error=%v", status, problem)
	}
	if status, problem := serve(multipartRequest(map[string]string{"model": "m", "prompt": "p", "image_url": onePixelDataURI, "n": "two"}, 0)); status != http.StatusBadRequest || problem["code"] != "invalid_parameter" {
		t.Errorf("multipart n: status=%d error=%v", status, problem)
	}
	if status, problem := serve(multipartRequest(map[string]string{"model": "m", "prompt": "p"}, 2<<20)); status != http.StatusRequestEntityTooLarge {
		t.Errorf("multipart body limit: status=%d error=%v", status, problem)
	}
	// A well-formed multipart edit passes transport validation and reaches authentication.
	if status, problem := serve(multipartRequest(map[string]string{"model": "m", "prompt": "p", "image_url": onePixelDataURI, "quality": "high"}, 0)); status != http.StatusUnauthorized {
		t.Errorf("valid multipart: status=%d error=%v", status, problem)
	}
}

func TestImageGenerationLeavesProviderParametersToRouting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	NewHandler(nil, nil, 1<<20).Register(router.Group("/v1"))
	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{
		"model":"m","prompt":"p","quality":"high","size":"1536x1024","output_format":"webp","background":"transparent","moderation":"low"
	}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
