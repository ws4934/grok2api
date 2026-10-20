package gateway

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// observedSelector wires the selector as application.go does: repository writes
// notify it synchronously.
func observedSelector(t *testing.T) (*Selector, *relational.AccountRepository) {
	t.Helper()
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "selector.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
	accounts.SetInvalidationObserver(func(_ context.Context, event repository.InvalidationEvent) {
		selector.ApplyInvalidation(event)
	})
	return selector, accounts
}

func observedAccount(t *testing.T, accounts *relational.AccountRepository, name string) account.Credential {
	t.Helper()
	value, _, err := accounts.UpsertByIdentity(context.Background(), account.Credential{
		Provider: account.ProviderBuild, Name: name, SourceKey: name, EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 10, MaxConcurrent: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func acquireOnce(t *testing.T, selector *Selector, model string) (account.Credential, error) {
	t.Helper()
	lease, err := selector.Acquire(context.Background(), account.ProviderBuild, 0, model, "", "", nil, false)
	if err != nil {
		return account.Credential{}, err
	}
	lease.Release()
	return lease.Credential, nil
}

// A video job's success (MarkSuccess) must not drop its account from the pool.
func TestMarkSuccessKeepsTheAccountSelectable(t *testing.T) {
	selector, accounts := observedSelector(t)
	observedAccount(t, accounts, "only")
	credential, err := acquireOnce(t, selector, "grok-test")
	if err != nil {
		t.Fatal(err)
	}
	selector.markSuccess(context.Background(), credential, true)
	if _, err := acquireOnce(t, selector, "grok-test"); err != nil {
		t.Fatalf("after MarkSuccess: %v", err)
	}
}

// A model denial blocks that model only; other models keep the account. (The
// removed eviction raced with concurrent reloads, which a sequential test
// cannot reproduce; this pins the intended outcome.)
func TestModelAccessDeniedKeepsOtherModels(t *testing.T) {
	selector, accounts := observedSelector(t)
	only := observedAccount(t, accounts, "only")
	if err := selector.MarkModelAccessDenied(context.Background(), only, "grok-chat", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireOnce(t, selector, "grok-video"); err != nil {
		t.Fatalf("chat-only denial removed the account from video: %v", err)
	}
}

// Overlay events (a model block, the periodic model-cooldown prune) must not
// release an account that is still in a hard cooldown.
func TestOverlayEventsKeepHealthCooldowns(t *testing.T) {
	ctx := context.Background()
	selector, accounts := observedSelector(t)
	cooled := observedAccount(t, accounts, "cooled")
	other := observedAccount(t, accounts, "other")
	selector.UpdateConfig(time.Hour, time.Minute, time.Hour)
	// Load the base snapshot first, so the cooldown lives only in the health override.
	if _, err := selector.beginSelectionSession(ctx, account.ProviderBuild, 0, "grok-test", "", "", nil, false); err != nil {
		t.Fatal(err)
	}
	selector.MarkFailure(ctx, cooled, http.StatusPaymentRequired, 0)
	assertCooling := func(stage string) {
		t.Helper()
		_, err := selector.beginSelectionSession(ctx, account.ProviderBuild, 0, "grok-test", "", "", map[uint64]bool{other.ID: true}, false)
		var unavailable *SelectionUnavailableError
		if !errors.As(err, &unavailable) || unavailable.Reason != SelectionCooling {
			t.Fatalf("%s: cooled account selectable or wrong reason: %v", stage, err)
		}
	}
	assertCooling("after the failure")
	now := time.Now().UTC()
	if err := accounts.UpsertModelQuotaBlock(ctx, account.ModelQuotaBlock{AccountID: cooled.ID, UpstreamModel: "grok-other", Reason: "model_quota_depleted", CooldownUntil: now.Add(time.Hour), UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	assertCooling("after a block on another model")
	past := now.Add(-time.Minute)
	if err := accounts.UpsertModelQuotaBlock(ctx, account.ModelQuotaBlock{AccountID: other.ID, UpstreamModel: "grok-x", Reason: "model_access_denied", CooldownUntil: past, UpdatedAt: past}); err != nil {
		t.Fatal(err)
	}
	if n, err := accounts.PruneExpiredModelQuotaBlocks(ctx, time.Now().UTC(), 1000); err != nil || n == 0 {
		t.Fatalf("prune = %d, %v", n, err)
	}
	assertCooling("after the periodic prune")
}
