package clientkey

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/gin-gonic/gin"
)

func newHandlerTestRouter(t *testing.T) (*gin.Engine, *clientkeyapp.Service) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "client-key-handler.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	service := clientkeyapp.NewService(relational.NewClientKeyRepository(database), nil, nil, 60, 5, cipher)
	router := gin.New()
	NewHandler(service).Register(router.Group("/api"))
	return router, service
}

func TestCreateDistinguishesOmittedLimitsFromExplicitZero(t *testing.T) {
	ctx := context.Background()
	router, service := newHandlerTestRouter(t)

	assertCreate := func(body string) {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/api/client-keys", bytes.NewBufferString(body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusCreated {
			t.Fatalf("create response = %d %s", response.Code, response.Body.String())
		}
	}
	assertCreate(`{"name":"defaults"}`)
	assertCreate(`{"name":"unlimited","rpmLimit":0,"maxConcurrent":0}`)
	assertCreate(`{"name":"free-pool","accountPool":"free"}`)
	assertCreate(`{"name":"mixed-scope","providerScope":["grok_build","grok_web"],"tierScope":["free","super"]}`)

	defaults, total, err := service.List(ctx, 1, 20, "defaults", clientkeyapp.ListFilter{})
	if err != nil || total != 1 || len(defaults) != 1 {
		t.Fatalf("default key list = %#v, total = %d, err = %v", defaults, total, err)
	}
	if defaults[0].RPMLimit != 60 || defaults[0].MaxConcurrent != 5 {
		t.Fatalf("omitted limits = rpm %d, concurrency %d", defaults[0].RPMLimit, defaults[0].MaxConcurrent)
	}
	unlimited, total, err := service.List(ctx, 1, 20, "unlimited", clientkeyapp.ListFilter{})
	if err != nil || total != 1 || len(unlimited) != 1 {
		t.Fatalf("unlimited key list = %#v, total = %d, err = %v", unlimited, total, err)
	}
	if unlimited[0].RPMLimit != 0 || unlimited[0].MaxConcurrent != 0 {
		t.Fatalf("explicit zero limits = rpm %d, concurrency %d", unlimited[0].RPMLimit, unlimited[0].MaxConcurrent)
	}
	freePool, total, err := service.List(ctx, 1, 20, "free-pool", clientkeyapp.ListFilter{})
	if err != nil || total != 1 || len(freePool) != 1 || !freePool[0].ProviderScope.All || freePool[0].TierScope != 1 {
		t.Fatalf("free-pool key list = %#v, total = %d, err = %v", freePool, total, err)
	}
	mixedScope, total, err := service.List(ctx, 1, 20, "mixed-scope", clientkeyapp.ListFilter{})
	if err != nil || total != 1 || len(mixedScope) != 1 || !reflect.DeepEqual(mixedScope[0].ProviderScope.Values(), []string{"grok_build", "grok_web"}) || mixedScope[0].TierScope != 3 {
		t.Fatalf("mixed-scope key list = %#v, total = %d, err = %v", mixedScope, total, err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/client-keys", bytes.NewBufferString(`{"name":"invalid-pool","accountPool":"unknown"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid pool response = %d %s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "/api/client-keys", bytes.NewBufferString(`{"name":"ambiguous","accountPool":"free","tierScope":["super"]}`))
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("ambiguous scope response = %d %s", response.Code, response.Body.String())
	}
	for _, body := range []string{
		`{"name":"empty-providers","providerScope":[]}`,
		`{"name":"empty-tiers","tierScope":[]}`,
		`{"name":"empty-legacy-pool","accountPool":""}`,
	} {
		request = httptest.NewRequest(http.MethodPost, "/api/client-keys", bytes.NewBufferString(body))
		request.Header.Set("Content-Type", "application/json")
		response = httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("empty scope response = %d %s", response.Code, response.Body.String())
		}
	}
}

func TestProviderScopeWireFormatAcceptsAllAndExplicitProviders(t *testing.T) {
	router, _ := newHandlerTestRouter(t)
	send := func(method, path, body string, wantStatus int) []byte {
		t.Helper()
		request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != wantStatus {
			t.Fatalf("%s %s = %d %s", method, path, response.Code, response.Body.String())
		}
		return response.Body.Bytes()
	}
	var created struct {
		Data struct {
			Key keyResponse `json:"key"`
		} `json:"data"`
	}
	if err := json.Unmarshal(send(http.MethodPost, "/api/client-keys", `{"name":"all","providerScope":["all"]}`, http.StatusCreated), &created); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(created.Data.Key.ProviderScope, []string{"all"}) {
		t.Fatalf("created provider scope = %#v", created.Data.Key.ProviderScope)
	}
	path := "/api/client-keys/" + strconv.FormatUint(created.Data.Key.ID, 10)
	var updated struct {
		Data keyResponse `json:"data"`
	}
	if err := json.Unmarshal(send(http.MethodPatch, path, `{"providerScope":["grok_web","chatgpt"]}`, http.StatusOK), &updated); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(updated.Data.ProviderScope, []string{"chatgpt", "grok_web"}) {
		t.Fatalf("updated provider scope = %#v", updated.Data.ProviderScope)
	}
	if err := json.Unmarshal(send(http.MethodPatch, path, `{"providerScope":["all"]}`, http.StatusOK), &updated); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(updated.Data.ProviderScope, []string{"all"}) {
		t.Fatalf("provider scope after reset = %#v", updated.Data.ProviderScope)
	}
	invalid := string(send(http.MethodPatch, path, `{"providerScope":["all","grok_web"]}`, http.StatusBadRequest))
	if strings.Contains(invalid, "grok_build") || strings.Contains(invalid, "grok_console") {
		t.Fatalf("provider scope error lists a closed provider set: %s", invalid)
	}
}

// IDs are signed BIGINTs: one with the top bit set is invalid input, not a
// value database/sql rejects with a 500.
