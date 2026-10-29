package inference

import (
	"bytes"
	"io"
	"mime/multipart"

	"github.com/chenyme/grok2api/backend/internal/pkg/requestparams"
	"github.com/gin-gonic/gin"
)

func captureRequestParameters(c *gin.Context, value any) {
	c.Request = c.Request.WithContext(requestparams.WithClient(c.Request.Context(), value))
}

func captureFormParameters(c *gin.Context, form *multipart.Form) {
	values := make(map[string]any, len(form.Value)+len(form.File))
	for key, value := range form.Value {
		values[key] = value
	}
	for key, files := range form.File {
		items := make([]map[string]any, 0, len(files))
		for _, file := range files {
			items = append(items, map[string]any{"filename": file.Filename, "size_bytes": file.Size, "content_type": file.Header.Get("Content-Type")})
		}
		values[key] = items
	}
	captureRequestParameters(c, values)
}

func decodeRequestJSON(c *gin.Context, target any) error {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return err
	}
	captureRequestParameters(c, body)
	return decodeSingleJSON(bytes.NewReader(body), target, false)
}
