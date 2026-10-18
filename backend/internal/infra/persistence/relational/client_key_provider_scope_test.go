package relational

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestClientKeyProviderScopesRoundTrip(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	keys := NewClientKeyRepository(database)
	future := clientkeydomain.ProviderScopeOf(account.ProviderWeb, "chatgpt")
	explicit, err := keys.Create(ctx, clientkeydomain.Key{Name: "explicit", Prefix: "explicit", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, ProviderScope: future})
	if err != nil || !explicit.ProviderScope.Equal(future) {
		t.Fatalf("created explicit scope = %+v, err = %v", explicit.ProviderScope, err)
	}
	all, err := keys.Create(ctx, clientkeydomain.Key{Name: "all", Prefix: "all", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, ProviderScope: clientkeydomain.AllProviders()})
	if err != nil || !all.ProviderScope.All {
		t.Fatalf("created all scope = %+v, err = %v", all.ProviderScope, err)
	}
	byID, err := keys.Get(ctx, explicit.ID)
	if err != nil || !byID.ProviderScope.Equal(future) {
		t.Fatalf("loaded scope = %+v, err = %v", byID.ProviderScope, err)
	}
	if byPrefix, err := keys.GetByPrefix(ctx, "explicit"); err != nil || !byPrefix.ProviderScope.Equal(future) {
		t.Fatalf("auth lookup scope = %+v, err = %v", byPrefix.ProviderScope, err)
	}
	listed, _, err := keys.List(ctx, repository.ClientKeyListQuery{Page: repository.PageQuery{Limit: 20}})
	if err != nil || len(listed) != 2 {
		t.Fatalf("listed = %#v, err = %v", listed, err)
	}
	for _, value := range listed {
		if (value.ID == explicit.ID && !value.ProviderScope.Equal(future)) || (value.ID == all.ID && !value.ProviderScope.All) {
			t.Fatalf("listed scope for %s = %+v", value.Name, value.ProviderScope)
		}
	}
	countRows := func(id uint64) int64 {
		t.Helper()
		var count int64
		if err := database.db.WithContext(ctx).Model(&clientKeyProviderScopeModel{}).Where("client_key_id = ?", id).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		return count
	}
	byID.ProviderScope = clientkeydomain.AllProviders()
	updated, err := keys.Update(ctx, byID)
	if err != nil || !updated.ProviderScope.All || countRows(explicit.ID) != 0 {
		t.Fatalf("scope after switching to all = %+v, err = %v", updated.ProviderScope, err)
	}
	updated.ProviderScope = clientkeydomain.ProviderScopeOf(account.ProviderBuild)
	if updated, err = keys.Update(ctx, updated); err != nil || !updated.ProviderScope.Equal(clientkeydomain.ProviderScopeOf(account.ProviderBuild)) {
		t.Fatalf("scope after switching to a list = %+v, err = %v", updated.ProviderScope, err)
	}
	if err := keys.Delete(ctx, explicit.ID); err != nil {
		t.Fatal(err)
	}
	if count := countRows(explicit.ID); count != 0 {
		t.Fatalf("deleted key kept %d provider scope rows", count)
	}
}

// providerScopeMaskFixtures maps each retired bitmask to the scope it must become.
var providerScopeMaskFixtures = []struct {
	mask uint8
	want clientkeydomain.ProviderScope
}{
	{mask: 7, want: clientkeydomain.AllProviders()},
	{mask: 1, want: clientkeydomain.ProviderScopeOf(account.ProviderBuild)},
	{mask: 3, want: clientkeydomain.ProviderScopeOf(account.ProviderBuild, account.ProviderWeb)},
}

func TestInitializeSchemaMigratesClientKeyProviderScopeMask(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "legacy-provider-scope-mask.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	ids := seedProviderScopeMaskKeys(t, ctx, database, "")
	if err := recreateClientKeysWithProviderScopeMask(ctx, database); err != nil {
		t.Fatal(err)
	}
	setProviderScopeMasks(t, ctx, database, ids)
	for range 2 {
		if err := database.InitializeSchema(ctx); err != nil {
			t.Fatal(err)
		}
		assertMigratedProviderScopes(t, ctx, database, ids)
	}
	var tableSQL string
	if err := database.db.WithContext(ctx).Raw("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'client_keys'").Scan(&tableSQL).Error; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(tableSQL, "provider_scope_mask") {
		t.Fatalf("retired provider scope mask survived the upgrade: %s", tableSQL)
	}
	if !database.db.WithContext(ctx).Migrator().HasIndex("client_keys", "idx_client_keys_prefix") {
		t.Fatal("client key prefix index was not restored after the table rebuild")
	}
}

func TestPostgresMigratesClientKeyProviderScopeMask(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not configured")
	}
	ctx := context.Background()
	database, err := OpenPostgres(ctx, dsn, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	ids := seedProviderScopeMaskKeys(t, ctx, database, "postgres-")
	for _, statement := range []string{
		"ALTER TABLE client_keys ADD COLUMN provider_scope_mask smallint NOT NULL DEFAULT 7 CONSTRAINT chk_client_keys_provider_scope CHECK (provider_scope_mask BETWEEN 1 AND 7)",
		"ALTER TABLE client_keys DROP COLUMN provider_scope_mode",
		"DROP TABLE client_key_provider_scopes",
	} {
		if err := database.db.WithContext(ctx).Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	setProviderScopeMasks(t, ctx, database, ids)
	for range 2 {
		if err := database.InitializeSchema(ctx); err != nil {
			t.Fatal(err)
		}
		assertMigratedProviderScopes(t, ctx, database, ids)
	}
	definition, err := database.constraintDefinition(ctx, consoleConstraint{model: &clientKeyModel{}, table: "client_keys", name: "chk_client_keys_provider_scope"})
	if err != nil || definition != "" {
		t.Fatalf("retired provider scope CHECK = %q, err = %v", definition, err)
	}
	keys := NewClientKeyRepository(database)
	for _, id := range ids {
		if err := keys.Delete(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
}

func seedProviderScopeMaskKeys(t *testing.T, ctx context.Context, database *Database, prefix string) []uint64 {
	t.Helper()
	keys := NewClientKeyRepository(database)
	ids := make([]uint64, 0, len(providerScopeMaskFixtures))
	for _, fixture := range providerScopeMaskFixtures {
		name := fmt.Sprintf("%smask-%d", prefix, fixture.mask)
		value, err := keys.Create(ctx, clientkeydomain.Key{Name: name, Prefix: name, SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, value.ID)
	}
	return ids
}

func setProviderScopeMasks(t *testing.T, ctx context.Context, database *Database, ids []uint64) {
	t.Helper()
	for index, fixture := range providerScopeMaskFixtures {
		if err := database.db.WithContext(ctx).Exec("UPDATE client_keys SET provider_scope_mask = ? WHERE id = ?", fixture.mask, ids[index]).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func assertMigratedProviderScopes(t *testing.T, ctx context.Context, database *Database, ids []uint64) {
	t.Helper()
	keys := NewClientKeyRepository(database)
	for index, fixture := range providerScopeMaskFixtures {
		stored, err := keys.Get(ctx, ids[index])
		if err != nil || !stored.ProviderScope.Equal(fixture.want) {
			t.Fatalf("mask %d migrated to %+v, err = %v", fixture.mask, stored.ProviderScope, err)
		}
		if fixture.mask == 7 && !stored.AccountScope().AllowsProvider("chatgpt") {
			t.Fatal("a migrated all-provider key must include providers registered later")
		}
	}
	db := database.db.WithContext(ctx)
	if db.Migrator().HasColumn("client_keys", "provider_scope_mask") {
		t.Fatal("retired provider scope mask column still exists")
	}
	if err := db.Exec("UPDATE client_keys SET provider_scope_mode = 'bogus' WHERE id = ?", ids[0]).Error; err == nil {
		t.Fatal("provider scope mode constraint accepted an unknown mode")
	}
}

func recreateClientKeysWithProviderScopeMask(ctx context.Context, database *Database) error {
	return database.withSQLiteForeignKeysDisabled(ctx, func() error {
		db := database.db.WithContext(ctx)
		var tableSQL string
		if err := db.Raw("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'client_keys'").Scan(&tableSQL).Error; err != nil {
			return err
		}
		legacySQL := strings.Replace(tableSQL, ",`provider_scope_mode` text NOT NULL DEFAULT \"all\"", ",`provider_scope_mask` integer NOT NULL DEFAULT 7", 1)
		legacySQL = strings.Replace(legacySQL, ",CONSTRAINT `chk_client_keys_provider_scope_mode` CHECK (provider_scope_mode IN ('all','list'))", ",CONSTRAINT `chk_client_keys_provider_scope` CHECK (provider_scope_mask BETWEEN 1 AND 7)", 1)
		if strings.Contains(legacySQL, "provider_scope_mode") {
			return fmt.Errorf("legacy fixture still has the provider scope mode: %s", legacySQL)
		}
		legacySQL = strings.Replace(legacySQL, "client_keys", "client_keys_legacy_mask", 1)
		const columns = "id,name,prefix,secret_hash,encrypted_secret,internal_kind,enabled,expires_at,rpm_limit,max_concurrent,billing_limit_usd_ticks,billed_usage_usd_ticks,reserved_usage_usd_ticks,allow_model_aliases,tier_scope_mask,last_used_at,created_at,updated_at"
		for _, statement := range []string{
			legacySQL,
			"INSERT INTO client_keys_legacy_mask (" + columns + ") SELECT " + columns + " FROM client_keys",
			"DROP TABLE client_keys",
			"ALTER TABLE client_keys_legacy_mask RENAME TO client_keys",
			"DROP TABLE client_key_provider_scopes",
		} {
			if err := db.Exec(statement).Error; err != nil {
				return fmt.Errorf("%s: %w", statement, err)
			}
		}
		return nil
	})
}
