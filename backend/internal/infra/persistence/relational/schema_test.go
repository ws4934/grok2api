package relational

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/google/uuid"
)

func TestManagedRoutesAllowOnePublicIDPerCapability(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "model-capability-pool.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := NewModelRepository(database)
	routes := []modeldomain.Route{
		{PublicID: "grok-imagine-image-quality", Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-image-quality", Capability: modeldomain.CapabilityImage, Enabled: true},
		{PublicID: "grok-imagine-image-quality", Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-image-quality", Capability: modeldomain.CapabilityImageEdit, Enabled: true},
	}
	if err := repo.ReplaceProviderRoutes(ctx, account.ProviderConsole, routes); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertDiscovered(ctx, account.ProviderConsole, []string{"grok-imagine-image-quality"}); err != nil {
		t.Fatal(err)
	}
	var rows []modelRouteModel
	if err := database.db.Where("public_id = ?", "Console/grok-imagine-image-quality").Order("capability ASC").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Capability == rows[1].Capability {
		t.Fatalf("managed capability routes = %#v", rows)
	}
	firstIDs := []uint64{rows[0].ID, rows[1].ID}
	if err := repo.ReplaceProviderRoutes(ctx, account.ProviderConsole, routes); err != nil {
		t.Fatal(err)
	}
	rows = nil
	if err := database.db.Where("public_id = ?", "Console/grok-imagine-image-quality").Order("capability ASC").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].ID != firstIDs[0] || rows[1].ID != firstIDs[1] {
		t.Fatalf("managed capability route IDs changed: before=%v after=%#v", firstIDs, rows)
	}
}

// The partial unique index rejects a second managed route for one public ID and
// capability, while manual routes may share it.
func TestManagedRouteIndexRejectsDuplicates(t *testing.T) {
	dialectDatabases(t, func(t *testing.T, database *Database) {
		db := database.db.WithContext(context.Background())
		publicID := "dup-" + uuid.NewString()[:8]
		now := time.Now().UTC()
		route := func(origin string) *modelRouteModel {
			return &modelRouteModel{PublicID: publicID, Provider: string(account.ProviderBuild), UpstreamModel: "m-" + uuid.NewString()[:8],
				Capability: string(modeldomain.CapabilityResponses), Origin: origin, Enabled: true, CreatedAt: now, UpdatedAt: now}
		}
		for _, origin := range []string{"manual", "manual", "catalog"} {
			if err := db.Create(route(origin)).Error; err != nil {
				t.Fatalf("create %s route: %v", origin, err)
			}
		}
		if err := mapError(db.Create(route("discovered")).Error); !errors.Is(err, repository.ErrConflict) {
			t.Fatalf("second managed route err = %v", err)
		}
	})
}

// SQLite changes a CHECK by rebuilding the table; rebuilding a referenced parent
// inside the guard must not cascade-delete its children.
func TestSQLiteParentRebuildKeepsChildRows(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	accounts := NewAccountRepository(database)
	parent, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "parent", SourceKey: "parent", EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	if err := accounts.SaveBilling(ctx, account.Billing{AccountID: parent.ID, SyncedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	db := database.db.WithContext(ctx)
	var ddl string
	if err := db.Raw("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'provider_accounts'").Scan(&ddl).Error; err != nil {
		t.Fatal(err)
	}
	rebuild := func() error {
		for _, statement := range []string{
			strings.Replace(ddl, "provider_accounts", "provider_accounts_rebuild", 1),
			"INSERT INTO provider_accounts_rebuild SELECT * FROM provider_accounts",
			"DROP TABLE provider_accounts",
			"ALTER TABLE provider_accounts_rebuild RENAME TO provider_accounts",
		} {
			if err := db.Exec(statement).Error; err != nil {
				return err
			}
		}
		return nil
	}
	if err := database.withSQLiteForeignKeysDisabled(ctx, rebuild); err != nil {
		t.Fatal(err)
	}
	if count := tableRowCount(t, database, "account_billing_snapshots"); count != 1 {
		t.Fatalf("billing rows after parent rebuild = %d", count)
	}
	var foreignKeys int
	if err := db.Raw("PRAGMA foreign_keys").Scan(&foreignKeys).Error; err != nil || foreignKeys != 1 {
		t.Fatalf("foreign_keys after rebuild = %d, %v", foreignKeys, err)
	}
}
