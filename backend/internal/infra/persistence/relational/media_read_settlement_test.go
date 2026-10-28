package relational

import (
	"context"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestMediaJobReadsPreserveSettlementState(t *testing.T) {
	verifyMediaJobReadSettlement(t, openTestDatabase(t))
}

func verifyMediaJobReadSettlement(t *testing.T, database *Database) {
	t.Helper()
	ctx := context.Background()
	accountID, keyID, _ := mediaJobFixtureOwners(t, database, "read-settlement")
	jobs := NewMediaJobRepository(database)
	for _, kind := range []media.Kind{media.KindImage, media.KindVideo} {
		for _, status := range []media.Status{media.StatusCompleted, media.StatusFailed, media.StatusUncertain, media.StatusCanceled} {
			t.Run(string(kind)+"/"+string(status), func(t *testing.T) {
				job := testImageJob("settlement_read_"+string(kind)+"_"+string(status), accountID, keyID, status)
				job.Kind = kind
				if kind == media.KindVideo {
					job.Operation, job.Seconds = media.VideoOperationGenerate, 6
				}
				if err := jobs.CreateMediaJob(ctx, job); err != nil {
					t.Fatal(err)
				}
				assertReads := func(recordedAt *time.Time, pending bool) {
					t.Helper()
					single, err := jobs.GetMediaJob(ctx, job.ID, keyID)
					if err != nil {
						t.Fatal(err)
					}
					batch, err := jobs.GetMediaJobsByIDs(ctx, []string{job.ID})
					if err != nil || len(batch) != 1 {
						t.Fatalf("batch read: %+v err=%v", batch, err)
					}
					listed, total, err := jobs.ListMediaJobs(ctx, repository.MediaJobListQuery{
						Page: repository.PageQuery{Search: job.ID, Limit: 10},
					})
					if err != nil || total != 1 || len(listed) != 1 {
						t.Fatalf("list read: %+v total=%d err=%v", listed, total, err)
					}
					for name, stored := range map[string]media.Job{"single": single, "batch": batch[0], "list": listed[0]} {
						if stored.UsagePending() != pending {
							t.Errorf("%s pending=%v want=%v (recordedAt=%v)", name, stored.UsagePending(), pending, stored.UsageRecordedAt)
						}
						if (recordedAt == nil) != (stored.UsageRecordedAt == nil) ||
							(recordedAt != nil && stored.UsageRecordedAt != nil && !stored.UsageRecordedAt.Equal(*recordedAt)) {
							t.Errorf("%s recordedAt=%v want=%v", name, stored.UsageRecordedAt, recordedAt)
						}
					}
				}
				pending := status != media.StatusCanceled || kind == media.KindImage
				assertReads(nil, pending)
				if !pending {
					return // An unsubmitted canceled video needs no usage settlement.
				}
				recordedAt := time.Now().UTC().Truncate(time.Microsecond)
				if err := jobs.MarkMediaJobUsageRecorded(ctx, job.ID, recordedAt); err != nil {
					t.Fatal(err)
				}
				assertReads(&recordedAt, false)
			})
		}
	}
}
