package relational

import (
	"context"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// Renewals scheduled by the old minutes-before-expiry rule move to the
// provider's lead at startup; other providers keep their schedule.
func TestChatGPTCredentialRefreshIsRescheduledAtStartup(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewAccountRepository(database)
	expiresAt := time.Now().UTC().Add(7 * 24 * time.Hour).Truncate(time.Second)
	oldDue := expiresAt.Add(-6 * time.Minute)
	ids := map[account.Provider]uint64{}
	for _, provider := range []account.Provider{account.ProviderChatGPT, account.ProviderBuild} {
		credential, _, err := repo.UpsertByIdentity(ctx, account.Credential{
			Provider: provider, AuthType: account.AuthTypeOAuth, Name: string(provider) + "-lead", SourceKey: string(provider) + "-lead",
			EncryptedAccessToken: "at", EncryptedRefreshToken: "rt", ExpiresAt: expiresAt, RefreshDueAt: &oldDue,
			Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids[provider] = credential.ID
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	for provider, id := range ids {
		stored, err := repo.Get(ctx, id)
		if err != nil || stored.RefreshDueAt == nil {
			t.Fatalf("%s: %+v %v", provider, stored.RefreshDueAt, err)
		}
		lead := expiresAt.Sub(*stored.RefreshDueAt)
		if provider == account.ProviderChatGPT && (lead < 24*time.Hour || lead > 24*time.Hour+3*time.Minute) {
			t.Fatalf("chatgpt lead = %v", lead)
		}
		if provider == account.ProviderBuild && !stored.RefreshDueAt.Equal(oldDue) {
			t.Fatalf("build due moved to %v", stored.RefreshDueAt)
		}
	}
	// A second start changes nothing.
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if again, _ := repo.Get(ctx, ids[account.ProviderChatGPT]); again.RefreshDueAt == nil || expiresAt.Sub(*again.RefreshDueAt) < 24*time.Hour {
		t.Fatalf("second start moved the renewal: %v", again.RefreshDueAt)
	}
}
