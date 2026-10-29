package middleware

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/pkg/requestmeta"
	"github.com/gin-gonic/gin"
)

func TestAccessLogLevelAndServiceRequestID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for code, level := range map[int]string{200: "INFO", 400: "WARN", 503: "ERROR"} {
		var logs bytes.Buffer
		router := gin.New()
		router.Use(RequestID(), AccessLog(slog.New(slog.NewJSONHandler(&logs, nil))))
		router.GET("/test/:id", func(c *gin.Context) {
			if requestmeta.RequestID(c.Request.Context()) != "req-42" {
				t.Error("request context lost correlation")
			}
			c.Status(code)
		})
		req := httptest.NewRequest("GET", "/test/42?token=should-not-log", nil)
		req.Header.Set("X-Request-ID", "req-42")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		var event map[string]any
		if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		if event["level"] != level || event["path"] != "/test/:id" || event["request_id"] != "req-42" {
			t.Fatalf("event=%v", event)
		}
	}
}
