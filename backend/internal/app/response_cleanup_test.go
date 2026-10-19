package app

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
)

func TestCleanupExpiredResponsesDrainsBacklogInOneRun(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "cleanup.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountValue, _, err := relational.NewAccountRepository(database).UpsertByIdentity(ctx, accountdomain.Credential{Provider: accountdomain.ProviderWeb, Name: "cleanup", SourceKey: "cleanup", EncryptedAccessToken: "encrypted-token", AuthStatus: accountdomain.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	keyValue, err := relational.NewClientKeyRepository(database).Create(ctx, clientkeydomain.Key{Name: "cleanup", Prefix: "cleanup", SecretHash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", EncryptedSecret: "encrypted-token", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	responses := relational.NewResponseRepository(database)
	now := time.Now().UTC()
	// More expired web states than the former cap of 100 batches per run removed.
	const expiredStates, expiredOwnership = 100*webResponseStateCleanupBatchSize + 60, responseOwnershipCleanupBatchSize + 10
	for index := range expiredStates {
		id := fmt.Sprintf("expired-%d", index)
		if err := responses.SaveWebState(ctx, inferencedomain.WebResponseState{ResponseID: id, AccountID: accountValue.ID, ConversationID: "conversation", UpstreamParentResponseID: "parent", ResponseJSON: "{}", Status: "completed", ExpiresAt: now.Add(-time.Minute), CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
		if index < expiredOwnership {
			if err := responses.Save(ctx, inferencedomain.ResponseOwnership{ResponseID: id, AccountID: accountValue.ID, ClientKeyID: keyValue.ID, Provider: accountdomain.ProviderWeb, ExpiresAt: now.Add(-time.Minute), CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := responses.SaveWebState(ctx, inferencedomain.WebResponseState{ResponseID: "fresh", AccountID: accountValue.ID, ConversationID: "conversation", UpstreamParentResponseID: "parent", ResponseJSON: "{}", Status: "completed", ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := responses.Save(ctx, inferencedomain.ResponseOwnership{ResponseID: "fresh", AccountID: accountValue.ID, ClientKeyID: keyValue.ID, Provider: accountdomain.ProviderWeb, ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}

	app := &Application{logger: slog.New(slog.DiscardHandler), responses: responses}
	if err := app.cleanupExpiredResponses(ctx, now); err != nil {
		t.Fatal(err)
	}
	if left, err := responses.DeleteExpired(ctx, now, 1, 1); err != nil || left.OwnershipDeleted != 0 || left.WebStateDeleted != 0 {
		t.Fatalf("expired rows left after one run = %#v, err = %v", left, err)
	}
	if _, err := responses.GetWebState(ctx, "fresh", now); err != nil {
		t.Fatalf("fresh web state was removed: %v", err)
	}
	if _, err := responses.Get(ctx, "fresh", keyValue.ID, now); err != nil {
		t.Fatalf("fresh ownership was removed: %v", err)
	}
}
