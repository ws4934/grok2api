package inference

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

// The expected bodies were captured from /v1/models before it stopped loading
// per-route account counts; only route availability may decide the list.
func TestListModelsOutputFromDatabase(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "models.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	models := relational.NewModelRepository(database)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(models.UpsertDiscovered(ctx, account.ProviderBuild, []string{"grok-4", "grok-4-fast", "grok-unserved"}))
	must(models.UpsertDiscovered(ctx, account.ProviderChatGPT, []string{"gpt-5", "gpt-5-mini"}))
	create := func(provider account.Provider, name string, status account.AuthStatus, upstream ...string) uint64 {
		t.Helper()
		value, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: provider, Name: name, SourceKey: name, EncryptedAccessToken: "token", Enabled: true, AuthStatus: status})
		must(err)
		must(models.ReplaceAccountCapabilities(ctx, value.ID, upstream, value.CreatedAt))
		return value.ID
	}
	superBuild := create(account.ProviderBuild, "build-super", account.AuthStatusActive, "grok-4", "grok-4-fast")
	must(accounts.SaveBilling(ctx, account.Billing{AccountID: superBuild, MonthlyLimit: 100}))
	freeBuild := create(account.ProviderBuild, "build-free", account.AuthStatusActive, "grok-4")
	create(account.ProviderChatGPT, "gpt", account.AuthStatusActive, "gpt-5")
	create(account.ProviderChatGPT, "gpt-reauth", account.AuthStatusReauthRequired, "gpt-5-mini")
	_, err = models.Create(ctx, modeldomain.Route{PublicID: "team-grok", Provider: account.ProviderBuild, UpstreamModel: "grok-4", Capability: modeldomain.CapabilityResponses, Enabled: true}, []uint64{freeBuild})
	must(err)
	_, err = models.Create(ctx, modeldomain.Route{PublicID: "off-grok", Provider: account.ProviderBuild, UpstreamModel: "grok-4", Capability: modeldomain.CapabilityResponses}, nil)
	must(err)

	handler := NewHandler(nil, modelapp.NewService(models, accounts, nil, nil), 1<<20)
	list := func(key *clientkeydomain.Key) string {
		t.Helper()
		router := gin.New()
		v1 := router.Group("/v1", func(c *gin.Context) {
			if key != nil {
				c.Set(middleware.ClientKey, *key)
			}
		})
		handler.Register(v1)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
		}
		var body struct {
			Object string           `json:"object"`
			Data   []map[string]any `json:"data"`
		}
		must(json.Unmarshal(recorder.Body.Bytes(), &body))
		for _, item := range body.Data {
			if created, ok := item["created"].(float64); !ok || created <= 0 {
				t.Fatalf("created = %#v", item["created"])
			}
			item["created"] = 0
		}
		normalized, err := json.Marshal(body)
		must(err)
		return string(normalized)
	}
	cases := []struct {
		name string
		key  *clientkeydomain.Key
		want string
	}{
		{name: "no key", want: `{"object":"list","data":[{"created":0,"id":"grok-4","object":"model","owned_by":"grok2api"},{"created":0,"id":"grok-4-fast","object":"model","owned_by":"grok2api"},{"created":0,"id":"team-grok","object":"model","owned_by":"grok2api"},{"created":0,"id":"gpt-5","object":"model","owned_by":"grok2api"}]}`},
		{name: "all providers with aliases", key: &clientkeydomain.Key{ProviderScope: clientkeydomain.AllProviders(), TierScope: clientkeydomain.TierScopeAll, AllowModelAliases: true}, want: `{"object":"list","data":[{"created":0,"id":"grok-4","object":"model","owned_by":"grok2api"},{"created":0,"id":"grok-4-fast","object":"model","owned_by":"grok2api"},{"created":0,"id":"team-grok","object":"model","owned_by":"grok2api"},{"created":0,"id":"gpt-5","object":"model","owned_by":"grok2api"}]}`},
		{name: "chatgpt only", key: &clientkeydomain.Key{ProviderScope: clientkeydomain.ProviderScopeOf(account.ProviderChatGPT), TierScope: clientkeydomain.TierScopeAll}, want: `{"object":"list","data":[{"created":0,"id":"gpt-5","object":"model","owned_by":"grok2api"}]}`},
		{name: "super tier", key: &clientkeydomain.Key{ProviderScope: clientkeydomain.AllProviders(), TierScope: clientkeydomain.TierScopeSuper}, want: `{"object":"list","data":[{"created":0,"id":"grok-4","object":"model","owned_by":"grok2api"},{"created":0,"id":"grok-4-fast","object":"model","owned_by":"grok2api"}]}`},
	}
	for _, tc := range cases {
		if got := list(tc.key); got != tc.want {
			t.Errorf("%s: /v1/models = %s\nwant %s", tc.name, got, tc.want)
		}
	}
}
