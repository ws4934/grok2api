package relational

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestResponseRepositoryScopesOwnershipByClientAndExpiry(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "responses.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	accountValue, _, err := NewAccountRepository(database).UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "owner", SourceKey: "owner", EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	keyValue, err := NewClientKeyRepository(database).Create(ctx, clientkeydomain.Key{Name: "owner", Prefix: "owner-prefix", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 120, MaxConcurrent: 8})
	if err != nil {
		t.Fatal(err)
	}
	repo := NewResponseRepository(database)
	value := inferencedomain.ResponseOwnership{ResponseID: "resp_1", AccountID: accountValue.ID, ClientKeyID: keyValue.ID, ModelRouteID: 42, Provider: account.ProviderBuild, PromptCacheKey: "cache-key", ReasoningReplayKey: "replay-key", ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now}
	if err := repo.Save(ctx, value); err != nil {
		t.Fatal(err)
	}
	got, err := repo.Get(ctx, value.ResponseID, value.ClientKeyID, now)
	if err != nil || got.AccountID != value.AccountID || got.ModelRouteID != value.ModelRouteID || got.Provider != account.ProviderBuild || got.PromptCacheKey != value.PromptCacheKey || got.ReasoningReplayKey != value.ReasoningReplayKey {
		t.Fatalf("ownership = %#v, err = %v", got, err)
	}
	if _, err := repo.Get(ctx, value.ResponseID, 99, now); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("cross-client lookup err = %v", err)
	}
	if _, err := repo.Get(ctx, value.ResponseID, value.ClientKeyID, now.Add(2*time.Hour)); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("expired lookup err = %v", err)
	}
	deleted, err := repo.DeleteExpired(ctx, now.Add(2*time.Hour), 10, 10)
	if err != nil || deleted.OwnershipDeleted != 1 || deleted.WebStateDeleted != 0 {
		t.Fatalf("deleted = %#v, err = %v", deleted, err)
	}
}

func TestResponseRepositoryDeletesExpiredRowsInBoundedIndependentBatches(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "response-cleanup.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	accounts := NewAccountRepository(database)
	accountValue, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderWeb, Name: "cleanup-owner", SourceKey: "cleanup-owner", EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	keyValue, err := NewClientKeyRepository(database).Create(ctx, clientkeydomain.Key{Name: "cleanup-key", Prefix: "cleanup-prefix", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	repo := NewResponseRepository(database)
	for index := range 3 {
		responseID := "expired-ownership-" + string(rune('a'+index))
		if err := repo.Save(ctx, inferencedomain.ResponseOwnership{ResponseID: responseID, AccountID: accountValue.ID, ClientKeyID: keyValue.ID, Provider: account.ProviderWeb, ExpiresAt: now.Add(-time.Hour), CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
		if err := repo.SaveWebState(ctx, inferencedomain.WebResponseState{ResponseID: "expired-state-" + string(rune('a'+index)), AccountID: accountValue.ID, ConversationID: "conversation", UpstreamParentResponseID: "parent", ResponseJSON: "{}", Status: "completed", ExpiresAt: now.Add(-time.Hour), CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.Save(ctx, inferencedomain.ResponseOwnership{ResponseID: "active-ownership", AccountID: accountValue.ID, ClientKeyID: keyValue.ID, Provider: account.ProviderWeb, ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveWebState(ctx, inferencedomain.WebResponseState{ResponseID: "active-state", AccountID: accountValue.ID, ConversationID: "conversation", UpstreamParentResponseID: "parent", ResponseJSON: "{}", Status: "completed", ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}

	first, err := repo.DeleteExpired(ctx, now, 2, 1)
	if err != nil || first.OwnershipDeleted != 2 || first.WebStateDeleted != 1 || !first.HasMore {
		t.Fatalf("first cleanup = %#v, err = %v", first, err)
	}
	second, err := repo.DeleteExpired(ctx, now, 10, 10)
	if err != nil || second.OwnershipDeleted != 1 || second.WebStateDeleted != 2 || second.HasMore {
		t.Fatalf("second cleanup = %#v, err = %v", second, err)
	}
	if _, err := repo.Get(ctx, "active-ownership", keyValue.ID, now); err != nil {
		t.Fatalf("active ownership was removed: %v", err)
	}
	if _, err := repo.GetWebState(ctx, "active-state", now); err != nil {
		t.Fatalf("active web state was removed: %v", err)
	}
}

func TestResponseRepositorySaveUpsertsNewAndExistingRows(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			database := openTestDatabase
			if dialect == "postgres" {
				database = openPostgresTestDatabase
			}
			db := database(t)
			suffix := fmt.Sprintf("%s-%d", dialect, time.Now().UnixNano()%1e9)
			accountValue, _, err := NewAccountRepository(db).UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderWeb, Name: "upsert-" + suffix, SourceKey: "upsert-" + suffix, EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive})
			if err != nil {
				t.Fatal(err)
			}
			keyValue, err := NewClientKeyRepository(db).Create(ctx, clientkeydomain.Key{Name: "upsert-" + suffix, Prefix: "upsert-" + suffix, SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			repo := NewResponseRepository(db)
			id := "resp-upsert-" + suffix
			base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
			// The second save overwrites every column, created_at included; updated_at is always stamped at save time.
			for _, value := range []inferencedomain.ResponseOwnership{
				{ResponseID: id, AccountID: accountValue.ID, ClientKeyID: keyValue.ID, ModelRouteID: 7, Provider: account.ProviderWeb, PromptCacheKey: "cache-1", ReasoningReplayKey: "replay-1", ExpiresAt: base.Add(time.Hour), CreatedAt: base, UpdatedAt: base},
				{ResponseID: id, AccountID: accountValue.ID, ClientKeyID: keyValue.ID, Provider: account.ProviderWeb, ReasoningReplayKey: "replay-2", ExpiresAt: base.Add(3 * time.Hour), CreatedAt: base.Add(2 * time.Hour), UpdatedAt: base.Add(2 * time.Hour)},
			} {
				started := time.Now().UTC().Truncate(time.Millisecond)
				if err := repo.Save(ctx, value); err != nil {
					t.Fatal(err)
				}
				got, err := repo.Get(ctx, id, keyValue.ID, base)
				if err != nil || got.AccountID != value.AccountID || got.ModelRouteID != value.ModelRouteID || got.Provider != value.Provider || got.PromptCacheKey != value.PromptCacheKey || got.ReasoningReplayKey != value.ReasoningReplayKey ||
					!got.ExpiresAt.Equal(value.ExpiresAt) || !got.CreatedAt.Equal(value.CreatedAt) || got.UpdatedAt.Before(started) {
					t.Fatalf("ownership after save = %#v, want %#v, err = %v", got, value, err)
				}
			}
			for _, value := range []inferencedomain.WebResponseState{
				{ResponseID: id, AccountID: accountValue.ID, ConversationID: "conv-1", UpstreamParentResponseID: "parent-1", ResponseJSON: `{"n":1}`, Status: "completed", ExpiresAt: base.Add(time.Hour), CreatedAt: base, UpdatedAt: base},
				{ResponseID: id, AccountID: accountValue.ID, ConversationID: "conv-2", UpstreamParentResponseID: "parent-2", ResponseJSON: `{"n":2}`, Status: "failed", ExpiresAt: base.Add(3 * time.Hour), CreatedAt: base.Add(2 * time.Hour), UpdatedAt: base.Add(2 * time.Hour)},
			} {
				started := time.Now().UTC().Truncate(time.Millisecond)
				if err := repo.SaveWebState(ctx, value); err != nil {
					t.Fatal(err)
				}
				got, err := repo.GetWebState(ctx, id, base)
				if err != nil || got.AccountID != value.AccountID || got.ConversationID != value.ConversationID || got.UpstreamParentResponseID != value.UpstreamParentResponseID || got.ResponseJSON != value.ResponseJSON || got.Status != value.Status ||
					!got.ExpiresAt.Equal(value.ExpiresAt) || !got.CreatedAt.Equal(value.CreatedAt) || got.UpdatedAt.Before(started) {
					t.Fatalf("web state after save = %#v, want %#v, err = %v", got, value, err)
				}
			}
		})
	}
}
