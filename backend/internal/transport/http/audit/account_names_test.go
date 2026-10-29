package audit

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	auditapp "github.com/chenyme/grok2api/backend/internal/application/audit"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/gin-gonic/gin"
)

func TestAuditListAndAttemptsResolveEmailWithoutRewritingSnapshots(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "labels.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(db)
	value, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{Provider: accountdomain.ProviderWeb, Name: "Grok Web 219ff735", Email: "audit@example.test", SourceKey: "audit-labels", EncryptedAccessToken: "encrypted", AuthStatus: accountdomain.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	audits := relational.NewAuditRepository(db)
	now := time.Now().UTC()
	if err := audits.Create(ctx, auditdomain.Record{RequestID: "labels", ClientKeyID: 1, ModelRouteID: 1, AccountID: &value.ID, AccountName: value.Name, StatusCode: 400, CreatedAt: now, Attempts: []auditdomain.Attempt{{Number: 1, AccountID: &value.ID, AccountName: value.Name, Source: auditdomain.AttemptSourceUpstreamHTTP, Stage: "upstream_response", StartedAt: now}}}); err != nil {
		t.Fatal(err)
	}
	service := auditapp.NewService(audits, slog.New(slog.NewJSONHandler(io.Discard, nil)), 100, 10)
	router := gin.New()
	NewHandler(service).WithAccountNames(accounts).Register(router.Group("/api/admin/v1"))
	for _, path := range []string{"/api/admin/v1/request-audits", "/api/admin/v1/request-audits/1"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"accountName":"audit@example.test"`) || strings.Contains(rec.Body.String(), value.Name) {
			t.Fatalf("path=%s status=%d body=%s", path, rec.Code, rec.Body.String())
		}
	}
	stored, err := audits.Get(ctx, 1)
	if err != nil || stored.AccountName != value.Name || stored.Attempts[0].AccountName != value.Name {
		t.Fatalf("snapshot=%+v err=%v", stored, err)
	}
}
