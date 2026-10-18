package relational

import (
	"context"
	"os"
	"testing"
	"time"
	"unicode/utf8"

	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
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
