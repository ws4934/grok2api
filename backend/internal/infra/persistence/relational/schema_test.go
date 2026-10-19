package relational

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
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
