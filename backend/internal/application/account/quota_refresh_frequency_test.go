package account

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// lossyQuotaCoordinator loses the dirty marker right after it is published,
// as a Redis restart, an eviction or the marker TTL would.
type lossyQuotaCoordinator struct {
	*memory.QuotaRefreshCoordinator
	lost atomic.Bool
}

func (c *lossyQuotaCoordinator) MarkQuotaRefreshDirty(ctx context.Context, id uint64, mode string, ttl time.Duration) (uint64, error) {
	generation, err := c.QuotaRefreshCoordinator.MarkQuotaRefreshDirty(ctx, id, mode, ttl)
	c.lost.Store(true)
	return generation, err
}

func (c *lossyQuotaCoordinator) QuotaRefreshGeneration(ctx context.Context, id uint64, mode string) (uint64, bool, error) {
	if c.lost.Load() {
		return 0, false, nil
	}
	return c.QuotaRefreshCoordinator.QuotaRefreshGeneration(ctx, id, mode)
}

func (c *lossyQuotaCoordinator) ClearQuotaRefreshDirty(ctx context.Context, id uint64, mode string, generation uint64) (bool, error) {
	if c.lost.Load() {
		return false, nil
	}
	return c.QuotaRefreshCoordinator.ClearQuotaRefreshDirty(ctx, id, mode, generation)
}

var _ repository.QuotaRefreshCoordinator = (*lossyQuotaCoordinator)(nil)

// A refresh whose shared marker vanished must end, not sync back to back.
func TestLostQuotaRefreshMarkerDoesNotLoop(t *testing.T) {
	accounts, credential := quotaFrequencyWebAccount(t)
	adapter := &quotaCountingAdapter{}
	service := NewService(accounts, nil, nil, nil, provider.NewRegistry(adapter), nil, nil)
	service.SetQuotaRefreshCoordinator(&lossyQuotaCoordinator{QuotaRefreshCoordinator: memory.NewQuotaRefreshCoordinator()})
	service.QueueQuotaRefresh(credential.ID, "weekly")
	request := <-service.quotaRefresh.requests
	service.quotaRefresh.mu.Lock()
	state := service.quotaRefresh.states[request.key]
	state.queued, state.running = false, true
	service.quotaRefresh.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { service.quotaRefresh.refresh(ctx, request); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		<-done
	}
	if calls := adapter.modeCalls.Load(); calls > 1 {
		t.Fatalf("one queued refresh made %d upstream syncs", calls)
	}
}

func quotaFrequencyDatabase(t *testing.T) *relational.AccountRepository {
	t.Helper()
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "quota.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	return relational.NewAccountRepository(database)
}

func quotaFrequencyWebAccount(t *testing.T) (*relational.AccountRepository, accountdomain.Credential) {
	t.Helper()
	accounts := quotaFrequencyDatabase(t)
	credential, _, err := accounts.UpsertByIdentity(context.Background(), accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, Name: "web", SourceKey: "web",
		EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: accountdomain.AuthStatusActive, WebTier: accountdomain.WebTierSuper,
	})
	if err != nil {
		t.Fatal(err)
	}
	return accounts, credential
}

// After a failed renewal, a 401 on the account must not send the refresh
// token again before the scheduled retry; a manual forced renewal still does.
func TestForcedRefreshWaitsOutAFailedRenewal(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	service, credential, adapter := newCredentialRefreshTestService(t, now)
	adapter.refreshErr = &provider.CredentialRefreshError{Status: 502, Code: "server_error", Message: "upstream unavailable"}
	var deferred *CredentialRefreshDeferredError
	for attempt := range 20 {
		latest, err := service.accounts.Get(ctx, credential.ID)
		if err != nil {
			t.Fatal(err)
		}
		_, err = service.RenewRejectedCredential(ctx, latest)
		if attempt > 0 && !errors.As(err, &deferred) {
			t.Fatalf("attempt %d: err = %v, want a deferred renewal", attempt, err)
		}
	}
	if calls := adapter.refreshCount.Load(); calls != 1 {
		t.Fatalf("20 renewals after 401s made %d OAuth calls", calls)
	}
	latest, _ := service.accounts.Get(ctx, credential.ID)
	service.renewer.forget(credential.ID)
	if _, err := service.EnsureCredential(ctx, latest, true); errors.As(err, &deferred) || adapter.refreshCount.Load() != 2 {
		t.Fatalf("manual forced renewal = %v, calls %d", err, adapter.refreshCount.Load())
	}
}

// snapshotThrottleAdapter syncs like ChatGPT: one call returns every window.
type snapshotThrottleAdapter struct {
	calls   atomic.Int64
	display []string
}

func (a *snapshotThrottleAdapter) Provider() accountdomain.Provider {
	return accountdomain.ProviderChatGPT
}
func (a *snapshotThrottleAdapter) Definition() provider.Definition {
	return provider.Definition{
		Provider: accountdomain.ProviderChatGPT, ModelNamespace: accountdomain.ProviderChatGPT.ModelNamespace(),
		Quota: provider.QuotaRemoteWindow, Credential: provider.CredentialSurface{AuthType: accountdomain.AuthTypeOAuth},
		Profile: provider.ProfileSurface{QuotaModes: []string{"reason", "image_gen", "file_upload", "codex_5h", "codex_weekly", "codex"}, DisplayQuotaModes: a.display, QuotaStaleAfter: 6 * time.Hour},
	}
}
func (a *snapshotThrottleAdapter) QuotaModeSyncReadsSnapshot() {}
func (a *snapshotThrottleAdapter) SyncQuota(_ context.Context, c accountdomain.Credential) (provider.QuotaSnapshot, error) {
	a.calls.Add(1)
	now := time.Now().UTC()
	return provider.QuotaSnapshot{SyncedAt: now, Windows: []accountdomain.QuotaWindow{
		{AccountID: c.ID, Mode: "image_gen", Remaining: 90, SyncedAt: &now, Source: accountdomain.QuotaSourceUpstream, UpdatedAt: now},
	}}, nil
}
func (a *snapshotThrottleAdapter) SyncQuotaMode(context.Context, accountdomain.Credential, string) (accountdomain.QuotaWindow, error) {
	return accountdomain.QuotaWindow{}, nil
}

// Request-driven refreshes of a snapshot provider share one sync per account
// and interval, whatever mode each request consumed.
func TestSnapshotQuotaRefreshesAreThrottledPerAccount(t *testing.T) {
	accounts := quotaFrequencyDatabase(t)
	credential, _, err := accounts.UpsertByIdentity(context.Background(), accountdomain.Credential{
		Provider: accountdomain.ProviderChatGPT, AuthType: accountdomain.AuthTypeOAuth, Name: "plus", SourceKey: "plus",
		EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &snapshotThrottleAdapter{}
	service := NewService(accounts, nil, nil, nil, provider.NewRegistry(adapter), nil, memory.NewLockStore())
	service.SetQuotaRefreshCoordinator(memory.NewQuotaRefreshCoordinator())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go service.RunQuotaRefresh(ctx)
	settle := func() {
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) && service.QuotaRefreshStats().Running > 0 {
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
	}
	for range 10 {
		service.QueueQuotaRefresh(credential.ID, "reason")
		settle()
	}
	for _, mode := range []string{"reason", "image_gen", "codex"} {
		service.QueueQuotaRefresh(credential.ID, mode)
	}
	settle()
	if calls := adapter.calls.Load(); calls != 1 {
		t.Fatalf("13 request-driven refreshes made %d upstream snapshots", calls)
	}
}

// A 429 on a mode the account has no window for cannot be stored as an
// exhausted quota; the caller must cool the account down instead.
func TestRateLimitWithoutAWindowIsInconclusive(t *testing.T) {
	ctx := context.Background()
	accounts := quotaFrequencyDatabase(t)
	credential, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderChatGPT, AuthType: accountdomain.AuthTypeOAuth, Name: "plus429", SourceKey: "plus429",
		EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := accounts.ReplaceQuotaWindows(ctx, credential.ID, "", now, []accountdomain.QuotaWindow{{AccountID: credential.ID, Mode: "codex", Remaining: 90, Total: 100, SyncedAt: &now, Source: accountdomain.QuotaSourceUpstream, UpdatedAt: now}}); err != nil {
		t.Fatal(err)
	}
	service := NewService(accounts, nil, nil, nil, provider.NewRegistry(&snapshotThrottleAdapter{}), nil, nil)
	if state, err := service.ReconcileRateLimit(ctx, credential.ID, "reason", time.Minute); err != nil || state != RateLimitReconcileInconclusive {
		t.Fatalf("429 without a window = %s, %v", state, err)
	}
	if state, err := service.ReconcileRateLimit(ctx, credential.ID, "codex", time.Minute); err != nil || state != RateLimitReconcileExhausted {
		t.Fatalf("429 on the codex window = %s, %v", state, err)
	}
}

// A queued refresh of an account the gateway cannot route to is dropped
// instead of being retried.
func TestQueuedRefreshDropsUnroutableAccounts(t *testing.T) {
	accounts, credential := quotaFrequencyWebAccount(t)
	credential.Enabled = false
	if _, err := accounts.Update(context.Background(), credential); err != nil {
		t.Fatal(err)
	}
	adapter := &quotaCountingAdapter{}
	service := NewService(accounts, nil, nil, nil, provider.NewRegistry(adapter), nil, nil)
	service.QueueQuotaRefresh(credential.ID, "weekly")
	request := <-service.quotaRefresh.requests
	service.quotaRefresh.refresh(context.Background(), request)
	service.quotaRefresh.mu.Lock()
	state := service.quotaRefresh.states[request.key]
	service.quotaRefresh.mu.Unlock()
	if state != nil || adapter.modeCalls.Load() != 0 {
		t.Fatalf("disabled account: state = %#v, syncs = %d", state, adapter.modeCalls.Load())
	}
}

// Display-only windows (ChatGPT's Codex 5-hour and weekly parts) get no
// recovery probe of their own; the routing window probes for them.
func TestDisplayQuotaModesDoNotControlRouting(t *testing.T) {
	adapter := &snapshotThrottleAdapter{display: []string{"codex_5h", "codex_weekly"}}
	service := NewService(nil, nil, nil, nil, provider.NewRegistry(adapter), nil, nil)
	if service.quotaWindowControlsRouting(accountdomain.ProviderChatGPT, "codex_5h") || !service.quotaWindowControlsRouting(accountdomain.ProviderChatGPT, "codex") {
		t.Fatal("display-only modes must not schedule recovery; routing modes must")
	}
}
