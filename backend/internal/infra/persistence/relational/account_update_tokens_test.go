package relational

import (
	"context"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/google/uuid"
)

// Update saves an account read before a token renewal; the renewed tokens and
// renewal state must survive it.
func TestAccountUpdateKeepsRenewedTokens(t *testing.T) {
	accountStateDatabases(t, func(t *testing.T, database *Database) {
		ctx := context.Background()
		repo := NewAccountRepository(database)
		stale, _, err := repo.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderBuild, Name: "renewed", SourceKey: "renewed-" + uuid.NewString(),
			EncryptedAccessToken: "access-0", EncryptedRefreshToken: "refresh-0", ExpiresAt: time.Now().UTC().Add(time.Hour),
			Enabled: true, AuthStatus: account.AuthStatusActive,
		})
		if err != nil {
			t.Fatal(err)
		}
		renewed, err := repo.UpdateTokens(ctx, stale.ID, "access-1", "refresh-1", "", time.Now().UTC().Add(2*time.Hour), 0)
		if err != nil {
			t.Fatal(err)
		}
		stale.Name = "edited"
		got, err := repo.Update(ctx, stale)
		if err != nil {
			t.Fatal(err)
		}
		if got.Name != "edited" || got.EncryptedAccessToken != "access-1" || got.EncryptedRefreshToken != "refresh-1" ||
			!got.ExpiresAt.Equal(renewed.ExpiresAt) || got.LastRefreshAt == nil {
			t.Fatalf("after a stale update: name=%q access=%q refresh=%q expires=%v lastRefresh=%v", got.Name, got.EncryptedAccessToken, got.EncryptedRefreshToken, got.ExpiresAt, got.LastRefreshAt)
		}
	})
}
