package audit

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	auditapp "github.com/chenyme/grok2api/backend/internal/application/audit"
	pricingapp "github.com/chenyme/grok2api/backend/internal/application/pricing"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
	pricingdomain "github.com/chenyme/grok2api/backend/internal/domain/pricing"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/gin-gonic/gin"
)

func TestAuditBreakdownReconstructsCustomPriceVersion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "audit-pricing.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	prices, err := pricingapp.NewService(ctx, relational.NewModelPriceRepository(database), func(account.Provider) bool { return true }, nil)
	if err != nil {
		t.Fatal(err)
	}
	put := func(input string) pricingdomain.Price {
		price, err := prices.Put(ctx, pricingapp.Input{Provider: "chatgpt", PricingModel: "gpt-test", Operation: "text", Rates: map[pricingdomain.Component]string{
			pricingdomain.InputToken: input, pricingdomain.CachedInputToken: "0.5", pricingdomain.OutputToken: "8",
		}})
		if err != nil {
			t.Fatal(err)
		}
		return price
	}
	old := put("2")
	quote, _ := prices.QuoteText("chatgpt", "gpt-test", 1000, 200, 300, 1000)
	// A later edit must not change how the earlier audit is explained.
	put("3")
	repo := relational.NewAuditRepository(database)
	now := time.Now().UTC()
	record := auditdomain.Record{EventID: "evt_custom_breakdown", RequestID: "custom", ClientKeyID: 1, ModelRouteID: 1, Provider: "chatgpt", StatusCode: 200,
		InputTokens: 1000, CachedInputTokens: 200, OutputTokens: 300, CreatedAt: now}
	quote.Apply(&record)
	official := auditdomain.Record{EventID: "evt_official_breakdown", RequestID: "official", ClientKeyID: 1, ModelRouteID: 1, Provider: "grok_build", StatusCode: 200,
		InputTokens: 100, CachedInputTokens: 20, OutputTokens: 50, CreatedAt: now.Add(-time.Second)}
	officialQuote, _ := prices.QuoteText("grok_build", "grok-build-0.1", 100, 20, 50, 100)
	officialQuote.Apply(&official)
	if err := repo.CreateBatch(ctx, []auditdomain.Record{record, official}); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	NewHandler(auditapp.NewService(repo, slog.Default(), 8, 4, time.Second)).WithPrices(prices).Register(router.Group("/api/admin/v1"))

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/v1/request-audits/1", nil))
	var detail struct {
		Data struct {
			Audit auditResponse `json:"audit"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &detail); err != nil || recorder.Code != http.StatusOK {
		t.Fatalf("detail = %d %s", recorder.Code, recorder.Body.String())
	}
	billing := detail.Data.Audit.Billing
	want := int64(800*20_000 + 200*5_000 + 300*80_000)
	if detail.Data.Audit.PriceID != old.ID || detail.Data.Audit.PricingVersion != auditdomain.CustomPricingVersion || billing == nil ||
		billing.Source != "custom" || billing.Method != "custom_rates" || billing.TotalInUSDTicks != want || len(billing.Components) != 3 ||
		billing.Components[0].UnitPriceInUSDTicks != 20_000 || billing.Components[0].Quantity != 800 {
		t.Fatalf("custom audit = %#v billing %#v", detail.Data.Audit, billing)
	}

	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/v1/request-audits?pagination=cursor", nil))
	var list struct {
		Data struct {
			Items []auditResponse `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &list); err != nil || len(list.Data.Items) != 2 {
		t.Fatalf("list = %d %s", recorder.Code, recorder.Body.String())
	}
	if list.Data.Items[0].Billing.Method != "custom_rates" || list.Data.Items[1].Billing.Source != "official" || list.Data.Items[1].Billing.Method != "official_rates" {
		t.Fatalf("list billing = %#v / %#v", list.Data.Items[0].Billing, list.Data.Items[1].Billing)
	}

	// Without the price row the stored estimate is still shown.
	missing := newAuditResponse(record, nil)
	if missing.Billing.Source != "custom" || missing.Billing.Method != "stored_estimate" || len(missing.Billing.Components) != 0 || missing.Billing.TotalInUSDTicks != want {
		t.Fatalf("missing price billing = %#v", missing.Billing)
	}

	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/v1/request-audits/summary?refresh=1", nil))
	var summary struct {
		Data struct {
			Pricing pricingResponse `json:"pricing"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Data.Pricing.Source != auditdomain.OfficialPricingSource || summary.Data.Pricing.PricedRequests != 2 || summary.Data.Pricing.CustomPricedRequests != 1 {
		t.Fatalf("summary pricing = %#v", summary.Data.Pricing)
	}
}
