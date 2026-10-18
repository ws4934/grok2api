package relational

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"
	"time"
	"unicode/utf8"

	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// openPostgresTestDatabase opens the PostgreSQL integration database, or skips.
func openPostgresTestDatabase(t *testing.T) *Database {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not configured")
	}
	ctx := context.Background()
	database, err := OpenPostgres(ctx, dsn, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	return database
}

func TestMediaJobTerminalWriteSanitizesErrorText(t *testing.T) {
	verifyMediaJobTerminalWriteSanitizesErrorText(t, openTestDatabase(t))
}

func TestPostgresMediaJobTerminalWriteSanitizesErrorText(t *testing.T) {
	verifyMediaJobTerminalWriteSanitizesErrorText(t, openPostgresTestDatabase(t))
}

func verifyMediaJobTerminalWriteSanitizesErrorText(t *testing.T, database *Database) {
	ctx := context.Background()
	name := "utf8" + time.Now().UTC().Format("150405.000000")
	accountID, keyID, _ := mediaJobFixtureOwners(t, database, name)
	jobs := NewMediaJobRepository(database)
	job := testImageJob("resp_img_utf8_"+name, accountID, keyID, mediadomain.StatusQueued)
	if err := jobs.CreateMediaJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	claimed, ok, err := jobs.TryClaimMediaJob(ctx, job.ID, now, now.Add(time.Hour), "claim_utf8_trunc_000")
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	// An upstream message cut at a byte limit inside a rune, plus a NUL byte.
	claimed.Status, claimed.ErrorCode = mediadomain.StatusFailed, "generation_failed"
	claimed.ErrorMessage = "ChatGPT upstream HTTP 422: \x00抱歉\xe8\xaf"
	claimed.LeaseUntil, claimed.CompletedAt = nil, &now
	if err := jobs.UpdateMediaJob(ctx, claimed); err != nil {
		t.Fatalf("terminal write rejected: %v", err)
	}
	stored, err := jobs.GetMediaJob(ctx, job.ID, keyID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != mediadomain.StatusFailed || !utf8.ValidString(stored.ErrorMessage) || stored.ErrorMessage != "ChatGPT upstream HTTP 422: 抱歉�" {
		t.Fatalf("stored = %s %q", stored.Status, stored.ErrorMessage)
	}
}

func TestCanceledImageJobsAwaitUsageRecording(t *testing.T) {
	verifyCanceledImageJobsAwaitUsageRecording(t, openTestDatabase(t))
}

func TestPostgresCanceledImageJobsAwaitUsageRecording(t *testing.T) {
	verifyCanceledImageJobsAwaitUsageRecording(t, openPostgresTestDatabase(t))
}

// A canceled image job may have delivered images before the cancel; their
// usage is recorded like any other ended job. Canceled videos have none.
func verifyCanceledImageJobsAwaitUsageRecording(t *testing.T, database *Database) {
	ctx := context.Background()
	suffix := time.Now().UTC().Format("150405.000000")
	accountID, keyID, _ := mediaJobFixtureOwners(t, database, "cu"+suffix)
	jobs := NewMediaJobRepository(database)
	now := time.Now().UTC()
	image := testImageJob("resp_img_cancel_"+suffix, accountID, keyID, mediadomain.StatusCanceled)
	image.ResultAssetIDs, image.CompletedAt = []string{"asset_delivered_000001"}, &now
	video := mediadomain.Job{
		ID: "video_cancel_" + suffix, RequestID: "request-video-cancel", ClientKeyID: keyID, Provider: "grok_web", Model: "grok-imagine-video",
		ModelRouteID: 1, UpstreamModel: "video", Prompt: "test", Seconds: 8, Size: "16:9", Quality: "720p", Status: mediadomain.StatusCanceled,
		InputJSON: "{}", CreatedAt: now, UpdatedAt: now, CompletedAt: &now,
	}
	for _, job := range []mediadomain.Job{image, video} {
		if err := jobs.CreateMediaJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	pending := map[string]bool{}
	values, err := jobs.ListUnrecordedTerminalMediaJobs(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range values {
		pending[value.ID] = true
	}
	if !pending[image.ID] || pending[video.ID] {
		t.Fatalf("unrecorded = %v", pending)
	}
	if err := jobs.MarkMediaJobUsageRecorded(ctx, image.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := jobs.MarkMediaJobUsageRecorded(ctx, image.ID, now); err != nil {
		t.Fatalf("repeated mark = %v", err)
	}
	if err := jobs.MarkMediaJobUsageRecorded(ctx, video.ID, now); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("canceled video mark = %v", err)
	}
}

func TestCleanupExpiredBillingReservationsProtectsPendingImageJobs(t *testing.T) {
	verifyCleanupProtectsPendingImageJobs(t, openTestDatabase(t))
}

func TestPostgresCleanupExpiredBillingReservationsProtectsPendingImageJobs(t *testing.T) {
	verifyCleanupProtectsPendingImageJobs(t, openPostgresTestDatabase(t))
}

func verifyCleanupProtectsPendingImageJobs(t *testing.T, database *Database) {
	ctx := context.Background()
	suffix := time.Now().UTC().Format("150405.000000")
	accountID, _, _ := mediaJobFixtureOwners(t, database, "is"+suffix)
	keys := NewClientKeyRepository(database)
	key, err := keys.Create(ctx, clientkeydomain.Key{Name: "image-sweep", Prefix: "isk" + suffix, SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, BillingLimitUSDTicks: 1_000})
	if err != nil {
		t.Fatal(err)
	}
	jobs := NewMediaJobRepository(database)
	now := time.Now().UTC()
	queued := testImageJob("resp_img_q_"+suffix, accountID, key.ID, mediadomain.StatusQueued)
	resumed := testImageJob("resp_img_r_"+suffix, accountID, key.ID, mediadomain.StatusInProgress)
	recorded := testImageJob("resp_img_d_"+suffix, accountID, key.ID, mediadomain.StatusCompleted)
	for _, job := range []mediadomain.Job{queued, resumed, recorded} {
		if err := jobs.CreateMediaJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	if err := jobs.MarkMediaJobUsageRecorded(ctx, recorded.ID, now); err != nil {
		t.Fatal(err)
	}
	protected := []string{"image_usage_" + queued.ID + "_0", "image_usage_" + queued.ID + "_1", "image_usage_" + resumed.ID + "_0_r2"}
	released := []string{"image_usage_" + recorded.ID + "_0", "image_usage_resp_img_gone_" + suffix + "_0"}
	for _, eventID := range append(append([]string{}, protected...), released...) {
		if reserved, err := keys.ReserveBillingUsage(ctx, key.ID, eventID, 100, now.Add(-time.Minute)); err != nil || !reserved {
			t.Fatalf("reserve %s: reserved=%v err=%v", eventID, reserved, err)
		}
	}
	if _, err := keys.CleanupExpiredBillingReservations(ctx, now, 1000); err != nil {
		t.Fatal(err)
	}
	var remaining []string
	if err := database.db.WithContext(ctx).Model(&billingReservationModel{}).Where("client_key_id = ?", key.ID).Order("event_id ASC").Pluck("event_id", &remaining).Error; err != nil {
		t.Fatal(err)
	}
	slices.Sort(protected)
	if !slices.Equal(remaining, protected) {
		t.Fatalf("remaining = %v, want %v", remaining, protected)
	}
	if stored, err := keys.Get(ctx, key.ID); err != nil || stored.ReservedUsageUSDTicks != 300 {
		t.Fatalf("reserved = %d err = %v", stored.ReservedUsageUSDTicks, err)
	}
}
