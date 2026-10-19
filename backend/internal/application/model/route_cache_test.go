package model

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type countingRouteRepository struct {
	repository.ModelRepository
	lookups int
	during  func()
}

func (r *countingRouteRepository) GetByPublicIDCandidates(ctx context.Context, publicID string) ([]modeldomain.Route, error) {
	r.lookups++
	routes, err := r.ModelRepository.GetByPublicIDCandidates(ctx, publicID)
	if r.during != nil {
		r.during()
	}
	return routes, err
}

func TestRouteResolutionCacheFollowsInvalidation(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "route-cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	modelRepo := relational.NewModelRepository(database)
	accountRepo := relational.NewAccountRepository(database)
	routes := &countingRouteRepository{ModelRepository: modelRepo}
	service := NewService(routes, accountRepo, nil, nil)
	notify := func(_ context.Context, event repository.InvalidationEvent) { service.ApplyInvalidation(event) }
	modelRepo.SetInvalidationObserver(notify)
	accountRepo.SetInvalidationObserver(notify)

	resolve := func(wantFound bool, wantLookups int) []modeldomain.Route {
		t.Helper()
		values, err := service.GetByPublicIDCandidates(ctx, "grok-cache")
		if wantFound && (err != nil || len(values) != 1) {
			t.Fatalf("routes = %#v, err = %v", values, err)
		}
		if !wantFound && !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("routes = %#v, err = %v; want not found", values, err)
		}
		if routes.lookups != wantLookups {
			t.Fatalf("repository lookups = %d, want %d", routes.lookups, wantLookups)
		}
		return values
	}

	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderBuild, []string{"grok-cache"}); err != nil {
		t.Fatal(err)
	}
	credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "cache", SourceKey: "cache", EncryptedAccessToken: "token", Enabled: true, AuthStatus: account.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	// No account reports the model yet; the miss is cached too.
	resolve(false, 1)
	resolve(false, 1)

	// A capability sync shows up on the next request, not after the TTL.
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-cache"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	route := resolve(true, 2)[0]
	resolve(true, 2)[0].PublicID = "mutated"
	if got := resolve(true, 2)[0]; got.PublicID != route.PublicID {
		t.Fatalf("callers mutated the cached route: %#v", got)
	}

	// Account and route edits invalidate immediately as well.
	credential.Enabled = false
	if credential, err = accountRepo.Update(ctx, credential); err != nil {
		t.Fatal(err)
	}
	resolve(false, 3)
	credential.Enabled = true
	if _, err := accountRepo.Update(ctx, credential); err != nil {
		t.Fatal(err)
	}
	resolve(true, 4)
	route.Enabled = false
	if _, err := modelRepo.Update(ctx, route, nil); err != nil {
		t.Fatal(err)
	}
	resolve(false, 5)

	// A lookup that overlaps an invalidation may have read the old state.
	invalidate := func() {
		service.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationRouteChanged})
	}
	invalidate()
	routes.during = invalidate
	resolve(false, 6)
	routes.during = nil
	resolve(false, 7)
	resolve(false, 7)

	// Per-request health and quota events cannot change availability.
	service.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationAccountHealthChanged, Provider: account.ProviderBuild, AccountID: credential.ID})
	service.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationAccountQuotaChanged, AccountID: credential.ID})
	resolve(false, 7)
}
