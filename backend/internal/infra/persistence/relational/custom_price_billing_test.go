package relational

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// An administrator price (PriceID != 0) is billed, sorted and charted at its own
// estimate even when upstream also reported cost_in_usd_ticks; without one the
// upstream cost still wins over the official estimate.
func TestCustomPriceOverridesUpstreamReportedCost(t *testing.T) {
	ctx := context.Background()
	open := map[string]func() (*Database, error){
		"sqlite": func() (*Database, error) { return OpenSQLite(ctx, filepath.Join(t.TempDir(), "custom-price.db")) },
	}
	if dsn := os.Getenv("TEST_POSTGRES_DSN"); dsn != "" {
		open["postgres"] = func() (*Database, error) { return OpenPostgres(ctx, dsn, 10, 2) }
	}
	for name, openDatabase := range open {
		t.Run(name, func(t *testing.T) {
			database, err := openDatabase()
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			if err := database.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			key := clientKeyModel{Name: "custom-price-billing", Prefix: "custom-price-billing", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 120, MaxConcurrent: 8}
			if err := database.db.WithContext(ctx).Create(&key).Error; err != nil {
				t.Fatal(err)
			}
			// A fixed past window keeps a shared PostgreSQL database's other audits out of the aggregates.
			now := time.Date(2001, 1, 1, 12, 0, 0, 0, time.UTC)
			repo := NewAuditRepository(database)
			if err := repo.CreateBatch(ctx, []audit.Record{
				{RequestID: "custom-price-marked-up", ClientKeyID: key.ID, ModelRouteID: 1, ModelPublicID: "grok-4.3", Provider: "grok_console", StatusCode: 200,
					CostInUSDTicks: 5, EstimatedCostInUSDTicks: 2000, PricingModel: "grok-4.3", PricingVersion: audit.CustomPricingVersion, PriceID: 7, CreatedAt: now.Add(-time.Hour)},
				{RequestID: "custom-price-official", ClientKeyID: key.ID, ModelRouteID: 1, ModelPublicID: "grok-4.3", Provider: "grok_console", StatusCode: 200,
					CostInUSDTicks: 20, EstimatedCostInUSDTicks: 90, PricingModel: "grok-4.3", PricingVersion: audit.OfficialPricingAsOf, CreatedAt: now.Add(-time.Hour + time.Second)},
			}); err != nil {
				t.Fatal(err)
			}

			var stored clientKeyModel
			if err := database.db.WithContext(ctx).First(&stored, key.ID).Error; err != nil {
				t.Fatal(err)
			}
			if stored.BilledUsageUSDTicks != 2000+20 {
				t.Fatalf("billed usage = %d, want custom 2000 + upstream 20", stored.BilledUsageUSDTicks)
			}

			query := repository.AuditCursorQuery{Limit: 10, Start: now.Add(-2 * time.Hour), End: now, Sort: repository.SortQuery{Field: "billing", Direction: repository.SortDescending}}
			rows, _, err := repo.ListCursor(ctx, query)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 2 || rows[0].RequestID != "custom-price-marked-up" || rows[1].RequestID != "custom-price-official" {
				t.Fatalf("billing sort = %#v", rows)
			}
			// The application cursor carries the billed amount (2000) of the last row.
			query.Cursor = &repository.SortCursor{ID: rows[0].ID, Value: int64(2000)}
			next, _, err := repo.ListCursor(ctx, query)
			if err != nil {
				t.Fatal(err)
			}
			if len(next) != 1 || next[0].RequestID != "custom-price-official" {
				t.Fatalf("billing cursor page = %#v", next)
			}

			snapshot, err := NewDashboardRepository(database).Snapshot(ctx, testDashboardWindow(testDashboardBoundaries(now.Add(-24*time.Hour), time.Hour, 24)), now)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Usage.BilledCostUSDTicks != 2000+20 {
				t.Fatalf("dashboard billed cost = %d", snapshot.Usage.BilledCostUSDTicks)
			}
		})
	}
}
