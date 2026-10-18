package model

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	pricingapp "github.com/chenyme/grok2api/backend/internal/application/pricing"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
)

func TestModelResponseCarriesEffectivePrice(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "model-price.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	prices, err := pricingapp.NewService(ctx, relational.NewModelPriceRepository(database), func(account.Provider) bool { return true }, func(_ account.Provider, upstream string) string { return upstream })
	if err != nil {
		t.Fatal(err)
	}
	route := modeldomain.Route{ID: 1, PublicID: "Build/grok-4.3", Provider: account.ProviderBuild, UpstreamModel: "grok-4.3", Capability: modeldomain.CapabilityResponses}
	if price := NewHandler(nil).routePrice(route); price != nil {
		t.Fatalf("price without a price book = %#v", price)
	}
	item := newModelResponse(route)
	item.Price = NewHandler(nil).WithPrices(prices).routePrice(route)
	encoded, _ := json.Marshal(item.Price)
	if string(encoded) != `{"source":"official","pricingModel":"grok-4.3","operation":"text","inputTokenUSDPerMillion":"1.25","cachedInputTokenUSDPerMillion":"0.2","outputTokenUSDPerMillion":"2.5"}` {
		t.Fatalf("official route price = %s", encoded)
	}
}
