package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	pricingapp "github.com/chenyme/grok2api/backend/internal/application/pricing"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	pricingdomain "github.com/chenyme/grok2api/backend/internal/domain/pricing"
	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/chatgpt"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func onePixelPNG(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, image.NewGray(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// asyncImageAdapter keeps the native ChatGPT declaration and replaces the
// upstream image steps.
type asyncImageAdapter struct {
	*chatgpt.Adapter
	image  []byte
	mu     sync.Mutex
	calls  []string
	submit func(provider.ImageJobRequest, func(provider.ImageCheckpoint) error) (provider.ImageCheckpoint, error)
	// fetchErr, when set, fails every download.
	fetchErr error
}

func (a *asyncImageAdapter) record(call string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, call)
}

func (a *asyncImageAdapter) Calls() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.calls...)
}

func (a *asyncImageAdapter) ListModels(context.Context, account.Credential) ([]string, error) {
	return []string{"gpt-5"}, nil
}

func (a *asyncImageAdapter) SubmitImage(_ context.Context, request provider.ImageJobRequest, save func(provider.ImageCheckpoint) error) (provider.ImageCheckpoint, error) {
	a.record(fmt.Sprintf("submit:%d", request.Credential.ID))
	if a.submit != nil {
		return a.submit(request, save)
	}
	if err := save(provider.ImageCheckpoint{Submitted: true}); err != nil {
		return provider.ImageCheckpoint{}, err
	}
	cp := provider.ImageCheckpoint{Submitted: true, UpstreamID: "conv"}
	return cp, save(cp)
}

func (a *asyncImageAdapter) PollImage(_ context.Context, request provider.ImageJobRequest, cp provider.ImageCheckpoint) (provider.ImageCheckpoint, error) {
	a.record(fmt.Sprintf("poll:%d:%s", request.Credential.ID, cp.UpstreamID))
	cp.Outputs = []string{"file"}
	return cp, nil
}

func (a *asyncImageAdapter) FetchImage(context.Context, provider.ImageJobRequest, provider.ImageCheckpoint) ([]byte, error) {
	a.record("fetch")
	if a.fetchErr != nil {
		return nil, a.fetchErr
	}
	return a.image, nil
}

type statusError struct{ status int }

func (e statusError) Error() string       { return fmt.Sprintf("upstream status %d", e.status) }
func (e statusError) HTTPStatusCode() int { return e.status }

type imageJobFixture struct {
	service  *Service
	jobs     *relational.MediaJobRepository
	keys     *relational.ClientKeyRepository
	audits   *relational.AuditRepository
	assets   *relational.MediaAssetRepository
	prices   *pricingapp.Service
	adapter  *asyncImageAdapter
	key      clientkey.Key
	accounts []account.Credential
}

// newImageJobFixture serves gpt-image-2 from ChatGPT accounts (priority
// descending) with a 0.04 USD custom image price and a billing-limited key.
func newImageJobFixture(t *testing.T, accounts int) *imageJobFixture {
	t.Helper()
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "image-jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo, modelRepo, auditRepo := relational.NewAccountRepository(database), relational.NewModelRepository(database), relational.NewAuditRepository(database)
	cipher := testCipher(t)
	adapter := &asyncImageAdapter{Adapter: chatgpt.NewAdapter(chatgpt.Config{ImageConversationModel: "gpt-5", VisionTokens: 256}, nil, cipher, nil), image: onePixelPNG(t)}
	registry := provider.NewRegistry(adapter)
	if err := registry.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.UpsertRoutes(ctx, chatgpt.Routes()); err != nil {
		t.Fatal(err)
	}
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, cipher, nil)
	models := modelapp.NewService(modelRepo, accountRepo, accountService, registry)
	fixture := &imageJobFixture{adapter: adapter, audits: auditRepo}
	for index := range accounts {
		token, err := cipher.Encrypt("AT")
		if err != nil {
			t.Fatal(err)
		}
		name := fmt.Sprintf("gpt-%d", index)
		credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderChatGPT, AuthType: account.AuthTypeOAuth, Name: name, UserID: name, SourceKey: name,
			EncryptedAccessToken: token, ExpiresAt: time.Now().Add(24 * time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive,
			MaxConcurrent: 1, Priority: 100 - index, Plan: "plus", // Plus: the Codex image routes resolve too
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := models.SyncAccount(ctx, credential.ID); err != nil {
			t.Fatal(err)
		}
		fixture.accounts = append(fixture.accounts, credential)
	}
	fixture.keys = relational.NewClientKeyRepository(database)
	fixture.key, err = fixture.keys.Create(ctx, clientkey.Key{
		Name: "image-key", Prefix: "imagekey", SecretHash: strings.Repeat("a", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 60, MaxConcurrent: 4, ProviderScope: clientkey.AllProviders(), BillingLimitUSDTicks: 1_000_000_000_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.prices, err = pricingapp.NewService(ctx, relational.NewModelPriceRepository(database), func(account.Provider) bool { return true }, registry.PricingModel)
	if err != nil {
		t.Fatal(err)
	}
	fixture.setPrice(t, "0.04")
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	keyService := clientkeyapp.NewService(fixture.keys, memory.NewRateLimiter(), memory.NewConcurrencyLimiter(), 60, 4, cipher)
	fixture.service = NewService(modelRepo, auditRepo, accountService, keyService, registry, selector, relational.NewResponseRepository(database), 3)
	fixture.service.SetPricing(fixture.prices)
	fixture.jobs = relational.NewMediaJobRepository(database)
	fixture.service.ConfigureMedia(fixture.jobs, 1)
	objects, err := localmedia.NewLocalStore(filepath.Join(t.TempDir(), "objects"))
	if err != nil {
		t.Fatal(err)
	}
	fixture.assets = relational.NewMediaAssetRepository(database)
	fixture.service.ConfigureMediaAssets(mediaapp.NewService(fixture.assets, fixture.jobs, objects, nil, mediaapp.Config{PublicBaseURL: "https://example.test", MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30, CleanupThresholdPercent: 90, CleanupInterval: time.Hour}))
	return fixture
}

func (f *imageJobFixture) setPrice(t *testing.T, usd string) pricingdomain.Price {
	t.Helper()
	price, err := f.prices.Put(context.Background(), pricingapp.Input{Provider: string(account.ProviderChatGPT), PricingModel: "gpt-image-2", Operation: string(pricingdomain.OperationImage), Rates: map[pricingdomain.Component]string{pricingdomain.OutputImage: usd}})
	if err != nil {
		t.Fatal(err)
	}
	return price
}

func (f *imageJobFixture) create(t *testing.T, input ImageJobInput) media.Job {
	t.Helper()
	if input.PublicModel == "" {
		input.PublicModel = "gpt-image-2"
	}
	if input.Prompt == "" {
		input.Prompt = "a fox"
	}
	input.ClientKey = f.key
	job, err := f.service.CreateImageJob(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func (f *imageJobFixture) job(t *testing.T, id string) media.Job {
	t.Helper()
	job, err := f.jobs.GetMediaJob(context.Background(), id, f.key.ID)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func (f *imageJobFixture) billing(t *testing.T) (billed, reserved int64) {
	t.Helper()
	key, err := f.keys.Get(context.Background(), f.key.ID)
	if err != nil {
		t.Fatal(err)
	}
	return key.BilledUsageUSDTicks, key.ReservedUsageUSDTicks
}

func (f *imageJobFixture) auditsByEvent(t *testing.T) map[string]audit.Record {
	t.Helper()
	values, _, err := f.audits.List(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]audit.Record{}
	for _, value := range values {
		result[value.EventID] = value
	}
	return result
}

// crashAfter simulates a worker that claimed the job, saved checkpoint and died.
func (f *imageJobFixture) crashAfter(t *testing.T, id string, checkpoint string) media.Job {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	claimed, ok, err := f.jobs.TryClaimMediaJob(ctx, id, now, now.Add(-time.Second), "claim_crashed_worker_token")
	if err != nil || !ok {
		t.Fatalf("claim = %v %v", ok, err)
	}
	claimed.Checkpoint, claimed.Submitted = checkpoint, true
	if err := f.jobs.UpdateMediaJob(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	return claimed
}

func TestImageJobDeliversEachImageAndBillsItsReservedShare(t *testing.T) {
	f := newImageJobFixture(t, 1)
	job := f.create(t, ImageJobInput{Count: 2, Size: "1024x1024"})
	if job.Status != media.StatusQueued || job.Kind != media.KindImage || !strings.HasPrefix(job.ID, ImageJobIDPrefix) || job.Price == nil || job.Price.CostInUSDTicks != 800_000_000 {
		t.Fatalf("created job = %#v price=%#v", job, job.Price)
	}
	if _, reserved := f.billing(t); reserved != 800_000_000 {
		t.Fatalf("reserved = %d", reserved)
	}
	// A price edit after submission does not change what the job settles at.
	f.setPrice(t, "1.00")
	f.service.processMediaJob(context.Background(), job.ID)

	stored := f.job(t, job.ID)
	if stored.Status != media.StatusCompleted || len(stored.ResultAssetIDs) != 2 || stored.Progress != 100 || stored.UsageRecordedAt == nil || stored.Submitted {
		t.Fatalf("stored job = %#v", stored)
	}
	want := []string{fmt.Sprintf("submit:%d", f.accounts[0].ID), fmt.Sprintf("poll:%d:conv", f.accounts[0].ID), "fetch"}
	if calls := f.adapter.Calls(); !slices.Equal(calls, append(append([]string{}, want...), want...)) {
		t.Fatalf("calls = %v", calls)
	}
	if billed, reserved := f.billing(t); billed != 800_000_000 || reserved != 0 {
		t.Fatalf("billed = %d reserved = %d", billed, reserved)
	}
	audits := f.auditsByEvent(t)
	for index := range 2 {
		record, ok := audits[imageUsageEventID(job.ID, index)]
		if !ok || record.StatusCode != 200 || record.EstimatedCostInUSDTicks != 400_000_000 || record.PriceID != job.Price.PriceID || record.MediaOutputImages != 1 || record.Operation != audit.OperationImage {
			t.Fatalf("audit %d = %#v", index, record)
		}
	}
	asset, err := f.assets.GetMediaAsset(context.Background(), stored.ResultAssetIDs[0])
	if err != nil || asset.Origin.JobID != job.ID || asset.Origin.Operation != string(audit.OperationImage) || asset.Origin.AccountID != f.accounts[0].ID {
		t.Fatalf("asset origin = %#v err = %v", asset.Origin, err)
	}
	images, usage, err := f.service.ImageJobOutput(context.Background(), stored)
	if err != nil || len(images) != 2 || images[0].Width != 1 || usage["output_tokens"] == nil {
		t.Fatalf("output = %d images, usage %v, err %v", len(images), usage, err)
	}
}

func TestImageJobRecoveryPollsTheCheckpointWithoutResubmitting(t *testing.T) {
	f := newImageJobFixture(t, 1)
	job := f.create(t, ImageJobInput{})
	crashed := f.crashAfter(t, job.ID, `{"submitted":true,"upstream_id":"conv-crashed"}`)

	f.service.processMediaJob(context.Background(), job.ID)

	if calls := f.adapter.Calls(); !slices.Equal(calls, []string{fmt.Sprintf("poll:%d:conv-crashed", f.accounts[0].ID), "fetch"}) {
		t.Fatalf("recovery calls = %v", calls)
	}
	stored := f.job(t, job.ID)
	if stored.Status != media.StatusCompleted || len(stored.ResultAssetIDs) != 1 || stored.Generation != 2 {
		t.Fatalf("recovered job = %#v", stored)
	}
	// The crashed worker's claim is fenced out.
	crashed.Status = media.StatusFailed
	if err := f.jobs.UpdateMediaJob(context.Background(), crashed); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("stale claim write = %v", err)
	}
	if billed, _ := f.billing(t); billed != 400_000_000 {
		t.Fatalf("billed = %d", billed)
	}
}

func TestImageJobSubmittedWithoutUpstreamIDIsUncertainAndRetriedOnce(t *testing.T) {
	f := newImageJobFixture(t, 1)
	source := "data:image/png;base64,iVBORw0KGgo="
	job := f.create(t, ImageJobInput{ImageURLs: []string{source}, Prompt: "edit me"})
	f.crashAfter(t, job.ID, `{"submitted":true}`)

	f.service.processMediaJob(context.Background(), job.ID)

	stored := f.job(t, job.ID)
	if stored.Status != media.StatusUncertain || stored.ErrorCode != "submission_uncertain" || stored.UsageRecordedAt == nil || len(f.adapter.Calls()) != 0 {
		t.Fatalf("uncertain job = %#v calls = %v", stored, f.adapter.Calls())
	}
	if billed, reserved := f.billing(t); billed != 0 || reserved != 0 {
		t.Fatalf("uncertain billing: billed %d reserved %d", billed, reserved)
	}
	if record := f.auditsByEvent(t)[imageUsageEventID(job.ID, 0)]; record.StatusCode < 400 || record.ErrorCode != "submission_uncertain" || record.Operation != audit.OperationImageEdit {
		t.Fatalf("failure audit = %#v", record)
	}
	ctx := context.Background()
	retried, err := f.service.RetryMediaJob(ctx, job.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if retried.ID == job.ID || retried.Status != media.StatusQueued || retried.Prompt != "edit me" || retried.InputImageCount != 1 || retried.IdempotencyKey != "retry:"+job.ID {
		t.Fatalf("retried job = %#v", retried)
	}
	if input, err := f.jobs.GetMediaJobInput(ctx, retried.ID); err != nil || !strings.Contains(input, source) {
		t.Fatalf("retry input = %q err = %v", input, err)
	}
	again, err := f.service.RetryMediaJob(ctx, job.ID, "")
	if err != nil || again.ID != retried.ID {
		t.Fatalf("second retry = %s err = %v, want %s", again.ID, err, retried.ID)
	}
	f.service.processMediaJob(ctx, retried.ID)
	if _, err := f.service.RetryMediaJob(ctx, retried.ID, ""); !errors.Is(err, ErrMediaJobNotRetryable) {
		t.Fatalf("retry of completed job = %v", err)
	}
}

func TestImageJobFailsOverOnlyBeforeSubmission(t *testing.T) {
	f := newImageJobFixture(t, 2)
	first, second := f.accounts[0].ID, f.accounts[1].ID
	f.adapter.submit = func(request provider.ImageJobRequest, save func(provider.ImageCheckpoint) error) (provider.ImageCheckpoint, error) {
		if request.Credential.ID == first {
			return provider.ImageCheckpoint{}, statusError{status: 500}
		}
		cp := provider.ImageCheckpoint{Submitted: true, UpstreamID: "conv"}
		return cp, save(cp)
	}
	job := f.create(t, ImageJobInput{})
	if job.AccountID != first {
		t.Fatalf("pinned account = %d, want %d", job.AccountID, first)
	}
	f.service.processMediaJob(context.Background(), job.ID)
	stored := f.job(t, job.ID)
	if stored.Status != media.StatusCompleted || stored.AccountID != second {
		t.Fatalf("failover job = %#v", stored)
	}

	// Once the request may have reached the upstream it is never resent.
	f.adapter.mu.Lock()
	f.adapter.calls = nil
	f.adapter.mu.Unlock()
	f.adapter.submit = func(_ provider.ImageJobRequest, save func(provider.ImageCheckpoint) error) (provider.ImageCheckpoint, error) {
		cp := provider.ImageCheckpoint{Submitted: true}
		if err := save(cp); err != nil {
			return provider.ImageCheckpoint{}, err
		}
		return cp, errors.New("stream broke")
	}
	job = f.create(t, ImageJobInput{Prompt: "another"})
	f.service.processMediaJob(context.Background(), job.ID)
	stored = f.job(t, job.ID)
	if calls := f.adapter.Calls(); stored.Status != media.StatusUncertain || len(calls) != 1 {
		t.Fatalf("post-submit failure job = %#v calls = %v", stored, calls)
	}
}

func TestCreateImageJobIsIdempotentPerClientKey(t *testing.T) {
	f := newImageJobFixture(t, 1)
	first := f.create(t, ImageJobInput{IdempotencyKey: "same"})
	second := f.create(t, ImageJobInput{IdempotencyKey: "same"})
	if first.ID != second.ID {
		t.Fatalf("idempotent jobs = %s, %s", first.ID, second.ID)
	}
	if _, reserved := f.billing(t); reserved != 400_000_000 {
		t.Fatalf("reserved = %d, want one reservation", reserved)
	}
	if _, err := f.service.CreateImageJob(context.Background(), ImageJobInput{ClientKey: f.key, PublicModel: "gpt-image-2", Prompt: "different", IdempotencyKey: "same"}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("different payload = %v", err)
	}
}

func TestCancelImageJobOnlyBeforeSubmission(t *testing.T) {
	f := newImageJobFixture(t, 1)
	ctx := context.Background()
	job := f.create(t, ImageJobInput{})
	canceled, err := f.service.CancelImageJob(ctx, f.key, job.ID)
	if err != nil || canceled.Status != media.StatusCanceled {
		t.Fatalf("cancel queued = %#v err = %v", canceled, err)
	}
	if _, reserved := f.billing(t); reserved != 0 {
		t.Fatalf("reserved after cancel = %d", reserved)
	}
	if again, err := f.service.CancelImageJob(ctx, f.key, job.ID); err != nil || again.Status != media.StatusCanceled {
		t.Fatalf("repeated cancel = %#v err = %v", again, err)
	}
	f.service.processMediaJob(ctx, job.ID)
	if len(f.adapter.Calls()) != 0 {
		t.Fatalf("canceled job ran: %v", f.adapter.Calls())
	}

	submitted := f.create(t, ImageJobInput{Prompt: "in flight"})
	f.crashAfter(t, submitted.ID, `{"submitted":true,"upstream_id":"conv"}`)
	if _, err := f.service.CancelImageJob(ctx, f.key, submitted.ID); !errors.Is(err, ErrMediaJobNotCancellable) {
		t.Fatalf("cancel submitted = %v", err)
	}
	if _, err := f.service.CancelImageJob(ctx, f.key, "video_unknown"); !errors.Is(err, ErrResponseNotFound) {
		t.Fatalf("cancel non-image id = %v", err)
	}
}

func TestImageToolModelFallsBackToDefaultOnlyForAsyncTextModels(t *testing.T) {
	f := newImageJobFixture(t, 1)
	ctx := context.Background()
	if got := f.service.ImageToolModel(ctx, "", "gpt-image-2"); got != "gpt-image-2" {
		t.Fatalf("image top-level model = %q", got)
	}
	if got := f.service.ImageToolModel(ctx, "", "gpt-5"); got != "" {
		t.Fatalf("no default = %q", got)
	}
	f.service.UpdateDefaultImageModel("gpt-image-2")
	if got := f.service.ImageToolModel(ctx, "", "gpt-5"); got != "gpt-image-2" {
		t.Fatalf("default for ChatGPT text model = %q", got)
	}
	if got := f.service.ImageToolModel(ctx, "gpt-5", "gpt-5"); got != "" {
		t.Fatalf("explicit non-image tool model = %q", got)
	}
	if got := f.service.ImageToolModel(ctx, "", "grok-4"); got != "" {
		t.Fatalf("unknown top-level model = %q", got)
	}
}

func TestImageJobFailureAuditKeepsUpstreamDiagnostic(t *testing.T) {
	f := newImageJobFixture(t, 1)
	f.adapter.fetchErr = downloadDeniedError{}
	job := f.create(t, ImageJobInput{})
	f.service.processMediaJob(context.Background(), job.ID)
	if stored := f.job(t, job.ID); stored.Status != media.StatusFailed || stored.ErrorCode != "media_download_failed" {
		t.Fatalf("job = %#v", stored)
	}
	record, ok := f.auditsByEvent(t)[imageUsageEventID(job.ID, 0)]
	if !ok {
		t.Fatal("missing failure audit")
	}
	stored, err := f.audits.Get(context.Background(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Attempts) != 1 || stored.Attempts[0].UpstreamStatusCode == nil || *stored.Attempts[0].UpstreamStatusCode != 403 || !strings.Contains(string(stored.Attempts[0].ResponseBody), "File stream access denied") {
		t.Fatalf("attempts = %#v", stored.Attempts)
	}
}

// ResumeMediaJob completes the video test double; resumes are image-only.
func (r *videoUsageRepository) ResumeMediaJob(context.Context, string, int, int, time.Time) (media.Job, error) {
	return r.job, repository.ErrConflict
}

func (f *imageJobFixture) resetCalls() {
	f.adapter.mu.Lock()
	f.adapter.calls = nil
	f.adapter.mu.Unlock()
}

func TestImageJobResumable(t *testing.T) {
	resumable := media.Job{Kind: media.KindImage, Status: media.StatusFailed, AccountID: 1, Submitted: true, Checkpoint: `{"submitted":true,"upstream_id":"conv","outputs":["file"]}`}
	if !ImageJobResumable(resumable) {
		t.Fatal("failed job with an upstream ID is not resumable")
	}
	for name, change := range map[string]func(*media.Job){
		"video":          func(job *media.Job) { job.Kind = media.KindVideo },
		"uncertain":      func(job *media.Job) { job.Status = media.StatusUncertain },
		"completed":      func(job *media.Job) { job.Status = media.StatusCompleted },
		"not submitted":  func(job *media.Job) { job.Submitted, job.Checkpoint = false, "{}" },
		"no upstream id": func(job *media.Job) { job.Checkpoint = `{"submitted":true}` },
		"no account":     func(job *media.Job) { job.AccountID = 0 },
	} {
		job := resumable
		change(&job)
		if ImageJobResumable(job) {
			t.Errorf("%s: resumable", name)
		}
	}
}

func TestRetryResumesDownloadFailedImageJobWithoutResubmitting(t *testing.T) {
	f := newImageJobFixture(t, 1)
	ctx := context.Background()
	f.adapter.fetchErr = downloadDeniedError{}
	job := f.create(t, ImageJobInput{})
	f.service.processMediaJob(ctx, job.ID)
	failed := f.job(t, job.ID)
	if failed.Status != media.StatusFailed || failed.ErrorCode != "media_download_failed" || !ImageJobResumable(failed) {
		t.Fatalf("failed job = %#v", failed)
	}
	if billed, reserved := f.billing(t); billed != 0 || reserved != 0 {
		t.Fatalf("after failure: billed %d reserved %d", billed, reserved)
	}

	// The first resume fails the same way: its own failure audit, its reservation released.
	f.resetCalls()
	resumed, err := f.service.RetryMediaJob(ctx, job.ID, "")
	if err != nil || resumed.ID != job.ID || resumed.Status != media.StatusInProgress || resumed.ErrorCode != "" || resumed.CompletedAt != nil || resumed.ResumeGeneration != 1 {
		t.Fatalf("resume = %#v err = %v", resumed, err)
	}
	if public, err := f.service.GetImageJob(ctx, f.key, job.ID); err != nil || public.Status != media.StatusInProgress {
		t.Fatalf("public status = %s err = %v", public.Status, err)
	}
	if _, reserved := f.billing(t); reserved != 400_000_000 {
		t.Fatalf("resume reserved = %d", reserved)
	}
	f.service.processMediaJob(ctx, job.ID)
	if stored := f.job(t, job.ID); stored.Status != media.StatusFailed || stored.ErrorCode != "media_download_failed" || !ImageJobResumable(stored) {
		t.Fatalf("failed resume = %#v", stored)
	}
	if billed, reserved := f.billing(t); billed != 0 || reserved != 0 {
		t.Fatalf("after failed resume: billed %d reserved %d", billed, reserved)
	}

	// The second resume delivers; a repeated click while it runs changes nothing.
	f.adapter.fetchErr = nil
	f.resetCalls()
	if resumed, err = f.service.RetryMediaJob(ctx, job.ID, ""); err != nil || resumed.ID != job.ID || resumed.ResumeGeneration != 2 {
		t.Fatalf("second resume = %#v err = %v", resumed, err)
	}
	if again, err := f.service.RetryMediaJob(ctx, job.ID, ""); !errors.Is(err, ErrMediaJobNotRetryable) || again.ID != "" {
		t.Fatalf("repeated resume of a running job = %#v err = %v", again, err)
	}
	f.service.processMediaJob(ctx, job.ID)
	if calls := f.adapter.Calls(); !slices.Equal(calls, []string{fmt.Sprintf("poll:%d:conv", f.accounts[0].ID), "fetch"}) {
		t.Fatalf("resume calls = %v", calls)
	}
	stored := f.job(t, job.ID)
	if stored.Status != media.StatusCompleted || len(stored.ResultAssetIDs) != 1 || stored.UsageRecordedAt == nil || stored.Submitted || ImageJobResumable(stored) {
		t.Fatalf("resumed job = %#v", stored)
	}
	if billed, reserved := f.billing(t); billed != 400_000_000 || reserved != 0 {
		t.Fatalf("after resume: billed %d reserved %d", billed, reserved)
	}
	// Usage reconciliation replays the job's audits without billing twice.
	if err := f.service.recordImageJobAudits(ctx, stored, 0); err != nil {
		t.Fatal(err)
	}
	if billed, _ := f.billing(t); billed != 400_000_000 {
		t.Fatalf("billed after replay = %d", billed)
	}
	audits := f.auditsByEvent(t)
	original, first, second := imageUsageEventID(job.ID, 0), imageUsageEventID(job.ID, 0)+"_r1", imageUsageEventID(job.ID, 0)+"_r2"
	if record := audits[original]; record.StatusCode < 400 || record.ErrorCode != "media_download_failed" {
		t.Fatalf("original failure audit = %#v", record)
	}
	if record := audits[first]; record.StatusCode < 400 || record.EstimatedCostInUSDTicks != 0 {
		t.Fatalf("failed resume audit = %#v", record)
	}
	if record := audits[second]; record.StatusCode != 200 || record.EstimatedCostInUSDTicks != 400_000_000 || record.PriceID != job.Price.PriceID || record.MediaOutputImages != 1 {
		t.Fatalf("resume success audit = %#v", record)
	}
	if len(audits) != 3 {
		t.Fatalf("audits = %v", audits)
	}
	if _, err := f.service.RetryMediaJob(ctx, job.ID, ""); !errors.Is(err, ErrMediaJobNotRetryable) {
		t.Fatalf("retry of the completed job = %v", err)
	}
}

func TestConcurrentResumeRunsTheImageJobOnce(t *testing.T) {
	f := newImageJobFixture(t, 1)
	ctx := context.Background()
	f.adapter.fetchErr = downloadDeniedError{}
	job := f.create(t, ImageJobInput{})
	f.service.processMediaJob(ctx, job.ID)
	f.adapter.fetchErr = nil
	f.resetCalls()

	// Every request read the failed job before any resume committed: one
	// compare-and-set wins and the others resolve to it.
	stale := f.job(t, job.ID)
	var wait sync.WaitGroup
	results := make([]error, 4)
	for index := range results {
		wait.Add(1)
		go func() {
			defer wait.Done()
			resumed, err := f.service.resumeImageJob(ctx, stale, f.key)
			if err == nil && (resumed.ID != job.ID || resumed.Status != media.StatusInProgress) {
				err = fmt.Errorf("resumed %#v", resumed)
			}
			results[index] = err
		}()
	}
	wait.Wait()
	for _, err := range results {
		if err != nil {
			t.Fatalf("concurrent resume = %v", err)
		}
	}
	if _, reserved := f.billing(t); reserved != 400_000_000 {
		t.Fatalf("reserved = %d, want one reservation", reserved)
	}
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			f.service.processMediaJob(ctx, job.ID)
		}()
	}
	wait.Wait()
	if calls := f.adapter.Calls(); !slices.Equal(calls, []string{fmt.Sprintf("poll:%d:conv", f.accounts[0].ID), "fetch"}) {
		t.Fatalf("calls = %v", calls)
	}
	if billed, reserved := f.billing(t); billed != 400_000_000 || reserved != 0 {
		t.Fatalf("billed %d reserved %d", billed, reserved)
	}
}

func TestResumedImageJobStaysResumableAndCountsClaimsFromTheResume(t *testing.T) {
	f := newImageJobFixture(t, 1)
	ctx := context.Background()
	f.adapter.fetchErr = downloadDeniedError{}
	job := f.create(t, ImageJobInput{})
	f.service.processMediaJob(ctx, job.ID)
	if _, err := f.service.RetryMediaJob(ctx, job.ID, ""); err != nil {
		t.Fatal(err)
	}
	// Workers keep dying on the resumed job until its claims run out.
	for index := range mediaJobMaxClaims {
		now := time.Now().UTC()
		if _, ok, err := f.jobs.TryClaimMediaJob(ctx, job.ID, now, now.Add(-time.Second), fmt.Sprintf("claim_dead_worker_%02d", index)); err != nil || !ok {
			t.Fatalf("claim %d = %v %v", index, ok, err)
		}
	}
	f.resetCalls()
	f.service.processMediaJob(ctx, job.ID)
	stored := f.job(t, job.ID)
	if stored.Status != media.StatusFailed || stored.ErrorCode != "recovery_exhausted" || !ImageJobResumable(stored) || len(f.adapter.Calls()) != 0 {
		t.Fatalf("exhausted resume = %#v calls = %v", stored, f.adapter.Calls())
	}
	if billed, reserved := f.billing(t); billed != 0 || reserved != 0 {
		t.Fatalf("after exhausted resume: billed %d reserved %d", billed, reserved)
	}

	// A later resume starts a fresh claim budget although the generation is high.
	f.adapter.fetchErr = nil
	if _, err := f.service.RetryMediaJob(ctx, job.ID, ""); err != nil {
		t.Fatal(err)
	}
	f.service.processMediaJob(ctx, job.ID)
	if stored := f.job(t, job.ID); stored.Status != media.StatusCompleted || stored.Generation <= mediaJobMaxClaims {
		t.Fatalf("second resume = %#v", stored)
	}
	if billed, reserved := f.billing(t); billed != 400_000_000 || reserved != 0 {
		t.Fatalf("after resume: billed %d reserved %d", billed, reserved)
	}
}

func TestResumeOfMultiImageJobFetchesOnlyTheImageInFlight(t *testing.T) {
	f := newImageJobFixture(t, 1)
	ctx := context.Background()
	submits := 0
	f.adapter.submit = func(_ provider.ImageJobRequest, save func(provider.ImageCheckpoint) error) (provider.ImageCheckpoint, error) {
		submits++
		if submits == 2 {
			f.adapter.fetchErr = downloadDeniedError{}
		}
		cp := provider.ImageCheckpoint{Submitted: true, UpstreamID: fmt.Sprintf("conv-%d", submits)}
		return cp, save(cp)
	}
	job := f.create(t, ImageJobInput{Count: 3})
	f.service.processMediaJob(ctx, job.ID)
	failed := f.job(t, job.ID)
	if failed.Status != media.StatusFailed || len(failed.ResultAssetIDs) != 1 || !ImageJobResumable(failed) {
		t.Fatalf("failed job = %#v", failed)
	}
	if billed, reserved := f.billing(t); billed != 400_000_000 || reserved != 0 {
		t.Fatalf("after failure: billed %d reserved %d", billed, reserved)
	}

	f.adapter.fetchErr = nil
	f.resetCalls()
	if _, err := f.service.RetryMediaJob(ctx, job.ID, ""); err != nil {
		t.Fatal(err)
	}
	f.service.processMediaJob(ctx, job.ID)
	if calls := f.adapter.Calls(); !slices.Equal(calls, []string{fmt.Sprintf("poll:%d:conv-2", f.accounts[0].ID), "fetch"}) {
		t.Fatalf("resume calls = %v", calls)
	}
	stored := f.job(t, job.ID)
	if stored.Status != media.StatusFailed || stored.ErrorCode != "partial_result" || len(stored.ResultAssetIDs) != 2 || stored.ResultAssetIDs[0] != failed.ResultAssetIDs[0] || ImageJobResumable(stored) {
		t.Fatalf("resumed job = %#v", stored)
	}
	if billed, reserved := f.billing(t); billed != 800_000_000 || reserved != 0 {
		t.Fatalf("after resume: billed %d reserved %d", billed, reserved)
	}
	audits := f.auditsByEvent(t)
	if record := audits[imageUsageEventID(job.ID, 1)+"_r1"]; record.StatusCode != 200 || record.EstimatedCostInUSDTicks != 400_000_000 {
		t.Fatalf("resumed image audit = %#v", record)
	}
	if _, ok := audits[imageUsageEventID(job.ID, 2)]; ok || len(audits) != 3 {
		t.Fatalf("audits = %v", audits)
	}
}

// Codex image models answer synchronously, so an image job for one is refused
// before any reservation instead of failing after it starts.
func TestCreateImageJobRefusesSynchronousOnlyModels(t *testing.T) {
	f := newImageJobFixture(t, 1)
	ctx := context.Background()
	_, err := f.service.CreateImageJob(ctx, ImageJobInput{ClientKey: f.key, PublicModel: "codex-gpt-image-2", Prompt: "a fox"})
	var rejected *ImageRequestRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("err = %v, want a request rejection", err)
	}
	if _, reserved := f.billing(t); reserved != 0 {
		t.Fatalf("reserved = %d", reserved)
	}
	if f.service.IsImageJobModel(ctx, "codex-gpt-image-2") || !f.service.IsImageJobModel(ctx, "gpt-image-2") {
		t.Fatal("IsImageJobModel")
	}
}

// Saves extend the heartbeat lease instead of writing back the claim-time one.
func TestImageJobSavesKeepTheHeartbeatLease(t *testing.T) {
	f := newImageJobFixture(t, 1)
	var jobID string
	var lease time.Duration
	f.adapter.submit = func(_ provider.ImageJobRequest, save func(provider.ImageCheckpoint) error) (provider.ImageCheckpoint, error) {
		cp := provider.ImageCheckpoint{Submitted: true, UpstreamID: "conv"}
		if err := save(cp); err != nil {
			return cp, err
		}
		lease = time.Until(*f.job(t, jobID).LeaseUntil)
		return cp, nil
	}
	job := f.create(t, ImageJobInput{})
	jobID = job.ID
	f.service.processMediaJob(context.Background(), job.ID)
	if lease <= 0 || lease > mediaJobLease {
		t.Fatalf("lease after a save = %v, want at most %v", lease, mediaJobLease)
	}
}

// failingRoutes fails the first route lookup like a locked database.
type failingRoutes struct {
	routeResolver
	failed bool
}

func (r *failingRoutes) Get(ctx context.Context, id uint64) (modeldomain.Route, error) {
	if !r.failed {
		r.failed = true
		return modeldomain.Route{}, errors.New("database is locked")
	}
	return r.routeResolver.Get(ctx, id)
}

// A failed route lookup leaves the job to recovery instead of ending it.
func TestMediaJobSurvivesAFailedRouteLookup(t *testing.T) {
	f := newImageJobFixture(t, 1)
	job := f.create(t, ImageJobInput{})
	f.service.models = &failingRoutes{routeResolver: f.service.models}
	f.service.processMediaJob(context.Background(), job.ID)
	if stored := f.job(t, job.ID); stored.Status.Terminal() {
		t.Fatalf("a failed route lookup ended the job: %#v", stored)
	}
}
