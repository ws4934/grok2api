package relational

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/pricing"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/google/uuid"
)

// priceDatabases runs a check on SQLite and, when configured, PostgreSQL.
func priceDatabases(t *testing.T, run func(*testing.T, *Database)) {
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

func TestModelPriceRepositoryIsAppendOnly(t *testing.T) {
	priceDatabases(t, func(t *testing.T, database *Database) {
		ctx := context.Background()
		repo := NewModelPriceRepository(database)
		provider := "p" + uuid.NewString()[:8]
		now := time.Now().UTC().Truncate(time.Second)
		value := pricing.Price{Provider: provider, PricingModel: "gpt-test", Operation: pricing.OperationText, Rates: pricing.Rates{InputToken: 10, CachedInputToken: 1, OutputToken: 20}}
		first, err := repo.Replace(ctx, value, now)
		if err != nil {
			t.Fatal(err)
		}
		value.OutputToken = 30
		second, err := repo.Replace(ctx, value, now.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		other, err := repo.Replace(ctx, pricing.Price{Provider: provider, PricingModel: "gpt-test", Operation: pricing.OperationImage, Rates: pricing.Rates{OutputImage: 7}}, now)
		if err != nil {
			t.Fatal(err)
		}
		if second.ID <= first.ID || second.SupersededAt != nil || second.OutputToken != 30 {
			t.Fatalf("second version = %#v", second)
		}
		active, err := repo.List(ctx, repository.ModelPriceQuery{Provider: provider})
		if err != nil {
			t.Fatal(err)
		}
		if len(active) != 2 || active[0].ID != other.ID || active[1].ID != second.ID {
			t.Fatalf("active rows = %#v", active)
		}
		history, err := repo.List(ctx, repository.ModelPriceQuery{Provider: provider, PricingModel: "gpt-test", IncludeHistory: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(history) != 3 || history[1].ID != second.ID || history[2].ID != first.ID || history[2].SupersededAt == nil || history[2].OutputToken != 20 {
			t.Fatalf("history = %#v", history)
		}
		if err := repo.Supersede(ctx, second.ID, now.Add(2*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := repo.Supersede(ctx, second.ID, now.Add(3*time.Minute)); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("superseding twice err = %v", err)
		}
		if err := repo.Supersede(ctx, first.ID, now); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("superseding history err = %v", err)
		}
		rows, err := repo.ListByIDs(ctx, []uint64{first.ID, second.ID})
		if err != nil || len(rows) != 2 {
			t.Fatalf("rows by id = %#v, %v", rows, err)
		}
		active, err = repo.List(ctx, repository.ModelPriceQuery{Provider: provider})
		if err != nil || len(active) != 1 || active[0].ID != other.ID {
			t.Fatalf("active after delete = %#v, %v", active, err)
		}
	})
}

func TestAuditStoresPriceIDAndCountsCustomPricedRequests(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewAuditRepository(database)
	now := time.Now().UTC()
	custom := audit.Record{EventID: "evt_custom_priced", RequestID: "custom", ClientKeyID: 1, ModelRouteID: 1, Provider: "chatgpt", StatusCode: 200, CreatedAt: now}
	audit.PricingResult{Model: "gpt-test", CostInUSDTicks: 42, PriceID: 5}.Apply(&custom)
	official := audit.Record{EventID: "evt_official_priced", RequestID: "official", ClientKeyID: 1, ModelRouteID: 1, Provider: "grok_build", StatusCode: 200, CreatedAt: now}
	audit.PricingResult{Model: "grok-4.3", CostInUSDTicks: 7}.Apply(&official)
	if err := repo.CreateBatch(ctx, []audit.Record{custom, official}); err != nil {
		t.Fatal(err)
	}
	stored, err := repo.Get(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if stored.PriceID != 5 || stored.PricingVersion != audit.CustomPricingVersion || stored.EstimatedCostInUSDTicks != 42 {
		t.Fatalf("custom audit = %#v", stored)
	}
	summary, err := repo.Summarize(ctx, repository.AuditSummaryQuery{Start: now.Add(-time.Hour), End: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if summary.PricedRequests != 2 || summary.CustomPricedRequests != 1 {
		t.Fatalf("summary = %#v", summary)
	}
}
