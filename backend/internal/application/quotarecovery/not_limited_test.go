package quotarecovery

import (
	"context"
	"fmt"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestRunOneAcksModeTheSnapshotNoLongerLimits(t *testing.T) {
	now := time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)
	queue := &quotaQueueStub{}
	service := NewService(testLogger(), queue, notLimitedSyncStub{}, 30*time.Second, 30*time.Minute)

	service.runOne(context.Background(), now, accountdomain.QuotaRecoveryEvent{AccountID: 9, Mode: "file_upload", DueAt: now, ClaimToken: "claim"})

	if queue.acked != 1 || len(queue.rescheduled) != 0 {
		t.Fatalf("acked = %d, rescheduled = %#v", queue.acked, queue.rescheduled)
	}
}

// notLimitedSyncStub probes like a complete quota snapshot that omits the mode.
type notLimitedSyncStub struct{}

func (notLimitedSyncStub) ProbeQuotaMode(_ context.Context, _ uint64, mode string) (accountdomain.QuotaWindow, error) {
	return accountdomain.QuotaWindow{}, fmt.Errorf("%w: %s", accountdomain.ErrQuotaNotLimited, mode)
}

func (notLimitedSyncStub) ListDueQuotaWindows(context.Context, time.Time, int) ([]accountdomain.QuotaWindow, error) {
	return nil, nil
}
