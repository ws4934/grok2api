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
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
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
		if a.fail != nil {
			return provider.VideoResult{}, a.fail
		}
		if err := request.SaveCheckpoint(`{"request_id":"created"}`); err != nil {
			return provider.VideoResult{}, err
		}
	}
	return provider.VideoResult{URL: "https://assets.example/video.mp4", ContentType: "video/mp4", AssetID: "video_asset_checkpoint"}, nil
}

func TestVideoJobResumesCheckpointOnSubmittingAccountAndMarksUnknownSubmissionUncertain(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "video-checkpoint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
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
	service.ConfigureMedia(mediaRepo, 1)

	newJob := func(id string) media.Job {
		now := time.Now().UTC()
		job := media.Job{
			ID: id, Kind: media.KindVideo, RequestID: "request-" + id, ClientKeyID: key.ID, ClientKeyName: key.Name,
			AccountID: accounts[1].ID, AccountName: accounts[1].Name, Provider: string(account.ProviderWeb),
			Model: route.PublicID, ModelRouteID: route.ID, UpstreamModel: route.UpstreamModel,
			Operation: media.VideoOperationGenerate, Prompt: "test", Seconds: 5, Quality: "720p",
			Status: media.StatusQueued, InputJSON: `{}`, CreatedAt: now, UpdatedAt: now,
		}
		if err := mediaRepo.CreateMediaJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		return job
	}

	// A worker died after the upstream accepted the job: recovery resumes on
	// the submitting account from its checkpoint.
	resumed := newJob("video_checkpoint_resume")
	now := time.Now().UTC()
	claimed, ok, err := mediaRepo.TryClaimMediaJob(ctx, resumed.ID, now, now.Add(-time.Second), "claim_dead_video_worker")
	if err != nil || !ok {
		t.Fatalf("claim = %v %v", ok, err)
	}
	claimed.Checkpoint, claimed.Submitted = `{"request_id":"up-1"}`, true
	if err := mediaRepo.UpdateMediaJob(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	service.processMediaJob(ctx, resumed.ID)
	stored, err := mediaRepo.GetMediaJob(ctx, resumed.ID, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("%d:{\"request_id\":\"up-1\"}", accounts[1].ID); len(adapter.calls) != 1 || adapter.calls[0] != want {
		t.Fatalf("resume calls = %v, want %s", adapter.calls, want)
	}
	if stored.Status != media.StatusCompleted || stored.AccountID != accounts[1].ID || stored.Generation != 2 {
		t.Fatalf("resumed job = %#v", stored)
	}

	// The create may have reached the upstream without returning an ID.
	adapter.calls, adapter.fail = nil, provider.WrapVideoStage(provider.VideoStageSubmitted, 0, errors.New("create response lost"))
	unknown := newJob("video_checkpoint_unknown")
	service.processMediaJob(ctx, unknown.ID)
	stored, err = mediaRepo.GetMediaJob(ctx, unknown.ID, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != media.StatusUncertain || stored.ErrorCode != "submission_uncertain" || !stored.Submitted || len(adapter.calls) != 1 {
		t.Fatalf("unknown submission job = %#v calls = %v", stored, adapter.calls)
	}
}
