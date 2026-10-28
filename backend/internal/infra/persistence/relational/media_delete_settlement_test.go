package relational

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestMediaJobDeleteRequiresDurableSettlement(t *testing.T) {
	verifyMediaJobDeleteSettlement(t, openTestDatabase(t))
}

func verifyMediaJobDeleteSettlement(t *testing.T, database *Database) {
	t.Helper()
	ctx := context.Background()
	accountID, keyID, _ := mediaJobFixtureOwners(t, database, "delete-settlement")
	jobs := NewMediaJobRepository(database)
	for _, kind := range []media.Kind{media.KindImage, media.KindVideo} {
		for _, status := range []media.Status{media.StatusCompleted, media.StatusFailed, media.StatusUncertain, media.StatusCanceled} {
			t.Run(string(kind)+"/"+string(status), func(t *testing.T) {
				job := testImageJob("settlement_delete_"+string(kind)+"_"+string(status), accountID, keyID, status)
				job.Kind = kind
				if kind == media.KindVideo {
					job.Operation, job.Seconds = media.VideoOperationGenerate, 6
				}
				if err := jobs.CreateMediaJob(ctx, job); err != nil {
					t.Fatal(err)
				}
				err := jobs.DeleteMediaJob(ctx, job.ID)
				if kind == media.KindVideo && status == media.StatusCanceled {
					if err != nil {
						t.Fatalf("unsubmitted canceled video: %v", err)
					}
					return
				}
				if !errors.Is(err, repository.ErrConflict) {
					t.Fatalf("unsettled delete: %v", err)
				}
				if err := jobs.MarkMediaJobUsageRecorded(ctx, job.ID, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
				if err := jobs.DeleteMediaJob(ctx, job.ID); err != nil {
					t.Fatalf("settled delete: %v", err)
				}
				if err := jobs.DeleteMediaJob(ctx, job.ID); !errors.Is(err, repository.ErrNotFound) {
					t.Fatalf("missing job: %v", err)
				}
			})
		}
	}
	// A caller's old settled snapshot does not authorize deleting resumed work.
	now := time.Now().UTC()
	active := testImageJob("settlement_delete_resumed", accountID, keyID, media.StatusQueued)
	active.UsageRecordedAt = &now
	if err := jobs.CreateMediaJob(ctx, active); err != nil {
		t.Fatal(err)
	}
	if err := jobs.DeleteMediaJob(ctx, active.ID); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("delete active job with stale settlement marker: %v", err)
	}
}
