package gateway

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	pricingdomain "github.com/chenyme/grok2api/backend/internal/domain/pricing"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

// checkpointVideoAdapter persists an upstream ID like Console and resumes
// from it instead of creating another job.
type checkpointVideoAdapter struct {
	mu    sync.Mutex
	calls []string
	fail  error
	// submitted, when set, runs after the submission checkpoint is saved.
	submitted func()
	// seconds is the shortened duration the result reports.
	seconds int
}

func (a *checkpointVideoAdapter) Provider() account.Provider { return account.ProviderWeb }

func (a *checkpointVideoAdapter) Definition() provider.Definition {
	definition := testConversationDefinition(account.ProviderWeb)
	definition.Media.VideoGeneration = true
	return definition
}

func (a *checkpointVideoAdapter) GenerateVideo(_ context.Context, request provider.VideoRequest) (provider.VideoResult, error) {
	a.mu.Lock()
	a.calls = append(a.calls, fmt.Sprintf("%d:%s", request.Credential.ID, request.Checkpoint))
	a.mu.Unlock()
	if !strings.Contains(request.Checkpoint, "request_id") {
		if err := request.SaveCheckpoint(`{"submitting":true}`); err != nil {
			return provider.VideoResult{}, err
		}
		if a.submitted != nil {
			a.submitted()
		}
		if a.fail != nil {
			return provider.VideoResult{}, a.fail
		}
		if err := request.SaveCheckpoint(`{"request_id":"created"}`); err != nil {
			return provider.VideoResult{}, err
		}
	}
	return provider.VideoResult{URL: "https://assets.example/video.mp4", ContentType: "video/mp4", AssetID: "video_asset_checkpoint", Seconds: a.seconds}, nil
}

// videoJobFixture serves grok-imagine-video from two Web accounts through
// checkpointVideoAdapter; created jobs are pinned to the second account.
type videoJobFixture struct {
	service  *Service
	jobs     *relational.MediaJobRepository
	adapter  *checkpointVideoAdapter
	prices   *pricingapp.Service
	key      clientkey.Key
	accounts []account.Credential
	route    model.Route
}

func newVideoJobFixture(t *testing.T) *videoJobFixture {
	t.Helper()
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "video-job.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo, modelRepo, auditRepo := relational.NewAccountRepository(database), relational.NewModelRepository(database), relational.NewAuditRepository(database)
	mediaRepo, keyRepo := relational.NewMediaJobRepository(database), relational.NewClientKeyRepository(database)
	key, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "video-checkpoint", Prefix: "video-checkpoint", SecretHash: strings.Repeat("c", 64),
		EncryptedSecret: "encrypted", Enabled: true, RPMLimit: 60, MaxConcurrent: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	var accounts []account.Credential
	for index, priority := range []int{200, 100} {
		name := fmt.Sprintf("checkpoint-%d", index)
		credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierSuper,
			Name: name, SourceKey: name, EncryptedAccessToken: name + "-token", ExpiresAt: time.Now().Add(time.Hour),
			Enabled: true, AuthStatus: account.AuthStatusActive, Priority: priority, MaxConcurrent: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		accounts = append(accounts, credential)
	}
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderWeb, []string{"grok-imagine-video"}); err != nil {
		t.Fatal(err)
	}
	for _, credential := range accounts {
		if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-imagine-video"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	route, err := modelRepo.GetByProviderUpstream(ctx, account.ProviderWeb, "grok-imagine-video")
	if err != nil {
		t.Fatal(err)
	}
	adapter := &checkpointVideoAdapter{}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(keyRepo, nil, nil, 60, 4, nil), registry, selector, nil, 3)
	prices, err := pricingapp.NewService(ctx, relational.NewModelPriceRepository(database), func(account.Provider) bool { return true }, registry.PricingModel)
	if err != nil {
		t.Fatal(err)
	}
	service.SetPricing(prices)
	service.ConfigureMedia(mediaRepo, 1)
	return &videoJobFixture{service: service, jobs: mediaRepo, adapter: adapter, prices: prices, key: key, accounts: accounts, route: route}
}

func (f *videoJobFixture) create(t *testing.T, id string) media.Job {
	t.Helper()
	now := time.Now().UTC()
	job := media.Job{
		ID: id, Kind: media.KindVideo, RequestID: "request-" + id, ClientKeyID: f.key.ID, ClientKeyName: f.key.Name,
		AccountID: f.accounts[1].ID, AccountName: f.accounts[1].Name, Provider: string(account.ProviderWeb),
		Model: f.route.PublicID, ModelRouteID: f.route.ID, UpstreamModel: f.route.UpstreamModel,
		Operation: media.VideoOperationGenerate, Prompt: "test", Seconds: 5, Quality: "720p",
		Status: media.StatusQueued, InputJSON: `{}`, CreatedAt: now, UpdatedAt: now,
	}
	if err := f.jobs.CreateMediaJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	return job
}

func (f *videoJobFixture) job(t *testing.T, id string) media.Job {
	t.Helper()
	stored, err := f.jobs.GetMediaJob(context.Background(), id, f.key.ID)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func TestVideoJobResumesCheckpointOnSubmittingAccountAndMarksUnknownSubmissionUncertain(t *testing.T) {
	ctx := context.Background()
	f := newVideoJobFixture(t)

	// A worker died after the upstream accepted the job: recovery resumes on
	// the submitting account from its checkpoint.
	resumed := f.create(t, "video_checkpoint_resume")
	now := time.Now().UTC()
	claimed, ok, err := f.jobs.TryClaimMediaJob(ctx, resumed.ID, now, now.Add(-time.Second), "claim_dead_video_worker")
	if err != nil || !ok {
		t.Fatalf("claim = %v %v", ok, err)
	}
	claimed.Checkpoint, claimed.Submitted = `{"request_id":"up-1"}`, true
	if err := f.jobs.UpdateMediaJob(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	f.service.processMediaJob(ctx, resumed.ID)
	stored := f.job(t, resumed.ID)
	if want := fmt.Sprintf("%d:{\"request_id\":\"up-1\"}", f.accounts[1].ID); len(f.adapter.calls) != 1 || f.adapter.calls[0] != want {
		t.Fatalf("resume calls = %v, want %s", f.adapter.calls, want)
	}
	if stored.Status != media.StatusCompleted || stored.AccountID != f.accounts[1].ID || stored.Generation != 2 {
		t.Fatalf("resumed job = %#v", stored)
	}

	// The create may have reached the upstream without returning an ID.
	f.adapter.calls, f.adapter.fail = nil, provider.WrapVideoStage(provider.VideoStageSubmitted, 0, errors.New("create response lost"))
	unknown := f.create(t, "video_checkpoint_unknown")
	f.service.processMediaJob(ctx, unknown.ID)
	stored = f.job(t, unknown.ID)
	if stored.Status != media.StatusUncertain || stored.ErrorCode != "submission_uncertain" || !stored.Submitted || len(f.adapter.calls) != 1 {
		t.Fatalf("unknown submission job = %#v calls = %v", stored, f.adapter.calls)
	}
}

// A running video job holds the short heartbeat lease, so recovery picks up
// a dead worker's job within minutes instead of hours.
func TestVideoJobHoldsTheHeartbeatLease(t *testing.T) {
	f := newVideoJobFixture(t)
	job := f.create(t, "video_lease")
	var lease time.Duration
	f.adapter.submitted = func() { lease = time.Until(*f.job(t, job.ID).LeaseUntil) }
	f.service.processMediaJob(context.Background(), job.ID)
	if lease <= 0 || lease > mediaJobLease {
		t.Fatalf("lease while running = %v, want at most %v", lease, mediaJobLease)
	}
}

// A job whose workers keep dying ends instead of being reclaimed forever.
func TestVideoJobEndsAfterTooManyClaims(t *testing.T) {
	ctx := context.Background()
	f := newVideoJobFixture(t)
	job := f.create(t, "video_claims")
	for i := range mediaJobMaxClaims {
		now := time.Now().UTC()
		if _, ok, err := f.jobs.TryClaimMediaJob(ctx, job.ID, now, now.Add(-time.Second), fmt.Sprintf("claim_dead_worker_%d", i)); err != nil || !ok {
			t.Fatalf("claim %d = %v %v", i, ok, err)
		}
	}
	f.service.processMediaJob(ctx, job.ID)
	if stored := f.job(t, job.ID); stored.Status != media.StatusFailed || stored.ErrorCode != "recovery_exhausted" || len(f.adapter.calls) != 0 {
		t.Fatalf("job = %#v calls = %v", stored, f.adapter.calls)
	}
}

// A provider that generates a shorter video than requested (the Web free-tier
// cap) gets the job recorded and billed at the delivered duration.
func TestShortenedVideoIsBilledAtItsDuration(t *testing.T) {
	f := newVideoJobFixture(t)
	if _, err := f.prices.Put(context.Background(), pricingapp.Input{Provider: string(account.ProviderWeb), PricingModel: "grok-imagine-video", Operation: string(pricingdomain.OperationVideo),
		Rates: map[pricingdomain.Component]string{pricingdomain.OutputSecond: "0.1"}}); err != nil {
		t.Fatal(err)
	}
	job := f.create(t, "video_shortened")
	f.adapter.seconds = 3
	f.service.processMediaJob(context.Background(), job.ID)
	want, ok := f.prices.QuoteVideo(account.ProviderWeb, "grok-imagine-video", job.Quality, 3, 0)
	stored := f.job(t, job.ID)
	if !ok || stored.Status != media.StatusCompleted || stored.Seconds != 3 || stored.Price == nil || stored.Price.CostInUSDTicks != want.CostInUSDTicks {
		t.Fatalf("job = %s %ds price %+v, want completed 3s at %+v (priced %t)", stored.Status, stored.Seconds, stored.Price, want, ok)
	}
}
