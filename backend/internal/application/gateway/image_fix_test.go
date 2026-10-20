package gateway

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
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

// deliverImageWithoutAudit leaves an image job as a worker that saved its
// image and stopped before writing its audit and terminal state.
func deliverImageWithoutAudit(t *testing.T, f *imageJobFixture) media.Job {
	t.Helper()
	ctx := context.Background()
	job := f.create(t, ImageJobInput{})
	now := time.Now().UTC()
	claimed, ok, err := f.jobs.TryClaimMediaJob(ctx, job.ID, now, now.Add(time.Hour), "claim_delivered_first_image")
	if err != nil || !ok {
		t.Fatalf("claim = %v %v", ok, err)
	}
	claimed.ResultAssetID = "asset_delivered_000001"
	if err := f.jobs.UpdateMediaJob(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	return job
}

func TestCanceledImageJobBillsAnImageDeliveredBeforeTheCancel(t *testing.T) {
	f := newImageJobFixture(t, 1)
	ctx := context.Background()
	job := deliverImageWithoutAudit(t, f)
	canceled, err := f.service.CancelImageJob(ctx, f.key, job.ID)
	if err != nil || canceled.Status != media.StatusCanceled {
		t.Fatalf("cancel = %#v err = %v", canceled, err)
	}
	if billed, reserved := f.billing(t); billed != 400_000_000 || reserved != 0 {
		t.Fatalf("billed = %d reserved = %d", billed, reserved)
	}
	audits := f.auditsByEvent(t)
	if record := audits[imageEventID(job)]; record.StatusCode != http.StatusOK || record.MediaOutputImages != 1 || len(audits) != 1 {
		t.Fatalf("audits = %#v", audits)
	}
	if stored := f.job(t, job.ID); stored.UsageRecordedAt == nil {
		t.Fatal("usage of the canceled job is not recorded")
	}
}

func TestReconciliationBillsTheImageOfACanceledJob(t *testing.T) {
	f := newImageJobFixture(t, 1)
	ctx := context.Background()
	job := deliverImageWithoutAudit(t, f)
	// The cancel committed but its process died before writing the audit.
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

// outputFailureError is a downloaded image that fails the output options, as
// chatgpt reports it.
type outputFailureError struct{}

func (outputFailureError) Error() string            { return "image is not transparent" }
func (outputFailureError) ImageOutputFailure() bool { return true }

func TestImageJobDoesNotRefetchAnImageFailingTheOutputOptions(t *testing.T) {
	f := newImageJobFixture(t, 1)
	f.adapter.fetchErr = outputFailureError{}
	job := f.create(t, ImageJobInput{Background: "transparent"})
	f.service.processMediaJob(context.Background(), job.ID)
	fetches := 0
	for _, call := range f.adapter.Calls() {
		if call == "fetch" {
			fetches++
		}
	}
	if stored := f.job(t, job.ID); stored.Status != media.StatusFailed || stored.ErrorCode != "media_download_failed" || fetches != 1 {
		t.Fatalf("job = %s %s, fetches = %d", stored.Status, stored.ErrorCode, fetches)
	}
}

func TestImageJobsQueueApartFromVideoJobs(t *testing.T) {
	f := newImageJobFixture(t, 1)
	// The image job is queued on creation.
	job := f.create(t, ImageJobInput{})
	if !f.service.enqueueMediaJob("video_queued_job") {
		t.Fatal("video job not queued")
	}
	if len(f.service.imageQueue) != 1 || len(f.service.mediaQueue) != 1 {
		t.Fatalf("image queue = %d video queue = %d", len(f.service.imageQueue), len(f.service.mediaQueue))
	}
	// With every video worker busy (here: none running), the image pool runs the job.
	ctx, stop := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	f.service.runMediaQueue(ctx, f.service.imageQueue, &workers)
	defer func() { stop(); workers.Wait() }()
	for deadline := time.Now().Add(10 * time.Second); f.job(t, job.ID).Status != media.StatusCompleted; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("image job = %s", f.job(t, job.ID).Status)
		}
	}
	if len(f.service.mediaQueue) != 1 {
		t.Fatal("the image pool took a video job")
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

// A worker that died after saving the image left only the terminal write:
// recovery completes the job without drawing the image again.
func TestRecoveredImageJobWithItsImageCompletesWithoutRedrawing(t *testing.T) {
	f := newImageJobFixture(t, 1)
	ctx := context.Background()
	job := f.create(t, ImageJobInput{})
	now := time.Now().UTC()
	claimed, ok, err := f.jobs.TryClaimMediaJob(ctx, job.ID, now, now.Add(-time.Second), "claim_dead_after_delivery")
	if err != nil || !ok {
		t.Fatalf("claim = %v %v", ok, err)
	}
	claimed.ResultAssetID = "asset_delivered_000001"
	if err := f.jobs.UpdateMediaJob(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	f.service.processMediaJob(ctx, job.ID)
	if calls := f.adapter.Calls(); len(calls) != 0 {
		t.Fatalf("recovery drew again: %v", calls)
	}
	if stored := f.job(t, job.ID); stored.Status != media.StatusCompleted || stored.UsageRecordedAt == nil {
		t.Fatalf("recovered job = %#v", stored)
	}
	if billed, reserved := f.billing(t); billed != 400_000_000 || reserved != 0 {
		t.Fatalf("billed = %d reserved = %d", billed, reserved)
	}
}
