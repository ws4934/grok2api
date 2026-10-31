package account

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestQuotaRefreshDropsDeletedAccountAndSharedMarker(t *testing.T) {
	for _, shared := range []bool{false, true} {
		for _, duringSync := range []bool{false, true} {
			t.Run(fmt.Sprintf("shared=%t/duringSync=%t", shared, duringSync), func(t *testing.T) {
				ctx := context.Background()
				accounts, credential := quotaDiscardConsoleAccount(t)
				service := NewService(accounts, nil, nil, nil, nil, nil, nil)
				var logs bytes.Buffer
				service.SetLogger(slog.New(slog.NewJSONHandler(&logs, nil)))
				if shared {
					service.SetQuotaRefreshCoordinator(memory.NewQuotaRefreshCoordinator())
				}
				deleteAccount := func() {
					t.Helper()
					if _, err := accounts.DeleteManyWithLinked(ctx, "", []uint64{credential.ID}, nil, false); err != nil {
						t.Fatal(err)
					}
				}
				if !duringSync {
					deleteAccount()
				}
				service.quotaRefresh.syncMode = func(ctx context.Context, id uint64, mode string) error {
					if !duringSync {
						t.Fatal("deleted account reached upstream sync")
					}
					deleteAccount()
					// The service maps the second lookup's repository.ErrNotFound
					// to ErrNotFound after the queue's preflight lookup succeeded.
					return service.syncQueuedQuota(ctx, id, mode)
				}
				service.QueueQuotaRefresh(credential.ID, "console")
				request := takeQuotaRefreshRequest(t, &service.quotaRefresh)
				service.quotaRefresh.refresh(ctx, request)
				assertQuotaRefreshDiscarded(t, &service.quotaRefresh)
				if logs.Len() != 0 {
					t.Fatalf("deleted account emitted operational logs: %s", logs.String())
				}
			})
		}
	}
}

func TestQuotaRefreshDropsWrappedRepositoryNotFound(t *testing.T) {
	accounts, credential := quotaDiscardConsoleAccount(t)
	service := NewService(accounts, nil, nil, nil, nil, nil, nil)
	service.quotaRefresh.syncMode = func(context.Context, uint64, string) error {
		return fmt.Errorf("save quota snapshot: %w", repository.ErrNotFound)
	}
	service.QueueQuotaRefresh(credential.ID, "console")
	service.quotaRefresh.refresh(context.Background(), takeQuotaRefreshRequest(t, &service.quotaRefresh))
	assertQuotaRefreshDiscarded(t, &service.quotaRefresh)
}

func TestQuotaRefreshTransientFailureStillRetries(t *testing.T) {
	accounts, credential := quotaDiscardConsoleAccount(t)
	service := NewService(accounts, nil, nil, nil, nil, nil, nil)
	service.SetQuotaRefreshCoordinator(memory.NewQuotaRefreshCoordinator())
	var logs bytes.Buffer
	service.SetLogger(slog.New(slog.NewJSONHandler(&logs, nil)))
	service.quotaRefresh.syncMode = func(context.Context, uint64, string) error {
		return errors.New("temporary upstream failure")
	}
	service.QueueQuotaRefresh(credential.ID, "console")
	request := takeQuotaRefreshRequest(t, &service.quotaRefresh)
	service.quotaRefresh.refresh(context.Background(), request)
	state := service.quotaRefresh.states[request.key]
	if state == nil || state.running || !state.pending || state.failures != 1 || !state.nextAttemptAt.After(service.now()) {
		t.Fatalf("transient failure lost backoff: %+v", state)
	}
	if _, dirty, err := service.quotaRefresh.coordinator.QuotaRefreshGeneration(context.Background(), credential.ID, "console"); err != nil || !dirty {
		t.Fatalf("transient failure lost shared signal: dirty=%t err=%v", dirty, err)
	}
	service.quotaRefresh.requeue()
	if len(service.quotaRefresh.requests) != 0 || !bytes.Contains(logs.Bytes(), []byte("quota_refresh_failed")) {
		t.Fatalf("retry was immediate or warning missing: %s", logs.String())
	}
}

type quotaDiscardCoordinator struct {
	repository.QuotaRefreshCoordinator
	beforeClear func()
	clearErr    error
}

func (c *quotaDiscardCoordinator) ClearQuotaRefreshDirty(ctx context.Context, id uint64, mode string, generation uint64) (bool, error) {
	if c.beforeClear != nil {
		beforeClear := c.beforeClear
		c.beforeClear = nil
		beforeClear()
	}
	if c.clearErr != nil {
		return false, c.clearErr
	}
	return c.QuotaRefreshCoordinator.ClearQuotaRefreshDirty(ctx, id, mode, generation)
}

func TestQuotaRefreshDiscardPreservesNewGeneration(t *testing.T) {
	ctx := context.Background()
	service := NewService(quotaFrequencyDatabase(t), nil, nil, nil, nil, nil, nil)
	coordinator := &quotaDiscardCoordinator{QuotaRefreshCoordinator: memory.NewQuotaRefreshCoordinator()}
	service.SetQuotaRefreshCoordinator(coordinator)
	const missingID = 9_999_999
	coordinator.beforeClear = func() {
		// A concurrent local request and another replica's dirty signal must
		// survive cleanup of the older generation.
		service.QueueQuotaRefresh(missingID, "console")
		if _, err := coordinator.MarkQuotaRefreshDirty(ctx, missingID, "console", quotaRefreshDirtyTTL); err != nil {
			t.Fatal(err)
		}
	}
	service.QueueQuotaRefresh(missingID, "console")
	request := takeQuotaRefreshRequest(t, &service.quotaRefresh)
	service.quotaRefresh.refresh(ctx, request)
	state := service.quotaRefresh.states[request.key]
	if state == nil || state.running || !state.pending || state.generation != 2 {
		t.Fatalf("new local generation was lost or stuck running: %+v", state)
	}
	if generation, dirty, err := coordinator.QuotaRefreshGeneration(ctx, missingID, "console"); err != nil || !dirty || generation != 2 {
		t.Fatalf("new shared generation was cleared: generation=%d dirty=%t err=%v", generation, dirty, err)
	}
	service.quotaRefresh.requeue()
	service.quotaRefresh.refresh(ctx, takeQuotaRefreshRequest(t, &service.quotaRefresh))
	assertQuotaRefreshDiscarded(t, &service.quotaRefresh)
}

func TestQuotaRefreshDiscardCleanupFailureUsesBackoff(t *testing.T) {
	ctx := context.Background()
	service := NewService(quotaFrequencyDatabase(t), nil, nil, nil, nil, nil, nil)
	coordinator := &quotaDiscardCoordinator{QuotaRefreshCoordinator: memory.NewQuotaRefreshCoordinator(), clearErr: errors.New("coordinator unavailable")}
	service.SetQuotaRefreshCoordinator(coordinator)
	service.QueueQuotaRefresh(9_999_999, "console")
	request := takeQuotaRefreshRequest(t, &service.quotaRefresh)
	service.quotaRefresh.refresh(ctx, request)
	state := service.quotaRefresh.states[request.key]
	if state == nil || state.running || !state.pending || state.failures != 1 || !state.nextAttemptAt.After(service.now()) {
		t.Fatalf("failed shared cleanup did not back off: %+v", state)
	}
	service.quotaRefresh.requeue()
	if len(service.quotaRefresh.requests) != 0 {
		t.Fatal("cleanup failure retried immediately")
	}
	coordinator.clearErr = nil
	now := state.nextAttemptAt.Add(time.Millisecond)
	service.now = func() time.Time { return now }
	service.quotaRefresh.requeue()
	service.quotaRefresh.refresh(ctx, takeQuotaRefreshRequest(t, &service.quotaRefresh))
	assertQuotaRefreshDiscarded(t, &service.quotaRefresh)
}

func takeQuotaRefreshRequest(t *testing.T, queue *quotaRefreshQueue) quotaRefreshRequest {
	t.Helper()
	var request quotaRefreshRequest
	select {
	case request = <-queue.requests:
	default:
		t.Fatal("no quota refresh queued")
	}
	queue.mu.Lock()
	state := queue.states[request.key]
	if state != nil {
		state.queued, state.running = false, true
	}
	queue.mu.Unlock()
	if state == nil {
		t.Fatal("queued refresh has no state")
	}
	return request
}

func assertQuotaRefreshDiscarded(t *testing.T, queue *quotaRefreshQueue) {
	t.Helper()
	queue.recoverShared(context.Background(), queue.now())
	queue.requeue()
	if stats := queue.stats(); stats != (QuotaRefreshStats{}) || len(queue.states) != 0 || len(queue.requests) != 0 {
		t.Fatalf("discarded refresh recovered: stats=%+v states=%+v queued=%d", stats, queue.states, len(queue.requests))
	}
}

func quotaDiscardConsoleAccount(t *testing.T) (*relational.AccountRepository, accountdomain.Credential) {
	t.Helper()
	accounts := quotaFrequencyDatabase(t)
	credential, _, err := accounts.UpsertByIdentity(context.Background(), accountdomain.Credential{
		Provider: accountdomain.ProviderConsole, AuthType: accountdomain.AuthTypeSSO, Name: "console", SourceKey: "console",
		EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	return accounts, credential
}

func TestQuotaRefreshDiscardInactiveAccountClearsSharedMarker(t *testing.T) {
	for _, authStatus := range []accountdomain.AuthStatus{accountdomain.AuthStatusActive, accountdomain.AuthStatusReauthRequired} {
		t.Run(string(authStatus), func(t *testing.T) {
			accounts, credential := quotaDiscardConsoleAccount(t)
			if authStatus == accountdomain.AuthStatusReauthRequired {
				if err := accounts.MarkReauthRequired(context.Background(), credential.ID, "test rejected credential"); err != nil {
					t.Fatal(err)
				}
			} else {
				credential.Enabled = false
				if _, err := accounts.Update(context.Background(), credential); err != nil {
					t.Fatal(err)
				}
			}
			service := NewService(accounts, nil, nil, nil, nil, nil, nil)
			service.SetQuotaRefreshCoordinator(memory.NewQuotaRefreshCoordinator())
			service.quotaRefresh.syncMode = func(context.Context, uint64, string) error {
				t.Fatal("inactive account reached upstream sync")
				return nil
			}
			service.QueueQuotaRefresh(credential.ID, "console")
			service.quotaRefresh.refresh(context.Background(), takeQuotaRefreshRequest(t, &service.quotaRefresh))
			assertQuotaRefreshDiscarded(t, &service.quotaRefresh)
		})
	}
}
