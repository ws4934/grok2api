package gateway

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/web"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

type image20EditStub struct{ *web.Adapter }

func (s *image20EditStub) EditImage(context.Context, provider.ImageEditRequest) (*provider.Response, error) {
	return &provider.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"data":[{"url":"https://example.com/result.png"}]}`)), QuotaUnits: 1}, nil
}

func TestImage20UnifiedEditSelectsAndConsumesTheEffectiveTierQuota(t *testing.T) {
	for _, tier := range []account.WebTier{account.WebTierBasic, account.WebTierSuper} {
		t.Run(string(tier), func(t *testing.T) {
			ctx := context.Background()
			db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "image20.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			accounts, models, audits := relational.NewAccountRepository(db), relational.NewModelRepository(db), relational.NewAuditRepository(db)
			keys := relational.NewClientKeyRepository(db)
			credential, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: tier, Name: "image20", SourceKey: "image20", EncryptedAccessToken: "test-only", Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			remaining := func(mode string) int {
				if (tier == account.WebTierBasic && mode == account.QuotaModeWebImagePro) || (tier == account.WebTierSuper && mode == account.QuotaModeWebImageEdit) {
					return 2
				}
				return 0
			}
			windows := []account.QuotaWindow{}
			for _, mode := range []string{account.QuotaModeWebImagePro, account.QuotaModeWebImageEdit} {
				windows = append(windows, account.QuotaWindow{AccountID: credential.ID, Mode: mode, Remaining: remaining(mode), Total: 2, SyncedAt: &now, Source: account.QuotaSourceUpstream})
			}
			if err := accounts.SaveQuotaWindows(ctx, credential.ID, tier, now, windows); err != nil {
				t.Fatal(err)
			}
			if err := models.UpsertRoutes(ctx, web.Routes()); err != nil {
				t.Fatal(err)
			}
			key, err := keys.Create(ctx, clientkey.Key{Name: "k", Prefix: "image20", SecretHash: strings.Repeat("d", 64), EncryptedSecret: "test-only", Enabled: true, RPMLimit: 60, MaxConcurrent: 2})
			if err != nil {
				t.Fatal(err)
			}
			registry := provider.NewRegistry(&image20EditStub{Adapter: web.NewAdapter(web.Config{}, nil, nil, nil, nil)})
			sticky := memory.NewStickyStore()
			accountService := accountapp.NewService(accounts, audits, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
			selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
			service := NewService(models, audits, accountService, clientkeyapp.NewService(keys, nil, nil, 60, 4, nil), registry, selector, relational.NewResponseRepository(db), 1)
			result, err := service.EditImage(ctx, ImageEditInput{RequestID: "req-image20", ClientKey: key, PublicModel: "Web/grok-imagine-image-2.0", Prompt: "combine", ImageURLs: []string{"a", "b", "c", "d", "e"}, Count: 1, Resolution: "2k", Quality: "high"})
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.ReadAll(result.Body)
			result.Finalize(Usage{}, "", "")
			_ = result.Body.Close()
			current, err := accounts.GetQuotaWindows(ctx, []uint64{credential.ID})
			if err != nil {
				t.Fatal(err)
			}
			for _, window := range current[credential.ID] {
				want := remaining(window.Mode)
				if want > 0 {
					want--
				}
				if window.Remaining != want {
					t.Fatalf("mode=%s remaining=%d want=%d", window.Mode, window.Remaining, want)
				}
			}
			rows, _, err := audits.List(ctx, 0, 1)
			if err != nil || len(rows) != 1 || rows[0].MediaInputImages != 5 || rows[0].MediaOutputImages != 1 || rows[0].PricingModel == "" || rows[0].EstimatedCostInUSDTicks <= 0 || strings.Contains(rows[0].PricingModel, "2k") {
				t.Fatalf("audit=%#v error=%v", rows, err)
			}
		})
	}
}
