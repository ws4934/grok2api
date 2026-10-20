package relational

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/google/uuid"
)

// accountStateDatabases runs the test against a fresh SQLite database and, when configured, PostgreSQL.
func accountStateDatabases(t *testing.T, run func(*testing.T, *Database)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) { run(t, openTestDatabase(t)) })
	t.Run("postgres", func(t *testing.T) {
		dsn := os.Getenv("TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("TEST_POSTGRES_DSN is not configured")
		}
		database, err := OpenPostgres(context.Background(), dsn, 4, 4)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = database.Close() })
		if err := database.InitializeSchema(context.Background()); err != nil {
			t.Fatal(err)
		}
		run(t, database)
	})
}

func TestCredentialStateAndPlanPersistence(t *testing.T) {
	accountStateDatabases(t, func(t *testing.T, database *Database) {
		ctx := context.Background()
		repo := NewAccountRepository(database)
		provider := account.Provider("acme_" + uuid.NewString()[:8])
		value := account.Credential{
			Provider: provider, AuthType: account.AuthTypeOAuth, Name: "stateful", SourceKey: "state-" + uuid.NewString(), UserID: "user-" + uuid.NewString(),
			EncryptedAccessToken: testEncryptedToken, EncryptedRefreshToken: "sealed-refresh", EncryptedState: "sealed-state",
			Plan: "plus", Enabled: true, AuthStatus: account.AuthStatusActive,
		}
		created, _, err := repo.UpsertByIdentity(ctx, value)
		if err != nil {
			t.Fatal(err)
		}
		if created.Plan != "plus" || created.EncryptedState != "sealed-state" {
			t.Fatalf("created = %#v", created)
		}
		material, err := repo.GetCredentialMaterial(ctx, created.ID, provider)
		if err != nil || material.EncryptedState != "sealed-state" {
			t.Fatalf("material = %#v, err = %v", material, err)
		}
		if hydrated, ok := material.ApplyTo(account.Credential{ID: created.ID, Provider: provider}); !ok || hydrated.EncryptedState != "sealed-state" {
			t.Fatalf("hydrated = %#v", hydrated)
		}

		// A re-import without plan or state keeps the stored values.
		value.Plan, value.EncryptedState = "", ""
		if _, _, err := repo.UpsertByIdentity(ctx, value); err != nil {
			t.Fatal(err)
		}
		if stored, err := repo.Get(ctx, created.ID); err != nil || stored.Plan != "plus" || stored.EncryptedState != "sealed-state" {
			t.Fatalf("re-imported = %#v, err = %v", stored, err)
		}

		// Token refresh replaces state only when the provider returned one.
		if refreshed, err := repo.UpdateTokens(ctx, created.ID, "rotated", "", "", time.Now().UTC().Add(time.Hour), 0); err != nil || refreshed.EncryptedState != "sealed-state" {
			t.Fatalf("refresh without state = %#v, err = %v", refreshed, err)
		}
		if refreshed, err := repo.UpdateTokens(ctx, created.ID, "rotated-2", "", "sealed-state-2", time.Now().UTC().Add(time.Hour), 0); err != nil || refreshed.EncryptedState != "sealed-state-2" || refreshed.EncryptedRefreshToken != "sealed-refresh" {
			t.Fatalf("refresh with state = %#v, err = %v", refreshed, err)
		}

		// Identity sync writes a reported plan; an empty plan keeps it.
		if err := repo.UpdateIdentityMetadata(ctx, created.ID, "", "", "", "pro"); err != nil {
			t.Fatal(err)
		}
		if err := repo.UpdateIdentityMetadata(ctx, created.ID, "user@example.com", "", "", ""); err != nil {
			t.Fatal(err)
		}
		if stored, err := repo.Get(ctx, created.ID); err != nil || stored.Plan != "pro" || stored.Email != "user@example.com" {
			t.Fatalf("identity = %#v, err = %v", stored, err)
		}
	})
}

func TestCredentialRefreshQueriesUseProviderSet(t *testing.T) {
	accountStateDatabases(t, func(t *testing.T, database *Database) {
		ctx := context.Background()
		repo := NewAccountRepository(database)
		suffix := uuid.NewString()[:8]
		refreshable := account.Provider("acme_" + suffix)
		other := account.Provider("other_" + suffix)
		now := time.Now().UTC()
		ids := map[account.Provider]uint64{}
		for _, providerValue := range []account.Provider{refreshable, other} {
			created, _, err := repo.UpsertByIdentity(ctx, account.Credential{
				Provider: providerValue, AuthType: account.AuthTypeOAuth, Name: string(providerValue), SourceKey: "due-" + string(providerValue),
				EncryptedAccessToken: testEncryptedToken, EncryptedRefreshToken: "refresh", ExpiresAt: now, Enabled: true, AuthStatus: account.AuthStatusActive,
			})
			if err != nil {
				t.Fatal(err)
			}
			ids[providerValue] = created.ID
		}
		providers := []account.Provider{refreshable}
		due, err := repo.ListDueCredentialRefreshIDs(ctx, providers, now, 1000)
		if err != nil || !containsAccountID(due, ids[refreshable]) || containsAccountID(due, ids[other]) {
			t.Fatalf("due = %v, ids = %v, err = %v", due, ids, err)
		}
		critical, err := repo.ListCriticalCredentialRefreshIDs(ctx, providers, now, now.Add(time.Minute), 1000)
		if err != nil || !containsAccountID(critical, ids[refreshable]) || containsAccountID(critical, ids[other]) {
			t.Fatalf("critical = %v, err = %v", critical, err)
		}
		if next, err := repo.NextCredentialRefreshDueAt(ctx, providers); err != nil || next == nil {
			t.Fatalf("next = %v, err = %v", next, err)
		}
		if _, err := repo.BackfillCredentialRefreshSchedules(ctx, providers, now, 10); err != nil {
			t.Fatal(err)
		}
		if due, err := repo.ListDueCredentialRefreshIDs(ctx, nil, now, 1000); err != nil || len(due) != 0 {
			t.Fatalf("empty provider set due = %v, err = %v", due, err)
		}

		stale, err := repo.ListStaleQuotaAccountIDs(ctx, providers, now.Add(time.Minute), 1000)
		if err != nil || !containsAccountID(stale, ids[refreshable]) || containsAccountID(stale, ids[other]) {
			t.Fatalf("stale = %v, err = %v", stale, err)
		}
	})
}

func containsAccountID(values []uint64, id uint64) bool {
	for _, value := range values {
		if value == id {
			return true
		}
	}
	return false
}

func TestUpsertModelQuotaBlockKeepsTheLaterCooldown(t *testing.T) {
	accountStateDatabases(t, func(t *testing.T, database *Database) {
		ctx := context.Background()
		repo := NewAccountRepository(database)
		created, _, err := repo.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderBuild, Name: "blocked", SourceKey: "blocked-" + uuid.NewString(),
			EncryptedAccessToken: "access", ExpiresAt: time.Now().UTC().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive,
		})
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC().Truncate(time.Second)
		check := func(reason string, until time.Time, wantReason string, wantUntil time.Time) {
			t.Helper()
			if err := repo.UpsertModelQuotaBlock(ctx, account.ModelQuotaBlock{AccountID: created.ID, UpstreamModel: "grok-x", Reason: reason, CooldownUntil: until}); err != nil {
				t.Fatal(err)
			}
			var row accountModelQuotaBlockModel
			if err := database.db.WithContext(ctx).Where("account_id = ? AND upstream_model = ?", created.ID, "grok-x").First(&row).Error; err != nil {
				t.Fatal(err)
			}
			if row.Reason != wantReason || !row.CooldownUntil.Equal(wantUntil) {
				t.Fatalf("block = %q until %v, want %q until %v", row.Reason, row.CooldownUntil, wantReason, wantUntil)
			}
		}
		check("first", now.Add(time.Hour), "first", now.Add(time.Hour))
		check("shorter", now.Add(time.Minute), "first", now.Add(time.Hour))
		check("longer", now.Add(2*time.Hour), "longer", now.Add(2*time.Hour))
	})
}
