package relational

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/google/uuid"
)

// The quota catch-up lists never-synced accounts first on every dialect and
// skips a recent sync, including one that reported no limits.
func TestStaleQuotaAccountsFollowSyncMarker(t *testing.T) {
	dialectDatabases(t, func(t *testing.T, database *Database) {
		ctx := context.Background()
		repo := NewAccountRepository(database)
		suffix := uuid.NewString()
		now := time.Now().UTC().Truncate(time.Second)
		old := now.Add(-7 * time.Hour)
		reset := now.Add(time.Hour)
		create := func(name string) account.Credential {
			t.Helper()
			value, _, err := repo.UpsertByIdentity(ctx, account.Credential{
				Provider: account.ProviderChatGPT, AuthType: account.AuthTypeOAuth, Name: name, SourceKey: name + "-" + suffix,
				EncryptedAccessToken: testEncryptedToken, Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			return value
		}
		window := func(id uint64, mode string, remaining int, resetAt, syncedAt *time.Time) account.QuotaWindow {
			return account.QuotaWindow{AccountID: id, Mode: mode, Remaining: remaining, ResetAt: resetAt, SyncedAt: syncedAt, Source: account.QuotaSourceUpstream}
		}
		older, empty, fresh, never := create("older"), create("empty"), create("fresh"), create("never")
		if err := repo.SaveQuotaWindows(ctx, older.ID, "", old, []account.QuotaWindow{window(older.ID, "reason", 3, &reset, &old)}); err != nil {
			t.Fatal(err)
		}
		if err := repo.ReplaceQuotaWindows(ctx, empty.ID, "", now, nil); err != nil {
			t.Fatal(err)
		}
		if err := repo.SaveQuotaWindows(ctx, fresh.ID, "", now, []account.QuotaWindow{window(fresh.ID, "reason", 3, nil, &now)}); err != nil {
			t.Fatal(err)
		}
		before := now.Add(-6 * time.Hour)
		stale, err := repo.ListStaleQuotaAccountIDs(ctx, []account.Provider{account.ProviderChatGPT}, before, 1000)
		if err != nil {
			t.Fatal(err)
		}
		position := func(id uint64) int { return slices.Index(stale, id) }
		if position(empty.ID) >= 0 || position(fresh.ID) >= 0 || position(older.ID) < 0 || position(never.ID) < 0 {
			t.Fatalf("stale = %v (older %d empty %d fresh %d never %d)", stale, older.ID, empty.ID, fresh.ID, never.ID)
		}
		if position(never.ID) > position(older.ID) {
			t.Fatalf("never-synced account sorts after an old one: %v", stale)
		}

		// The marker is owned by quota syncs: account saves and re-imports keep it.
		stored, err := repo.Get(ctx, empty.ID)
		if err != nil || stored.QuotaSyncedAt == nil || stored.QuotaSyncedAt.Sub(now).Abs() > time.Second {
			t.Fatalf("empty snapshot marker = %v, err = %v", stored.QuotaSyncedAt, err)
		}
		stored.QuotaSyncedAt, stored.Name = nil, "renamed"
		if _, err := repo.Update(ctx, stored); err != nil {
			t.Fatal(err)
		}
		reimport := empty
		reimport.ID, reimport.QuotaSyncedAt = 0, nil
		if _, _, err := repo.UpsertByIdentity(ctx, reimport); err != nil {
			t.Fatal(err)
		}
		if again, err := repo.Get(ctx, empty.ID); err != nil || again.QuotaSyncedAt == nil {
			t.Fatalf("account save cleared the quota sync marker: %v, err = %v", again.QuotaSyncedAt, err)
		}
	})
}
