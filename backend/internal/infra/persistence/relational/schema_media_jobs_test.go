package relational

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// mediaJobModelBeforeKinds is media_jobs as released before image jobs.
type mediaJobModelBeforeKinds struct {
	ID              string  `gorm:"size:64;primaryKey;check:chk_media_jobs_id,length(id) BETWEEN 1 AND 64"`
	RequestID       string  `gorm:"size:64;not null;check:chk_media_jobs_request_id,length(request_id) BETWEEN 1 AND 64"`
	ClientKeyID     uint64  `gorm:"not null;check:chk_media_jobs_client_key_id,client_key_id > 0"`
	ClientKeyName   string  `gorm:"size:160;not null;default:''"`
	ClientIP        string  `gorm:"size:45;not null;default:''"`
	AccountID       *uint64 `gorm:"check:chk_media_jobs_account_id,account_id IS NULL OR account_id > 0"`
	AccountName     string  `gorm:"size:160;not null;default:''"`
	EgressNodeID    *uint64
	EgressNodeName  string `gorm:"size:160;not null;default:''"`
	EgressScope     string `gorm:"size:32;not null;default:''"`
	EgressMode      string `gorm:"size:16;not null;default:'';check:chk_media_jobs_egress_mode,egress_mode IN ('','direct','proxy')"`
	Provider        string `gorm:"size:32;not null"`
	Model           string `gorm:"size:255;not null"`
	ModelRouteID    uint64 `gorm:"not null"`
	UpstreamModel   string `gorm:"size:255;not null"`
	Operation       string `gorm:"size:16;not null;default:'generate';check:chk_media_jobs_operation,operation IN ('generate','edit','extend')"`
	Prompt          string `gorm:"type:text;not null"`
	Seconds         int    `gorm:"not null;check:chk_media_jobs_seconds,seconds BETWEEN 0 AND 15"`
	Size            string `gorm:"size:32;not null;default:''"`
	Quality         string `gorm:"size:32;not null;default:''"`
	Status          string `gorm:"size:32;not null;check:chk_media_jobs_status,status IN ('queued','in_progress','completed','failed')"`
	Progress        int    `gorm:"not null"`
	InputJSON       string `gorm:"type:text;not null;default:'{}';check:chk_media_jobs_input_json,length(input_json) <= 33554432"`
	InputImageCount *int   `gorm:"check:chk_media_jobs_input_image_count,input_image_count IS NULL OR input_image_count BETWEEN 0 AND 8"`
	UpstreamURL     string `gorm:"type:text;not null;default:''"`
	ResultAssetID   string `gorm:"size:64;not null;default:''"`
	ContentType     string `gorm:"size:128;not null;default:''"`
	ErrorCode       string `gorm:"size:100;not null;default:''"`
	ErrorMessage    string `gorm:"size:512;not null;default:''"`
	LeaseUntil      *time.Time
	ClaimToken      string    `gorm:"size:64;not null;default:''"`
	CreatedAt       time.Time `gorm:"not null"`
	UpdatedAt       time.Time `gorm:"not null"`
	CompletedAt     *time.Time
	UsageRecordedAt *time.Time
	Account         *accountModel   `gorm:"foreignKey:AccountID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:SET NULL"`
	ClientKey       *clientKeyModel `gorm:"foreignKey:ClientKeyID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:RESTRICT"`
}

func (mediaJobModelBeforeKinds) TableName() string { return "media_jobs" }

func TestMediaJobKindUpgradeKeepsVideoRows(t *testing.T) {
	verifyMediaJobKindUpgrade(t, openTestDatabase(t))
}

func TestMediaJobLifecycleFencesClaimsAndCancels(t *testing.T) {
	verifyMediaJobLifecycle(t, openTestDatabase(t))
}

func TestPostgresMediaJobKindUpgradeAndLifecycle(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not configured")
	}
	ctx := context.Background()
	database, err := OpenPostgres(ctx, dsn, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	verifyMediaJobKindUpgrade(t, database)
	verifyMediaJobLifecycle(t, database)
}

func mediaJobFixtureOwners(t *testing.T, database *Database, name string) (uint64, uint64, uint64) {
	t.Helper()
	ctx := context.Background()
	accountValue, _, err := NewAccountRepository(database).UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderChatGPT, AuthType: accountdomain.AuthTypeOAuth, Name: name, SourceKey: name,
		EncryptedAccessToken: testEncryptedToken, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	var keys []uint64
	for _, suffix := range []string{"a", "b"} {
		key := clientKeyModel{Name: name + suffix, Prefix: name + suffix, SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 60, MaxConcurrent: 4}
		if err := database.db.WithContext(ctx).Create(&key).Error; err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key.ID)
	}
	return accountValue.ID, keys[0], keys[1]
}

func testImageJob(id string, accountID, keyID uint64, status mediadomain.Status) mediadomain.Job {
	now := time.Now().UTC()
	return mediadomain.Job{
		ID: id, Kind: mediadomain.KindImage, RequestID: "request-" + id, ClientKeyID: keyID, AccountID: accountID,
		Provider: string(accountdomain.ProviderChatGPT), Model: "gpt-image-2", ModelRouteID: 1, UpstreamModel: "chatgpt/gpt-image-2",
		Operation: mediadomain.VideoOperationEdit, Prompt: "edit", Size: "1024x1024", Status: status, ItemCount: 2,
		InputJSON: `{"image_urls":["data:image/png;base64,AAAA"]}`, InputImageCount: 1,
		Params: mediadomain.ImageParams{OutputFormat: "webp", ResponseModel: "gpt-5", ResponseBackground: true},
		CreatedAt: now, UpdatedAt: now,
	}
}

func verifyMediaJobKindUpgrade(t *testing.T, database *Database) {
	t.Helper()
	ctx := context.Background()
	accountID, keyID, otherKeyID := mediaJobFixtureOwners(t, database, "kind-upgrade")
	migrator := database.db.WithContext(ctx).Migrator()
	if err := migrator.DropTable("media_jobs"); err != nil {
		t.Fatal(err)
	}
	if err := migrator.CreateTable(&mediaJobModelBeforeKinds{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	completedAt := now.Add(time.Minute)
	images := 2
	legacy := mediaJobModelBeforeKinds{
		ID: "video_before_kinds", RequestID: "request-before-kinds", ClientKeyID: keyID, AccountID: &accountID,
		Provider: "grok_console", Model: "grok-imagine-video", ModelRouteID: 7, UpstreamModel: "Console/grok-imagine-video",
		Operation: "extend", Prompt: "keep me", Seconds: 10, Size: "16:9", Quality: "720p", Status: "completed", Progress: 100,
		InputJSON: `{"image_urls":["https://example.com/a.png","https://example.com/b.png"]}`, InputImageCount: &images,
		ResultAssetID: "vid_before_kinds_00000001", ContentType: "video/mp4", CreatedAt: now, UpdatedAt: now, CompletedAt: &completedAt,
	}
	if err := database.db.WithContext(ctx).Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := database.InitializeSchema(ctx); err != nil {
			t.Fatal(err)
		}
	}
	jobs := NewMediaJobRepository(database)
	stored, err := jobs.GetMediaJob(ctx, legacy.ID, keyID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Kind != mediadomain.KindVideo || stored.ItemCount != 1 || stored.Operation != mediadomain.VideoOperationExtend || stored.Seconds != 10 ||
		stored.Prompt != "keep me" || stored.Status != mediadomain.StatusCompleted || stored.ResultAssetID != legacy.ResultAssetID ||
		stored.InputImageCount != 2 || stored.Price != nil || stored.Generation != 0 || stored.Checkpoint != "{}" || len(stored.ResultAssetIDs) != 0 ||
		!stored.CompletedAt.Equal(completedAt) {
		t.Fatalf("upgraded video row = %#v", stored)
	}
	if input, err := jobs.GetMediaJobInput(ctx, legacy.ID); err != nil || input != legacy.InputJSON {
		t.Fatalf("upgraded input = %q err = %v", input, err)
	}
	for name, marker := range map[string]string{"chk_media_jobs_operation": "kind", "chk_media_jobs_seconds": "kind", "chk_media_jobs_status": "uncertain", "chk_media_jobs_kind": "image"} {
		definition, err := database.constraintDefinition(ctx, consoleConstraint{model: &mediaJobModel{}, table: "media_jobs", name: name})
		if err != nil || !strings.Contains(definition, marker) {
			t.Fatalf("%s = %q err = %v", name, definition, err)
		}
	}

	image := testImageJob("resp_img_upgrade_ok", accountID, keyID, mediadomain.StatusUncertain)
	image.IdempotencyKey, image.PayloadHash = "client-retry-1", strings.Repeat("a", 64)
	if err := jobs.CreateMediaJob(ctx, image); err != nil {
		t.Fatalf("image job rejected: %v", err)
	}
	duplicate := testImageJob("resp_img_upgrade_dup", accountID, keyID, mediadomain.StatusQueued)
	duplicate.IdempotencyKey = image.IdempotencyKey
	if err := jobs.CreateMediaJob(ctx, duplicate); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("duplicate idempotency key = %v", err)
	}
	otherKey := duplicate
	otherKey.ClientKeyID = otherKeyID
	if err := jobs.CreateMediaJob(ctx, otherKey); err != nil {
		t.Fatalf("same idempotency key on another client key: %v", err)
	}
	if found, err := jobs.GetMediaJobByIdempotencyKey(ctx, keyID, image.IdempotencyKey); err != nil || found.ID != image.ID || found.PayloadHash != image.PayloadHash || found.Params != image.Params {
		t.Fatalf("idempotent lookup = %#v err = %v", found, err)
	}
	extend := testImageJob("resp_img_upgrade_extend", accountID, keyID, mediadomain.StatusQueued)
	extend.Operation = mediadomain.VideoOperationExtend
	if err := jobs.CreateMediaJob(ctx, extend); err == nil {
		t.Fatal("image job accepted the video-only extend operation")
	}
	longVideo := testMediaJob("video_upgrade_long", accountID, keyID, mediadomain.StatusQueued, now)
	longVideo.Seconds = 20
	if err := jobs.CreateMediaJob(ctx, longVideo); err == nil {
		t.Fatal("video job accepted 20 seconds")
	}
}

func verifyMediaJobLifecycle(t *testing.T, database *Database) {
	t.Helper()
	ctx := context.Background()
	accountID, keyID, _ := mediaJobFixtureOwners(t, database, "lifecycle")
	jobs := NewMediaJobRepository(database)
	assets := NewMediaAssetRepository(database)
	job := testImageJob("resp_img_lifecycle_claim", accountID, keyID, mediadomain.StatusQueued)
	if err := jobs.CreateMediaJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	first, ok, err := jobs.TryClaimMediaJob(ctx, job.ID, now, now.Add(time.Hour), "claim_lifecycle_first")
	if err != nil || !ok || first.Generation != 1 {
		t.Fatalf("first claim = %#v ok=%v err=%v", first, ok, err)
	}
	if _, ok, err := jobs.TryClaimMediaJob(ctx, job.ID, now, now.Add(time.Hour), "claim_lifecycle_racing"); err != nil || ok {
		t.Fatalf("claim during a live lease = %v err = %v", ok, err)
	}
	// The first worker stalls past its lease; recovery claims the next generation.
	expired := now.Add(-time.Second)
	first.LeaseUntil = &expired
	first.ResultAssetIDs = []string{"img_lifecycle_result_0001"}
	if err := jobs.UpdateMediaJob(ctx, first); err != nil {
		t.Fatal(err)
	}
	second, ok, err := jobs.TryClaimMediaJob(ctx, job.ID, now, now.Add(time.Hour), "claim_lifecycle_second")
	if err != nil || !ok || second.Generation != 2 || len(second.ResultAssetIDs) != 1 {
		t.Fatalf("recovery claim = %#v ok=%v err=%v", second, ok, err)
	}
	first.Status = mediadomain.StatusCompleted
	if err := jobs.UpdateMediaJob(ctx, first); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("stale finish = %v", err)
	}
	if err := jobs.RenewMediaJobLease(ctx, first, now.Add(time.Hour)); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("stale renew = %v", err)
	}
	if err := jobs.RenewMediaJobLease(ctx, second, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	protected, err := assets.ListProtectedMediaAssetIDs(ctx)
	if _, ok := protected["img_lifecycle_result_0001"]; err != nil || !ok {
		t.Fatalf("running image result is not protected: %v err=%v", protected, err)
	}

	// Submitted work cannot be canceled; unsubmitted work can, and the claim is fenced out.
	second.Submitted = true
	if err := jobs.UpdateMediaJob(ctx, second); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.CancelMediaJob(ctx, job.ID, keyID, now); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("cancel submitted = %v", err)
	}
	second.Submitted = false
	if err := jobs.UpdateMediaJob(ctx, second); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.CancelMediaJob(ctx, job.ID, keyID+1000, now); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("cancel by another key = %v", err)
	}
	canceled, err := jobs.CancelMediaJob(ctx, job.ID, keyID, now)
	if err != nil || canceled.Status != mediadomain.StatusCanceled || canceled.ClaimToken != "" {
		t.Fatalf("cancel = %#v err = %v", canceled, err)
	}
	if err := jobs.UpdateMediaJob(ctx, second); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("write after cancel = %v", err)
	}
	if again, err := jobs.CancelMediaJob(ctx, job.ID, 0, now); err != nil || again.Status != mediadomain.StatusCanceled {
		t.Fatalf("admin cancel of a canceled job = %#v err = %v", again, err)
	}

	uncertain := testImageJob("resp_img_lifecycle_uncertain", accountID, keyID, mediadomain.StatusUncertain)
	failed := testImageJob("resp_img_lifecycle_failed", accountID, keyID, mediadomain.StatusFailed)
	for _, value := range []mediadomain.Job{uncertain, failed} {
		if err := jobs.CreateMediaJob(ctx, value); err != nil {
			t.Fatal(err)
		}
		if err := jobs.MarkMediaJobUsageRecorded(ctx, value.ID, now); err != nil {
			t.Fatal(err)
		}
	}
	if input, _ := jobs.GetMediaJobInput(ctx, uncertain.ID); input != uncertain.InputJSON {
		t.Fatalf("uncertain input compacted to %q", input)
	}
	if input, _ := jobs.GetMediaJobInput(ctx, failed.ID); input != "{}" {
		t.Fatalf("failed input kept: %q", input)
	}
	listed, total, err := jobs.ListMediaJobs(ctx, repository.MediaJobListQuery{
		Page:   repository.PageQuery{Limit: 10, Search: "resp_img_lifecycle"},
		Filter: repository.MediaJobListFilter{Kind: string(mediadomain.KindImage), Provider: string(accountdomain.ProviderChatGPT), Status: string(mediadomain.StatusUncertain)},
	})
	if err != nil || total != 1 || listed[0].ID != uncertain.ID || listed[0].ItemCount != 2 || listed[0].Kind != mediadomain.KindImage {
		t.Fatalf("filtered list = %#v total=%d err=%v", listed, total, err)
	}
	stats, err := jobs.SummarizeMediaJobs(ctx, string(mediadomain.KindImage))
	if err != nil || stats.Canceled < 1 || stats.Uncertain < 1 || stats.Failed < 1 {
		t.Fatalf("image stats = %#v err = %v", stats, err)
	}
	if err := jobs.DeleteMediaJob(ctx, canceled.ID); err != nil {
		t.Fatalf("delete canceled job: %v", err)
	}
}
