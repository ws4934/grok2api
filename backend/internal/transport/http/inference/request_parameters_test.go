package inference

import (
	"bytes"
	"encoding/base64"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/pkg/requestparams"
	"github.com/gin-gonic/gin"
)

func TestImageEditParametersCaptureJSONAndMultipartWithoutMedia(t *testing.T) {
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	_ = writer.WriteField("model", "image")
	_ = writer.WriteField("prompt", "lighthouse")
	_ = writer.WriteField("n", "100")
	part, _ := writer.CreateFormFile("image", "reference.png")
	image, _ := base64.StdEncoding.DecodeString(onePixelPNG)
	_, _ = part.Write(image)
	_ = writer.Close()
	for _, tc := range []struct{ body, contentType string }{
		{`{"model":"image","prompt":"lighthouse","n":100,"image":"` + onePixelDataURI + `","api_key":"do-not-store"}`, "application/json"},
		{form.String(), writer.FormDataContentType()},
	} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(tc.body))
		c.Request.Header.Set("Content-Type", tc.contentType)
		_, _, _, ok := readImageEditRequest(c)
		if !ok {
			t.Fatalf("edit failed: %s", recorder.Body.String())
		}
		_, parameters := requestparams.Start(c.Request.Context(), nil)
		raw := string(parameters.Data().Client.Values)
		if strings.Contains(raw, "private-image-bytes") || strings.Contains(raw, onePixelPNG) || strings.Contains(raw, "do-not-store") {
			t.Fatalf("private content retained: %s", raw)
		}
		if !strings.Contains(raw, "100") || !strings.Contains(raw, "lighthouse") {
			t.Fatalf("original controls lost: %s", raw)
		}
	}
}
