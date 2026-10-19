package inference

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
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
