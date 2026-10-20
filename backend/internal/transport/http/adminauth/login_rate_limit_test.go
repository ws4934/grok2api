package adminauth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	adminapp "github.com/chenyme/grok2api/backend/internal/application/adminauth"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/gin-gonic/gin"
)

// Behind a trusted reverse proxy every login arrives from the proxy address;
// the per-IP login bucket (30/min) must key on the forwarded client, or one
// client's failed attempts against made-up usernames lock everyone out.
func TestLoginRateLimitKeysOnTheClientBehindATrustedProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	service := adminapp.NewService(relational.NewAdminRepository(database), relational.NewAdminSessionRepository(database), security.NewTokenService("12345678901234567890123456789012"), 15*time.Minute, 30*24*time.Hour)
	service.SetLoginRateLimiter(memory.NewRateLimiter())
	if err := service.Bootstrap(ctx, "admin", "password123"); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	if err := router.SetTrustedProxies([]string{"10.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	NewHandler(service, true).RegisterPublic(router.Group("/api/admin/v1"))
	login := func(client, username, password string) int {
		request := httptest.NewRequest(http.MethodPost, "/api/admin/v1/auth/login", strings.NewReader(fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)))
		request.Header.Set("Content-Type", "application/json")
		request.RemoteAddr = "10.0.0.1:443" // the reverse proxy
		request.Header.Set("X-Forwarded-For", client)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		return recorder.Code
	}
	for index := range 30 {
		_ = login("203.0.113.7", fmt.Sprintf("guess-%d", index), "wrong-password")
	}
	status := login("198.51.100.20", "admin", "password123")
	if status != http.StatusOK {
		t.Fatalf("admin login from an unrelated client = %d", status)
	}
}
