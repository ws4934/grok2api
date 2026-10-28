package inference

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/gin-gonic/gin"
)

// writeImageStream serves body as a streamed image result and returns what
// the client received and what the gateway was finalized with.
func writeImageStream(t *testing.T, body string) (*httptest.ResponseRecorder, gateway.Usage, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	var usage gateway.Usage
	code := "not finalized"
	result := &gateway.Result{
		StatusCode: http.StatusOK, Status: "200 OK",
		Header:   http.Header{"Content-Type": {"text/event-stream"}},
		Body:     io.NopCloser(strings.NewReader(body)),
		Finalize: func(value gateway.Usage, _, errorCode string) { usage, code = value, errorCode },
	}
	router := gin.New()
	router.POST("/", func(c *gin.Context) { NewHandler(nil, nil, 1<<20).writeResult(c, result, true, streamProtocolImage) })
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/", nil))
	return recorder, usage, code
}

// Streamed edits end with image_edit.completed (Grok Web, ChatGPT), which
// must finish the request as a success.
func TestWriteResultImageEditStreamCompletes(t *testing.T) {
	body := "event: image_edit.partial_image\ndata: " + `{"type":"image_edit.partial_image","b64_json":"AA==","partial_image_index":0}` + "\n\n" +
		"event: image_edit.completed\ndata: " + `{"type":"image_edit.completed","b64_json":"AA==","usage":{"total_tokens":0,"input_tokens":0,"output_tokens":0}}` + "\n\n"
	recorder, _, code := writeImageStream(t, body)
	if code != "" || recorder.Body.String() != body {
		t.Fatalf("final = %q body = %q", code, recorder.Body.String())
	}
}

// Images a stream forwarded before an error event reach Finalize, so the
// gateway can bill them; partial previews are not counted.
func TestWriteResultImageStreamCountsImagesBeforeAnError(t *testing.T) {
	body := "event: image_generation.partial_image\ndata: " + `{"type":"image_generation.partial_image","b64_json":"AA==","partial_image_index":0}` + "\n\n" +
		"event: image_generation.completed\ndata: " + `{"type":"image_generation.completed","b64_json":"AA==","usage":{"total_tokens":0,"input_tokens":0,"output_tokens":0}}` + "\n\n" +
		": keep-alive\n\n" +
		"event: error\ndata: " + `{"type":"error","error":{"type":"server_error","code":"upstream_error","message":"second image failed","param":null}}` + "\n\n"
	recorder, usage, code := writeImageStream(t, body)
	if code != "upstream_stream_error" || usage.OutputImages != 1 || !strings.Contains(recorder.Body.String(), "上游服务处理失败") || !strings.Contains(recorder.Body.String(), `"code":"upstream_error"`) {
		t.Fatalf("final = %q images = %d body = %q", code, usage.OutputImages, recorder.Body.String())
	}
}

func TestWriteResultImagePolicyErrorIsVisibleWithoutCountingAnImage(t *testing.T) {
	body := "event: error\ndata: " + `{"type":"error","error":{"type":"invalid_request_error","code":"content_policy_violation","message":"` + provider.ContentPolicyMessage + `","param":null}}` + "\n\n"
	recorder, usage, code := writeImageStream(t, body)
	if recorder.Code != http.StatusOK || code != "upstream_stream_error" || usage.OutputImages != 0 || recorder.Body.String() != body {
		t.Fatalf("status=%d final=%q images=%d body=%q", recorder.Code, code, usage.OutputImages, recorder.Body.String())
	}
}
