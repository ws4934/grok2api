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
		if provider == account.ProviderChatGPT && (lead < 24*time.Hour || lead > 30*time.Hour) {
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

// ChatGPT windows stored before reset times were read have none; their
// accounts are marked unsynced once so the quota catch-up refreshes them.
func TestChatGPTQuotaWindowsWithoutResetTimesAreResyncedOnce(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewAccountRepository(database)
	now := time.Now().UTC().Truncate(time.Second)
	resetAt := now.Add(3 * time.Hour)
	ids := map[string]uint64{}
	for _, item := range []struct {
		name     string
		provider account.Provider
		resetAt  *time.Time
	}{
		{"chatgpt-legacy", account.ProviderChatGPT, nil},
		{"chatgpt-current", account.ProviderChatGPT, &resetAt},
		{"web-legacy", account.ProviderWeb, nil},
	} {
		credential, _, err := repo.UpsertByIdentity(ctx, account.Credential{
			Provider: item.provider, AuthType: account.AuthTypeOAuth, Name: item.name, SourceKey: item.name,
			EncryptedAccessToken: "at", Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		windows := []account.QuotaWindow{{AccountID: credential.ID, Mode: "image_gen", Remaining: 0, ResetAt: item.resetAt, SyncedAt: &now, Source: account.QuotaSourceUpstream}}
		if err := repo.SaveQuotaWindows(ctx, credential.ID, "", now, windows); err != nil {
			t.Fatal(err)
		}
		ids[item.name] = credential.ID
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	stale, err := repo.ListStaleQuotaAccountIDs(ctx, []account.Provider{account.ProviderChatGPT, account.ProviderWeb}, now.Add(-time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0] != ids["chatgpt-legacy"] {
		t.Fatalf("stale = %v, want only chatgpt-legacy %d (ids %v)", stale, ids["chatgpt-legacy"], ids)
	}
}
