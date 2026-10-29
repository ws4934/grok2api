package systemlog

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	adminapp "github.com/chenyme/grok2api/backend/internal/application/adminauth"
	logapp "github.com/chenyme/grok2api/backend/internal/application/systemlog"
	"github.com/chenyme/grok2api/backend/internal/infra/observability"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

func TestSystemLogsRequireAdminAndExposeRedactedFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "logs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	auth := adminapp.NewService(relational.NewAdminRepository(db), relational.NewAdminSessionRepository(db), security.NewTokenService(strings.Repeat("s", 32)), time.Hour, time.Hour)
	if err := auth.Bootstrap(ctx, "admin", "local-test-password"); err != nil {
		t.Fatal(err)
	}
	_, tokens, err := auth.Login(ctx, "admin", "local-test-password", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	service := logapp.NewService(relational.NewSystemLogRepository(db))
	defer service.Close()
	logger := observability.WithSystemLogs(slog.New(slog.NewJSONHandler(io.Discard, nil)), service, "node")
	logger.Warn("account_quota_sync_failed", "provider", "console", "account_id", 42, "request_id", "req-42", "error", "CF blocked; Authorization: Bearer secret-value")
	// Close drains the queue and intentionally leaves reads available during shutdown.
	service.Close()
	router := gin.New()
	router.Use(middleware.RequestID(), middleware.AccessLog(logger))
	group := router.Group("/api/admin/v1")
	group.Use(middleware.AdminAuth(auth))
	NewHandler(service).Register(group)
	for _, token := range []string{"", "sk-client-key-not-an-admin"} {
		req := httptest.NewRequest(http.MethodGet, "/api/admin/v1/system/logs", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Fatalf("unauthorized status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
	request := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	rec := request("/api/admin/v1/system/logs?accountId=42&provider=console&level=WARN&requestId=req-42")
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" || strings.Contains(rec.Body.String(), "secret-value") || !strings.Contains(rec.Body.String(), "Cloudflare") && !strings.Contains(rec.Body.String(), "CF blocked") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Data struct {
			Items []json.RawMessage `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil || len(payload.Data.Items) != 1 {
		t.Fatalf("payload=%+v err=%v", payload, err)
	}
	for _, query := range []string{"limit=0", "limit=201", "level=TRACE", "accountId=oops", "cursor=-1", "since=not-time", "search=" + strings.Repeat("x", 201)} {
		rec := request("/api/admin/v1/system/logs?" + query)
		if rec.Code != 400 {
			t.Fatalf("query=%q status=%d", query, rec.Code)
		}
	}
	if rec := request("/api/admin/v1/system/logs?search=" + url.QueryEscape(strings.Repeat("中", 120))); rec.Code != 200 {
		t.Fatalf("valid Chinese search rejected: %d %s", rec.Code, rec.Body.String())
	}
	if rec := request("/api/admin/v1/system/logs?accountId=99"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatal(rec.Body.String())
	}
}
