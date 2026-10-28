package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type unavailableSettlementAudit struct{ repository.AuditRepository }

func (*unavailableSettlementAudit) Create(context.Context, audit.Record) error {
	return errors.New("audit storage temporarily unavailable")
}
func (*unavailableSettlementAudit) CreateBatch(context.Context, []audit.Record) error {
	return errors.New("audit storage temporarily unavailable")
}

func TestPendingImageSettlementSurvivesAdminDelete(t *testing.T) {
	f := newImageJobFixture(t, 1)
	ctx := context.Background()
	f.service.audits = &unavailableSettlementAudit{AuditRepository: f.audits}
	job := f.create(t, ImageJobInput{RequestID: "unsettled-delete"})
	f.service.processMediaJob(ctx, job.ID)
	stored := f.job(t, job.ID)
	if stored.Status != media.StatusCompleted || stored.ResultAssetID == "" || !stored.UsagePending() {
		t.Fatalf("not pending settlement: %+v", stored)
	}
	if billed, reserved := f.billing(t); billed != 0 || reserved != 400_000_000 {
		t.Fatalf("before recovery: billed=%d reserved=%d", billed, reserved)
	}
	// The repository must protect settlement even when callers bypass the service
	// or read a stale terminal snapshot before a concurrent resume.
	if err := f.jobs.DeleteMediaJob(ctx, job.ID); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("direct delete: %v", err)
	}
	admin := mediaapp.NewService(f.assets, f.jobs, nil, nil, mediaapp.Config{MaxTotalBytes: 1 << 30})
	if deleted, err := admin.AdminDeleteJobs(ctx, []string{job.ID}); deleted != 0 || !errors.Is(err, mediaapp.ErrPendingJobUsage) {
		t.Fatalf("admin delete: deleted=%d err=%v", deleted, err)
	}
	if pending, err := f.jobs.ListUnrecordedTerminalMediaJobs(ctx, 200); err != nil || len(pending) != 1 || pending[0].ID != job.ID {
		t.Fatalf("pending settlement: %v err=%v", pending, err)
	}
	f.service.audits = f.audits
	if err := f.service.reconcileMediaUsage(ctx); err != nil {
		t.Fatal(err)
	}
	if stored := f.job(t, job.ID); stored.UsagePending() || stored.UsageRecordedAt == nil {
		t.Fatalf("settlement marker missing: %+v", stored)
	}
	if billed, reserved := f.billing(t); billed != 400_000_000 || reserved != 0 {
		t.Fatalf("after recovery: billed=%d reserved=%d", billed, reserved)
	}
	if deleted, err := admin.AdminDeleteJobs(ctx, []string{job.ID}); deleted != 1 || err != nil {
		t.Fatalf("settled delete: deleted=%d err=%v", deleted, err)
	}
	if _, err := f.keys.CleanupExpiredBillingReservations(ctx, time.Now().UTC().Add(25*time.Hour), 200); err != nil {
		t.Fatal(err)
	}
	if billed, reserved := f.billing(t); billed != 400_000_000 || reserved != 0 {
		t.Fatalf("after reservation sweep: billed=%d reserved=%d", billed, reserved)
	}
	if records, _, err := f.audits.List(ctx, 0, 20); err != nil || len(records) != 1 {
		t.Fatalf("settled audit: %v err=%v", records, err)
	}
}
