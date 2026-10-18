package relational

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/google/uuid"
)

// The quota catch-up lists flagged and never-synced accounts first on every
// dialect, skips a recent sync that reported no limits, and keeps a flagged
// window stale until the account is resynced.
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
		older, flagged, empty, fresh, never := create("older"), create("flagged"), create("empty"), create("fresh"), create("never")
		if err := repo.SaveQuotaWindows(ctx, older.ID, "", old, []account.QuotaWindow{window(older.ID, "reason", 3, &reset, &old)}); err != nil {
			t.Fatal(err)
		}
		// Stored before reset_after was read: exhausted without a reset time.
		if err := repo.SaveQuotaWindows(ctx, flagged.ID, "", now, []account.QuotaWindow{window(flagged.ID, "image_gen", 0, nil, &now), window(flagged.ID, "reason", 4, nil, &now)}); err != nil {
			t.Fatal(err)
		}
		if err := repo.ReplaceQuotaWindows(ctx, empty.ID, "", now, nil); err != nil {
			t.Fatal(err)
		}
		if err := repo.SaveQuotaWindows(ctx, fresh.ID, "", now, []account.QuotaWindow{window(fresh.ID, "reason", 3, nil, &now)}); err != nil {
			t.Fatal(err)
		}
		if err := database.InitializeSchema(ctx); err != nil {
			t.Fatal(err)
		}

		before := now.Add(-6 * time.Hour)
		stale, err := repo.ListStaleQuotaAccountIDs(ctx, []account.Provider{account.ProviderChatGPT}, before, 1000)
		if err != nil {
			t.Fatal(err)
		}
		position := func(id uint64) int { return slices.Index(stale, id) }
		if position(empty.ID) >= 0 || position(fresh.ID) >= 0 || position(older.ID) < 0 || position(flagged.ID) < 0 || position(never.ID) < 0 {
			t.Fatalf("stale = %v (older %d flagged %d empty %d fresh %d never %d)", stale, older.ID, flagged.ID, empty.ID, fresh.ID, never.ID)
		}
		if position(flagged.ID) > position(older.ID) || position(never.ID) > position(older.ID) {
			t.Fatalf("flagged/never-synced accounts sort after an old one: %v", stale)
		}

		// A single-mode save from chat traffic does not hide the flagged window.
		later := time.Now().UTC()
		if err := repo.SaveQuotaWindows(ctx, flagged.ID, "", later, []account.QuotaWindow{window(flagged.ID, "reason", 3, nil, &later)}); err != nil {
			t.Fatal(err)
		}
		stale, err = repo.ListStaleQuotaAccountIDs(ctx, []account.Provider{account.ProviderChatGPT}, before, 1000)
		if err != nil || !slices.Contains(stale, flagged.ID) {
			t.Fatalf("flagged account no longer stale: %v, err = %v", stale, err)
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

// Upgrading adds the marker column and fills it from the stored windows, so
// existing accounts do not all look stale at once.
func TestQuotaSyncMarkerBackfilledOnUpgrade(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewAccountRepository(database)
	synced := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	credential, _, err := repo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, Name: "upgraded", SourceKey: "upgraded",
		EncryptedAccessToken: testEncryptedToken, Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveQuotaWindows(ctx, credential.ID, "", synced, []account.QuotaWindow{{AccountID: credential.ID, Mode: "auto", Remaining: 3, SyncedAt: &synced, Source: account.QuotaSourceUpstream}}); err != nil {
		t.Fatal(err)
	}
	if err := database.db.Exec("ALTER TABLE provider_accounts DROP COLUMN quota_synced_at").Error; err != nil {
		t.Fatal(err)
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	stored, err := repo.Get(ctx, credential.ID)
	if err != nil || stored.QuotaSyncedAt == nil || !stored.QuotaSyncedAt.Equal(synced) {
		t.Fatalf("backfilled marker = %v, want %v, err = %v", stored.QuotaSyncedAt, synced, err)
	}
}
