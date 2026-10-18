package gateway

import (
	"context"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
)

const (
	finalizationHealthBudget    = 750 * time.Millisecond
	finalizationOwnershipBudget = 1500 * time.Millisecond
	finalizationQuotaBudget     = time.Second
	finalizationAuditBudget     = 3 * time.Second
	finalizationMetadataBudget  = 500 * time.Millisecond
)

type finalizationBudget struct {
	deadline  time.Time
	operation string
	provider  string
}

func newFinalizationBudget(operation, provider string) finalizationBudget {
	return finalizationBudget{
		deadline:  time.Now().Add(finalizationTimeout),
		operation: operation,
		provider:  provider,
	}
}

func (b finalizationBudget) run(stage string, limit time.Duration, action func(context.Context) error) error {
	started := time.Now()
	deadline := b.deadline
	if candidate := started.Add(limit); candidate.Before(deadline) {
		deadline = candidate
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	err := action(ctx)
	cancel()
	outcome := "success"
	if err != nil {
		outcome = "failed"
	}
	perfmetrics.Default.ObserveDuration("finalization_stage_duration_us", perfmetrics.Labels{
		Subsystem: "gateway", Operation: b.operation, Provider: b.provider, Stage: stage, Outcome: outcome,
	}, time.Since(started))
	return err
}

// persistFailureAudit stamps a terminal failure record (duration, time, the
// failed account when known, egress) and writes it on a fresh context, so a
// canceled request still leaves its audit.
func (s *Service) persistFailureAudit(record audit.Record, startedAt time.Time, trace *infraegress.Trace, account *accountdomain.Credential) {
	record.DurationMS = time.Since(startedAt).Milliseconds()
	record.CreatedAt = time.Now().UTC()
	if account != nil && account.ID != 0 {
		accountID := account.ID
		record.AccountID = &accountID
		record.AccountName = account.Name
	}
	applyAuditEgress(&record, trace, accountdomain.Provider(record.Provider))
	persistCtx, cancel := context.WithTimeout(context.Background(), finalizationTimeout)
	defer cancel()
	if err := s.audits.Create(persistCtx, record); err != nil {
		s.logger.Error("request_usage_write_failed", "event_id", record.EventID, "request_id", record.RequestID, "error", err)
	}
}
