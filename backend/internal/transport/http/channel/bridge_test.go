package channelhttp

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"

	app "github.com/chenyme/grok2api/backend/internal/application/channel"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	domain "github.com/chenyme/grok2api/backend/internal/domain/channel"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/gin-gonic/gin"
)

type legacyRoutes struct{ repository.ModelRepository }

func (legacyRoutes) GetByPublicIDCandidates(context.Context, string) ([]model.Route, error) {
	return []model.Route{{ID: 1, Provider: account.ProviderWeb, Enabled: true}}, nil
}
func TestBridgePreservesLegacyChatBody(t *testing.T) {
	registry, _ := domain.NewRegistry()
	h := NewHandler(app.NewService(registry, nil, nil, nil, legacyRoutes{}))
	r := gin.New()
	r.Use(h.Dispatch())
	body := `{"model":"grok","messages":[{"role":"user","content":"hello"}],"tools":[{"type":"web_search"}]}`
	r.POST("/v1/chat/completions", func(c *gin.Context) {
		raw, _ := io.ReadAll(c.Request.Body)
		if string(raw) != body {
			t.Fatal("legacy body consumed or transformed")
		}
		c.Status(204)
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if w.Code != 204 {
		t.Fatalf("legacy request intercepted: %d %s", w.Code, w.Body.String())
	}
}
func TestBridgePreservesLegacyMultipartTemporaryFiles(t *testing.T) {
	registry, _ := domain.NewRegistry()
	h := NewHandler(app.NewService(registry, nil, nil, nil, legacyRoutes{}))
	r := gin.New()
	r.Use(h.Dispatch())
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	_ = form.WriteField("model", "grok")
	file, _ := form.CreateFormFile("image", "input.png")
	_, _ = file.Write(bytes.Repeat([]byte{'x'}, 21<<20))
	_ = form.Close()
	r.POST("/v1/images/edits", func(c *gin.Context) {
		defer c.Request.MultipartForm.RemoveAll()
		file, e := c.Request.MultipartForm.File["image"][0].Open()
		if e != nil {
			t.Fatal("bridge removed legacy temporary file", e)
		}
		defer file.Close()
		n, e := io.Copy(io.Discard, file)
		if e != nil || n != 21<<20 {
			t.Fatalf("file truncated: %d %v", n, e)
		}
		c.Status(204)
	})
	request := httptest.NewRequest("POST", "/v1/images/edits", &body)
	request.Header.Set("Content-Type", form.FormDataContentType())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, request)
	if w.Code != 204 {
		t.Fatalf("legacy multipart intercepted: %d %s", w.Code, w.Body.String())
	}
}
