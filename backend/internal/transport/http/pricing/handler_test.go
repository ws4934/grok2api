package pricing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	pricingapp "github.com/chenyme/grok2api/backend/internal/application/pricing"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	pricingdomain "github.com/chenyme/grok2api/backend/internal/domain/pricing"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/gin-gonic/gin"
)

func newTestRouter(t *testing.T) (*gin.Engine, *pricingapp.Service) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "pricing-handler.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := pricingapp.NewService(ctx, relational.NewModelPriceRepository(database), func(value account.Provider) bool { return value == "chatgpt" }, func(_ account.Provider, upstream string) string { return upstream })
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	NewHandler(service).Register(router.Group("/api/admin/v1"))
	return router, service
}

func call(t *testing.T, router *gin.Engine, method, path, body string) (int, map[string]any) {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(method, path, strings.NewReader(body)))
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("%s %s: %s", method, path, recorder.Body.String())
	}
	return recorder.Code, payload
}

func TestModelPriceAPI(t *testing.T) {
	router, _ := newTestRouter(t)
	status, payload := call(t, router, http.MethodPut, "/api/admin/v1/model-prices",
		`{"provider":"chatgpt","pricingModel":"gpt-5","operation":"text","inputTokenUSDPerMillion":"1.25","outputTokenUSDPerMillion":10}`)
	if status != http.StatusOK {
		t.Fatalf("put status = %d, %v", status, payload)
	}
	first := payload["data"].(map[string]any)
	if first["inputTokenUSDPerMillion"] != "1.25" || first["cachedInputTokenUSDPerMillion"] != "1.25" || first["outputTokenUSDPerMillion"] != "10" ||
		first["active"] != true || first["operation"] != "text" || first["outputImageUSD"] != nil {
		t.Fatalf("created price = %v", first)
	}
	status, payload = call(t, router, http.MethodPut, "/api/admin/v1/model-prices",
		`{"provider":"chatgpt","pricingModel":"gpt-5","operation":"text","inputTokenUSDPerMillion":"2","cachedInputTokenUSDPerMillion":0.2,"outputTokenUSDPerMillion":"8"}`)
	if status != http.StatusOK {
		t.Fatalf("edit status = %d, %v", status, payload)
	}
	second := payload["data"].(map[string]any)
	if second["id"] == first["id"] || second["cachedInputTokenUSDPerMillion"] != "0.2" {
		t.Fatalf("edited price = %v", second)
	}

	_, payload = call(t, router, http.MethodGet, "/api/admin/v1/model-prices?provider=chatgpt", "")
	if items := payload["data"].(map[string]any)["items"].([]any); len(items) != 1 || items[0].(map[string]any)["id"] != second["id"] {
		t.Fatalf("active list = %v", items)
	}
	_, payload = call(t, router, http.MethodGet, "/api/admin/v1/model-prices?provider=chatgpt&model=gpt-5&includeHistory=true", "")
	items := payload["data"].(map[string]any)["items"].([]any)
	if len(items) != 2 || items[1].(map[string]any)["active"] != false || items[1].(map[string]any)["supersededAt"] == nil {
		t.Fatalf("history = %v", items)
	}

	for name, body := range map[string]string{
		"unknown provider": `{"provider":"grok_build","pricingModel":"m","operation":"image","outputImageUSD":"0.04"}`,
		"imprecise":        `{"provider":"chatgpt","pricingModel":"m","operation":"text","inputTokenUSDPerMillion":"0.00001","outputTokenUSDPerMillion":"1"}`,
		"irrelevant":       `{"provider":"chatgpt","pricingModel":"m","operation":"image","outputImageUSD":"0.04","characterUSDPerMillion":"1"}`,
		"missing":          `{"provider":"chatgpt","pricingModel":"m","operation":"video"}`,
		"negative number":  `{"provider":"chatgpt","pricingModel":"m","operation":"image","outputImageUSD":-0.04}`,
	} {
		if status, payload := call(t, router, http.MethodPut, "/api/admin/v1/model-prices", body); status != http.StatusBadRequest || payload["error"].(map[string]any)["code"] != "invalidModelPrice" {
			t.Fatalf("%s: status = %d, %v", name, status, payload)
		}
	}
	if status, _ := call(t, router, http.MethodPut, "/api/admin/v1/model-prices", `{"provider":`); status != http.StatusBadRequest {
		t.Fatalf("malformed status = %d", status)
	}

	if status, payload := call(t, router, http.MethodDelete, "/api/admin/v1/model-prices/"+first["id"].(string), ""); status != http.StatusNotFound {
		t.Fatalf("delete superseded = %d, %v", status, payload)
	}
	if status, payload := call(t, router, http.MethodDelete, "/api/admin/v1/model-prices/"+second["id"].(string), ""); status != http.StatusOK || payload["data"].(map[string]any)["deleted"] != true {
		t.Fatalf("delete = %d, %v", status, payload)
	}
	if status, _ := call(t, router, http.MethodDelete, "/api/admin/v1/model-prices/abc", ""); status != http.StatusBadRequest {
		t.Fatalf("invalid id status = %d", status)
	}
	_, payload = call(t, router, http.MethodGet, "/api/admin/v1/model-prices", "")
	if items := payload["data"].(map[string]any)["items"].([]any); len(items) != 0 {
		t.Fatalf("active after delete = %v", items)
	}
}

func TestRoutePriceResponse(t *testing.T) {
	_, service := newTestRouter(t)
	route := modeldomain.Route{Provider: "chatgpt", UpstreamModel: "gpt-image-1", Capability: modeldomain.CapabilityImageEdit}
	encoded, _ := json.Marshal(NewRoutePriceResponse(service.RoutePrice(route)))
	if string(encoded) != `{"source":"none","pricingModel":"gpt-image-1","operation":"image_edit"}` {
		t.Fatalf("unpriced = %s", encoded)
	}
	price, err := service.Put(context.Background(), pricingapp.Input{Provider: "chatgpt", PricingModel: "gpt-image-1", Operation: "image_edit", Rates: map[pricingdomain.Component]string{
		pricingdomain.OutputImage: "0.042", pricingdomain.InputImage: "0.0000000001",
	}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(NewRoutePriceResponse(service.RoutePrice(route)))
	want := `{"source":"custom","id":"` + strconv.FormatUint(price.ID, 10) + `","pricingModel":"gpt-image-1","operation":"image_edit","inputImageUSD":"0.0000000001","outputImageUSD":"0.042"}`
	if string(encoded) != want {
		t.Fatalf("custom = %s, want %s", encoded, want)
	}
}

// IDs are signed BIGINTs: one with the top bit set is invalid input, not a
// value database/sql rejects with a 500.
func TestIDParsersRejectTopBitIDs(t *testing.T) {
	router, _ := newTestRouter(t)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, "/api/admin/v1/model-prices/9223372036854775808", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}
