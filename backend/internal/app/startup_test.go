package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
	httpserver "github.com/chenyme/grok2api/backend/internal/transport/http"
)

func TestReadinessStartupReportDoesNotExposeInternalErrors(t *testing.T) {
	state := newStartupState(0)
	state.recordError(errors.New("postgres://private-host/internal"))
	_, _, report, _ := state.snapshot()
	payload, err := json.Marshal(newReadinessStartupReport(report))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "private-host") || !strings.Contains(string(payload), `"errorCount":1`) {
		t.Fatalf("public readiness leaked internal error: %s", payload)
	}
}

func TestReadinessKeepsBuildReadyWhenWebIsUnavailable(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "readiness.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	models := relational.NewModelRepository(database)
	now := time.Now().UTC()
	build, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "build-ready", SourceKey: "build-ready",
		EncryptedAccessToken: "access", EncryptedRefreshToken: "refresh", ExpiresAt: now.Add(time.Hour),
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := models.UpsertRoutes(ctx, []modeldomain.Route{
		{PublicID: "build-model", Provider: accountdomain.ProviderBuild, UpstreamModel: "build-model", Capability: modeldomain.CapabilityResponses, Enabled: true},
		{PublicID: "web-model", Provider: accountdomain.ProviderWeb, UpstreamModel: "web-model", Capability: modeldomain.CapabilityChat, Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := models.ReplaceAccountCapabilities(ctx, build.ID, []string{"build-model"}, now); err != nil {
		t.Fatal(err)
	}
	state := newStartupState(0)
	state.setPhase("running")
	state.setStatsig("unavailable", "test", 0)
	snapshot := readinessSnapshot(ctx, state, func(context.Context) error { return nil }, models, accounts, provider.NewRegistry(), nil)
	if !snapshot.Ready || snapshot.State != "degraded" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if snapshot.Components["grok_build"].State != "ready" || snapshot.Components["grok_web"].State != "unavailable" {
		t.Fatalf("components = %#v", snapshot.Components)
	}
}

func TestReadinessRejectsAccountWithoutAccessToken(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "readiness-missing-access.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	models := relational.NewModelRepository(database)
	now := time.Now().UTC()
	build, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, AuthType: accountdomain.AuthTypeOAuth,
		Name: "refresh-only", SourceKey: "refresh-only", EncryptedRefreshToken: "refresh",
		ExpiresAt: now.Add(time.Hour), Enabled: true, AuthStatus: accountdomain.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := models.UpsertRoutes(ctx, []modeldomain.Route{{
		PublicID: "build-model", Provider: accountdomain.ProviderBuild, UpstreamModel: "build-model", Capability: modeldomain.CapabilityResponses, Enabled: true,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := models.ReplaceAccountCapabilities(ctx, build.ID, []string{"build-model"}, now); err != nil {
		t.Fatal(err)
	}
	state := newStartupState(0)
	state.setPhase("running")
	snapshot := readinessSnapshot(ctx, state, func(context.Context) error { return nil }, models, accounts, provider.NewRegistry(), nil)
	if snapshot.Ready || snapshot.Components["grok_build"].State != "unavailable" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestReadinessRestoresPersistedCooldownWithoutUpstreamProbe(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "cooldown-readiness.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	models := relational.NewModelRepository(database)
	now := time.Now().UTC()
	cooldownUntil := now.Add(10 * time.Minute)
	build, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "cooling", SourceKey: "cooling",
		EncryptedAccessToken: "access", EncryptedRefreshToken: "refresh", ExpiresAt: now.Add(time.Hour),
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive, MaxConcurrent: 1, CooldownUntil: &cooldownUntil,
	})
	if err != nil {
		t.Fatal(err)
	}
	upstreams := []string{"build-model", "build-model-b", "build-model-c"}
	for _, upstream := range upstreams {
		if err := models.UpsertRoutes(ctx, []modeldomain.Route{{PublicID: upstream, Provider: accountdomain.ProviderBuild, UpstreamModel: upstream, Capability: modeldomain.CapabilityResponses, Enabled: true}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := models.ReplaceAccountCapabilities(ctx, build.ID, upstreams, now); err != nil {
		t.Fatal(err)
	}
	state := newStartupState(0)
	state.setPhase("running")
	counted := &countingRoutingAccounts{AccountRepository: accounts}
	snapshot := readinessSnapshot(ctx, state, func(context.Context) error { return nil }, models, counted, provider.NewRegistry(), nil)
	if snapshot.Ready || snapshot.State != "not_ready" || snapshot.Components["grok_build"].State != "unavailable" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	// The cooling account fails every route alike, so one candidate load suffices.
	if counted.loads != 1 {
		t.Fatalf("candidate loads = %d, want 1", counted.loads)
	}
}

type countingRoutingAccounts struct {
	repository.AccountRepository
	loads int
}

func (r *countingRoutingAccounts) ListRoutingCandidates(ctx context.Context, provider accountdomain.Provider, modelRouteID uint64, upstreamModel, quotaMode string) ([]accountdomain.RoutingCandidate, error) {
	r.loads++
	return r.AccountRepository.ListRoutingCandidates(ctx, provider, modelRouteID, upstreamModel, quotaMode)
}

// Concurrent probes share one load per TTL.
func TestCachedReadinessLoadsOncePerTTL(t *testing.T) {
	var loads atomic.Int32
	cached := cachedReadiness(time.Hour, func(context.Context) httpserver.ReadinessSnapshot {
		loads.Add(1)
		time.Sleep(10 * time.Millisecond)
		return httpserver.ReadinessSnapshot{Ready: true}
	})
	var wait sync.WaitGroup
	for range 20 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if !cached(context.Background()).Ready {
				t.Error("cached snapshot lost its state")
			}
		}()
	}
	wait.Wait()
	if loads.Load() != 1 {
		t.Fatalf("loads = %d, want 1", loads.Load())
	}
}
