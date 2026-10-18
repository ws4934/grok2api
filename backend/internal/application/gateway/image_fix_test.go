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
	pricingapp "github.com/chenyme/grok2api/backend/internal/application/pricing"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	pricingdomain "github.com/chenyme/grok2api/backend/internal/domain/pricing"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

// partialImageAdapter answers like chatgpt.Adapter.images after a later image
// failed: a 200 carrying the images already drawn, QuotaUnits = delivered.
type partialImageAdapter struct {
	staleTokenImageAdapter
	units int
}

func (a *partialImageAdapter) GenerateImage(context.Context, provider.ImageGenerationRequest) (*provider.Response, error) {
	return &provider.Response{
		StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"created":1,"data":[{"b64_json":"AA=="}]}`)),
		QuotaUnits: a.units,
	}, nil
}

func TestSyncImageBillsDeliveredImagesOnly(t *testing.T) {
	for name, tc := range map[string]struct {
		units, wantImages int
	}{
		"partial delivery":      {units: 1, wantImages: 1},
		"units not reported":    {units: 0, wantImages: 3},
		"units above the count": {units: 5, wantImages: 3},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "partial.db"))
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
			if err := modelRepo.UpsertRoutes(ctx, []modeldomain.Route{{PublicID: "img-partial", Provider: account.ProviderBuild, UpstreamModel: "img-partial", Capability: modeldomain.CapabilityImage, Enabled: true}}); err != nil {
				t.Fatal(err)
			}
			if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"img-partial"}, time.Now()); err != nil {
				t.Fatal(err)
			}
			key, err := keyRepo.Create(ctx, clientkey.Key{
				Name: "k", Prefix: "kkkk", SecretHash: strings.Repeat("e", 64), EncryptedSecret: "encrypted",
				Enabled: true, RPMLimit: 60, MaxConcurrent: 4, BillingLimitUSDTicks: 100_000_000_000,
			})
			if err != nil {
				t.Fatal(err)
			}
			registry := provider.NewRegistry(&partialImageAdapter{units: tc.units})
			sticky := memory.NewStickyStore()
			accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
			selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
			service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(keyRepo, nil, nil, 60, 4, nil), registry, selector, relational.NewResponseRepository(database), 1)
			prices, err := pricingapp.NewService(ctx, relational.NewModelPriceRepository(database), func(account.Provider) bool { return true }, registry.PricingModel)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := prices.Put(ctx, pricingapp.Input{Provider: string(account.ProviderBuild), PricingModel: "img-partial", Operation: "image", Rates: map[pricingdomain.Component]string{pricingdomain.OutputImage: "1"}}); err != nil {
				t.Fatal(err)
			}
			service.SetPricing(prices)

			result, err := service.GenerateImage(ctx, ImageGenerationInput{RequestID: "req-partial", ClientKey: key, PublicModel: "img-partial", Prompt: "fox", Count: 3})
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.ReadAll(result.Body)
			result.Finalize(Usage{}, "", "")
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
			if records[0].MediaOutputImages != int64(tc.wantImages) || settled.BilledUsageUSDTicks != int64(tc.wantImages)*oneImage || settled.ReservedUsageUSDTicks != 0 {
				t.Fatalf("images = %d billed = %d reserved = %d, want %d images", records[0].MediaOutputImages, settled.BilledUsageUSDTicks, settled.ReservedUsageUSDTicks, tc.wantImages)
			}
		})
	}
}

// deliverFirstImageWithoutAudit leaves a two-image job as a worker that saved
// its first image and stopped before writing that image's audit.
func deliverFirstImageWithoutAudit(t *testing.T, f *imageJobFixture) media.Job {
	t.Helper()
	ctx := context.Background()
	job := f.create(t, ImageJobInput{Count: 2})
	now := time.Now().UTC()
	claimed, ok, err := f.jobs.TryClaimMediaJob(ctx, job.ID, now, now.Add(time.Hour), "claim_delivered_first_image")
	if err != nil || !ok {
		t.Fatalf("claim = %v %v", ok, err)
	}
	claimed.ResultAssetIDs = []string{"asset_delivered_000001"}
	if err := f.jobs.UpdateMediaJob(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	return job
}

func TestCanceledImageJobBillsImagesDeliveredBeforeTheCancel(t *testing.T) {
	f := newImageJobFixture(t, 1)
	ctx := context.Background()
	job := deliverFirstImageWithoutAudit(t, f)
	canceled, err := f.service.CancelImageJob(ctx, f.key, job.ID)
	if err != nil || canceled.Status != media.StatusCanceled {
		t.Fatalf("cancel = %#v err = %v", canceled, err)
	}
	if billed, reserved := f.billing(t); billed != 400_000_000 || reserved != 0 {
		t.Fatalf("billed = %d reserved = %d", billed, reserved)
	}
	audits := f.auditsByEvent(t)
	if record := audits[imageUsageEventID(job.ID, 0)]; record.StatusCode != http.StatusOK || record.MediaOutputImages != 1 || len(audits) != 1 {
		t.Fatalf("audits = %#v", audits)
	}
	if stored := f.job(t, job.ID); stored.UsageRecordedAt == nil {
		t.Fatal("usage of the canceled job is not recorded")
	}
}

func TestReconciliationBillsImagesOfACanceledJob(t *testing.T) {
	f := newImageJobFixture(t, 1)
	ctx := context.Background()
	job := deliverFirstImageWithoutAudit(t, f)
	// The cancel committed but its process died before writing the audits.
	if _, err := f.jobs.CancelMediaJob(ctx, job.ID, f.key.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := f.service.RecoverMediaJobs(ctx); err != nil {
		t.Fatal(err)
	}
	if billed, _ := f.billing(t); billed != 400_000_000 {
		t.Fatalf("billed = %d", billed)
	}
	if stored := f.job(t, job.ID); stored.UsageRecordedAt == nil {
		t.Fatal("usage of the canceled job is not recorded")
	}
}

type requestScopedTestError struct{ message string }

func (e requestScopedTestError) Error() string              { return e.message }
func (e requestScopedTestError) RequestScopedFailure() bool { return true }

func TestImageJobErrorMessageIsCutOnARuneBoundary(t *testing.T) {
	f := newImageJobFixture(t, 1)
	// 1 + 3*200 bytes: a byte cut at 512 lands inside a rune.
	message := "x" + strings.Repeat("抱", 200)
	f.adapter.submit = func(provider.ImageJobRequest, func(provider.ImageCheckpoint) error) (provider.ImageCheckpoint, error) {
		return provider.ImageCheckpoint{}, requestScopedTestError{message}
	}
	job := f.create(t, ImageJobInput{})
	f.service.processMediaJob(context.Background(), job.ID)
	stored := f.job(t, job.ID)
	if stored.Status != media.StatusFailed || len(stored.ErrorMessage) != 511 || !strings.HasPrefix(message, stored.ErrorMessage) {
		t.Fatalf("stored = %s %d bytes %q", stored.Status, len(stored.ErrorMessage), stored.ErrorMessage)
	}
}
