package media

import (
	"testing"
	"time"
)

func TestJobUsagePending(t *testing.T) {
	now := time.Now().UTC()
	for _, kind := range []Kind{KindImage, KindVideo} {
		for _, status := range []Status{StatusQueued, StatusInProgress, StatusCompleted, StatusFailed, StatusCanceled, StatusUncertain} {
			t.Run(string(kind)+"/"+string(status), func(t *testing.T) {
				job := Job{Kind: kind, Status: status}
				want := status.Terminal() && !(kind == KindVideo && status == StatusCanceled)
				if job.UsagePending() != want {
					t.Fatalf("pending=%v want=%v", job.UsagePending(), want)
				}
				job.UsageRecordedAt = &now
				if job.UsagePending() {
					t.Fatal("settled job still pending")
				}
			})
		}
	}
}
