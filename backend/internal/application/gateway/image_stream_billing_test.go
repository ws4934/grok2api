package gateway

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	pricingapp "github.com/chenyme/grok2api/backend/internal/application/pricing"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	pricingdomain "github.com/chenyme/grok2api/backend/internal/domain/pricing"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

// A stream bills the images it forwarded, also when it failed after them,
// because the client and the gallery already have them. A failed
// non-streamed request still bills nothing.
func TestStreamedImageBillsForwardedImages(t *testing.T) {
	for name, tc := range map[string]struct {
		streaming  bool
		forwarded  int64
		errorCode  string
		wantImages int64
	}{
		"stream failed after one image":  {streaming: true, forwarded: 1, errorCode: "upstream_stream_error", wantImages: 1},
		"stream failed before any image": {streaming: true, errorCode: "upstream_stream_error"},
		"stream ended after one image":   {streaming: true, forwarded: 1, wantImages: 1},
		"stream completed":               {streaming: true, forwarded: 2, wantImages: 2},
		"sync request failed":            {forwarded: 1, errorCode: "stream_interrupted"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "stream.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			if err := database.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			accountRepo, modelRepo, auditRepo := relational.NewAccountRepository(database), relational.NewModelRepository(database), relational.NewAuditRepository(database)
			keyRepo := relational.NewClientKeyRepository(database)
			credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
				Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: "p", SourceKey: "p",
				EncryptedAccessToken: "fresh", EncryptedRefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := modelRepo.UpsertRoutes(ctx, []modeldomain.Route{{PublicID: "img-stream", Provider: account.ProviderBuild, UpstreamModel: "img-stream", Capability: modeldomain.CapabilityImage, Enabled: true}}); err != nil {
				t.Fatal(err)
			}
			if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"img-stream"}, time.Now()); err != nil {
				t.Fatal(err)
			}
			key, err := keyRepo.Create(ctx, clientkey.Key{
				Name: "k", Prefix: "kkkk", SecretHash: strings.Repeat("e", 64), EncryptedSecret: "encrypted",
				Enabled: true, RPMLimit: 60, MaxConcurrent: 4, BillingLimitUSDTicks: 100_000_000_000,
			})
			if err != nil {
				t.Fatal(err)
			}
			// QuotaUnits is the requested count up front, as chatgpt streamImages reports it.
			registry := provider.NewRegistry(&partialImageAdapter{units: 2})
			sticky := memory.NewStickyStore()
			accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
			selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
			service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(keyRepo, nil, nil, 60, 4, nil), registry, selector, relational.NewResponseRepository(database), 1)
			prices, err := pricingapp.NewService(ctx, relational.NewModelPriceRepository(database), func(account.Provider) bool { return true }, registry.PricingModel)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := prices.Put(ctx, pricingapp.Input{Provider: string(account.ProviderBuild), PricingModel: "img-stream", Operation: "image", Rates: map[pricingdomain.Component]string{pricingdomain.OutputImage: "1"}}); err != nil {
				t.Fatal(err)
			}
			service.SetPricing(prices)

			result, err := service.GenerateImage(ctx, ImageGenerationInput{RequestID: "req-stream", ClientKey: key, PublicModel: "img-stream", Prompt: "fox", Count: 2, Streaming: tc.streaming})
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.ReadAll(result.Body)
			result.Finalize(Usage{OutputImages: tc.forwarded}, "", tc.errorCode)
			_ = result.Body.Close()
			settled, err := keyRepo.Get(ctx, key.ID)
			if err != nil {
				t.Fatal(err)
			}
			records, _, err := auditRepo.List(ctx, 0, 1)
			if err != nil || len(records) != 1 {
				t.Fatalf("audits = %v err = %v", records, err)
			}
			const oneImage = 10_000_000_000 // 1 USD
			record := records[0]
			if record.ErrorCode != tc.errorCode || record.MediaOutputImages != tc.wantImages || record.EstimatedCostInUSDTicks != tc.wantImages*oneImage ||
				settled.BilledUsageUSDTicks != tc.wantImages*oneImage || settled.ReservedUsageUSDTicks != 0 {
				t.Fatalf("code = %q images = %d cost = %d billed = %d reserved = %d, want %d images",
					record.ErrorCode, record.MediaOutputImages, record.EstimatedCostInUSDTicks, settled.BilledUsageUSDTicks, settled.ReservedUsageUSDTicks, tc.wantImages)
			}
		})
	}
}
