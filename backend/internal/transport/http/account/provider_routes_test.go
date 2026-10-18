package account

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	admindomain "github.com/chenyme/grok2api/backend/internal/domain/admin"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

const (
	routeRedirectProvider accountdomain.Provider = "acme_chat"
	routeDeviceProvider   accountdomain.Provider = "acme_device"
	routeCallbackURL                             = "https://callback.example/?code=ok"
)

type routeRedirectAdapter struct{}

func (routeRedirectAdapter) Provider() accountdomain.Provider { return routeRedirectProvider }

func (routeRedirectAdapter) Definition() provider.Definition {
	return provider.Definition{
		Provider: routeRedirectProvider, Quota: provider.QuotaRemoteWindow,
		Credential: provider.CredentialSurface{AuthType: accountdomain.AuthTypeOAuth, Import: true, RedirectOAuth: true},
	}
}

func (routeRedirectAdapter) ParseImportedCredentials(data []byte) ([]provider.CredentialSeed, error) {
	var names []string
	if err := json.Unmarshal(data, &names); err != nil {
		return nil, err
	}
	seeds := make([]provider.CredentialSeed, 0, len(names))
	for _, name := range names {
		seeds = append(seeds, provider.CredentialSeed{Name: name, SourceKey: "route:" + name, AccessToken: "access-" + name, Plan: "plus"})
	}
	return seeds, nil
}

func (routeRedirectAdapter) MarshalCredentials([]provider.CredentialSeed) ([]byte, error) {
	return nil, nil
}

func (routeRedirectAdapter) StartAuthorization(context.Context) (provider.AuthorizationChallenge, error) {
	return provider.AuthorizationChallenge{Kind: provider.AuthorizationRedirect, State: "verifier", AuthorizeURL: "https://auth.example/authorize"}, nil
}

func (routeRedirectAdapter) CompleteAuthorization(_ context.Context, challenge provider.AuthorizationChallenge, input string) (provider.CredentialSeed, error) {
	if challenge.State != "verifier" || input != routeCallbackURL {
		return provider.CredentialSeed{}, provider.ErrAuthorizationDenied
	}
	return provider.CredentialSeed{Name: "redirect", SourceKey: "route:redirect", AccessToken: "access-redirect", Plan: "pro"}, nil
}

func (routeRedirectAdapter) SyncQuota(context.Context, accountdomain.Credential) (provider.QuotaSnapshot, error) {
	now := time.Now().UTC()
	return provider.QuotaSnapshot{SyncedAt: now, Windows: []accountdomain.QuotaWindow{{Mode: "reason", Remaining: 3, Total: 5, SyncedAt: &now}}}, nil
}

func (routeRedirectAdapter) SyncQuotaMode(context.Context, accountdomain.Credential, string) (accountdomain.QuotaWindow, error) {
	return accountdomain.QuotaWindow{}, errors.New("not used")
}

type routeDeviceAdapter struct {
	mu      sync.Mutex
	results []error
}

func (*routeDeviceAdapter) Provider() accountdomain.Provider { return routeDeviceProvider }

func (*routeDeviceAdapter) Definition() provider.Definition {
	return provider.Definition{Provider: routeDeviceProvider, Quota: provider.QuotaBilling, Credential: provider.CredentialSurface{AuthType: accountdomain.AuthTypeOAuth, DeviceOAuth: true}}
}

func (*routeDeviceAdapter) StartAuthorization(context.Context) (provider.AuthorizationChallenge, error) {
	return provider.AuthorizationChallenge{Kind: provider.AuthorizationDevice, State: "device", UserCode: "WXYZ", VerificationURI: "https://verify.example", ExpiresIn: time.Minute}, nil
}

func (a *routeDeviceAdapter) CompleteAuthorization(context.Context, provider.AuthorizationChallenge, string) (provider.CredentialSeed, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	err := a.results[0]
	a.results = a.results[1:]
	return provider.CredentialSeed{}, err
}

func newProviderRouteServer(t *testing.T, adminID uint64) (*httptest.Server, *relational.AccountRepository, *routeDeviceAdapter) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "provider-routes.db"))
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
	repository := relational.NewAccountRepository(database)
	device := &routeDeviceAdapter{}
	service := accountapp.NewService(repository, relational.NewAuditRepository(database), memory.NewDeviceSessionStore(), nil, provider.NewRegistry(routeRedirectAdapter{}, device), cipher, nil)
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set(middleware.AdminKey, admindomain.Admin{ID: adminID}) })
	NewHandler(service, nil).Register(router.Group("/api/admin/v1"))
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server, repository, device
}

func postRoute(t *testing.T, server *httptest.Server, path, contentType string, body io.Reader) (int, string) {
	t.Helper()
	response, err := server.Client().Post(server.URL+"/api/admin/v1"+path, contentType, body)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(data)
}

func TestProviderRoutesRejectUnregisteredProvider(t *testing.T) {
	server, _, _ := newProviderRouteServer(t, 1)
	for _, path := range []string{"/providers/nope/accounts/import", "/providers/nope/accounts/auth/start", "/providers/nope/accounts/auth/abc/complete", "/providers/nope/accounts/refresh-quotas"} {
		status, body := postRoute(t, server, path, "application/json", strings.NewReader(`{}`))
		if status != http.StatusNotFound || !strings.Contains(body, `"code":"providerNotFound"`) {
			t.Fatalf("%s: status = %d, body = %s", path, status, body)
		}
	}
}

func TestProviderImportRouteImportsWithProviderCodec(t *testing.T) {
	server, repository, _ := newProviderRouteServer(t, 1)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("files", "accounts.json")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte(`["alpha","beta"]`))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	status, response := postRoute(t, server, "/providers/acme_chat/accounts/import", writer.FormDataContentType(), &body)
	if status != http.StatusOK || !strings.Contains(response, `{"created":2,"updated":0,"skipped":0,"failed":0,"synced":0,"syncFailed":0}`) {
		t.Fatalf("status = %d, body = %s", status, response)
	}
	stored, err := repository.ListEnabled(context.Background(), routeRedirectProvider)
	if err != nil || len(stored) != 2 || stored[0].Plan != "plus" {
		t.Fatalf("stored = %#v, err = %v", stored, err)
	}
	// Device-only providers declare no import surface.
	status, response = postRoute(t, server, "/providers/acme_device/accounts/import", writer.FormDataContentType(), strings.NewReader(""))
	if status != http.StatusConflict {
		t.Fatalf("device import status = %d, body = %s", status, response)
	}
}

func TestProviderRedirectAuthRoutes(t *testing.T) {
	server, _, _ := newProviderRouteServer(t, 1)
	status, body := postRoute(t, server, "/providers/acme_chat/accounts/auth/start", "application/json", nil)
	if status != http.StatusCreated {
		t.Fatalf("start status = %d, body = %s", status, body)
	}
	var started struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &started); err != nil {
		t.Fatal(err)
	}
	sessionID, _ := started.Data["sessionId"].(string)
	if sessionID == "" || started.Data["kind"] != "redirect" || started.Data["authorizeUrl"] != "https://auth.example/authorize" || started.Data["expiresAt"] == nil {
		t.Fatalf("start payload = %s", body)
	}
	if _, ok := started.Data["userCode"]; ok {
		t.Fatalf("redirect start leaked device fields: %s", body)
	}
	if strings.Contains(body, "verifier") {
		t.Fatalf("challenge state leaked: %s", body)
	}
	complete := "/providers/acme_chat/accounts/auth/" + sessionID + "/complete"
	if status, body := postRoute(t, server, complete, "application/json", strings.NewReader(`{}`)); status != http.StatusBadRequest || !strings.Contains(body, `"code":"invalidRequest"`) {
		t.Fatalf("missing callback status = %d, body = %s", status, body)
	}
	status, body = postRoute(t, server, complete, "application/json", strings.NewReader(`{"callbackUrl":"`+routeCallbackURL+`"}`))
	if status != http.StatusOK || !strings.Contains(body, `"status":"completed"`) || !strings.Contains(body, `"plan":"pro"`) || !strings.Contains(body, `"provider":"acme_chat"`) {
		t.Fatalf("complete status = %d, body = %s", status, body)
	}
	if status, body := postRoute(t, server, complete, "application/json", strings.NewReader(`{"callbackUrl":"`+routeCallbackURL+`"}`)); status != http.StatusGone || !strings.Contains(body, `"code":"deviceLoginExpired"`) {
		t.Fatalf("reused session status = %d, body = %s", status, body)
	}
}

func TestProviderDeviceAuthRoutesKeepPollShapes(t *testing.T) {
	server, _, device := newProviderRouteServer(t, 1)
	device.results = []error{provider.ErrAuthorizationPending, provider.ErrSlowDown, provider.ErrAuthorizationDenied}
	status, body := postRoute(t, server, "/providers/acme_device/accounts/auth/start", "application/json", nil)
	if status != http.StatusCreated || !strings.Contains(body, `"kind":"device"`) || !strings.Contains(body, `"userCode":"WXYZ"`) || !strings.Contains(body, `"verificationUri":"https://verify.example"`) {
		t.Fatalf("start status = %d, body = %s", status, body)
	}
	var started struct {
		Data struct {
			SessionID string `json:"sessionId"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &started); err != nil {
		t.Fatal(err)
	}
	complete := "/providers/acme_device/accounts/auth/" + started.Data.SessionID + "/complete"
	if status, body := postRoute(t, server, complete, "application/json", nil); status != http.StatusAccepted || !strings.Contains(body, `"status":"pending"`) {
		t.Fatalf("pending status = %d, body = %s", status, body)
	}
	if status, body := postRoute(t, server, complete, "application/json", nil); status != http.StatusTooManyRequests || !strings.Contains(body, `"code":"devicePollTooFast"`) {
		t.Fatalf("slow_down status = %d, body = %s", status, body)
	}

	// Sessions are bound to the administrator that started them.
	other, _, _ := newProviderRouteServer(t, 2)
	if status, body := postRoute(t, other, complete, "application/json", nil); status != http.StatusGone {
		t.Fatalf("foreign session status = %d, body = %s", status, body)
	}
}

func TestProviderRefreshQuotasRouteSyncsWindowProvider(t *testing.T) {
	server, repository, _ := newProviderRouteServer(t, 1)
	created, _, err := repository.UpsertByIdentity(context.Background(), accountdomain.Credential{
		Provider: routeRedirectProvider, AuthType: accountdomain.AuthTypeOAuth, Name: "quota", SourceKey: "route:quota",
		EncryptedAccessToken: "a", AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	status, body := postRoute(t, server, "/providers/acme_chat/accounts/refresh-quotas", "application/json", nil)
	if status != http.StatusOK || !strings.Contains(body, "event: complete\n") || !strings.Contains(body, `{"succeeded":1,"failed":0}`) {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	windows, err := repository.GetQuotaWindows(context.Background(), []uint64{created.ID})
	if err != nil || len(windows[created.ID]) != 1 || windows[created.ID][0].Mode != "reason" {
		t.Fatalf("windows = %#v, err = %v", windows, err)
	}
}

func TestBatchRefreshTokensUsesProviderCapability(t *testing.T) {
	server, repository, _ := newProviderRouteServer(t, 1)
	created, _, err := repository.UpsertByIdentity(context.Background(), accountdomain.Credential{
		Provider: routeRedirectProvider, AuthType: accountdomain.AuthTypeOAuth, Name: "tokens", SourceKey: "route:tokens",
		EncryptedAccessToken: "a", AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := `{"ids":["` + uintString(created.ID) + `"],"provider":"acme_chat"}`
	if status, body := postRoute(t, server, "/accounts/batch/refresh-tokens", "application/json", strings.NewReader(payload)); status != http.StatusBadRequest || !strings.Contains(body, "不支持凭据刷新") {
		t.Fatalf("non-refreshable status = %d, body = %s", status, body)
	}
	if status, body := postRoute(t, server, "/accounts/batch/refresh-quotas", "application/json", strings.NewReader(payload)); status != http.StatusOK || !strings.Contains(body, `"succeeded":1`) {
		t.Fatalf("batch quota status = %d, body = %s", status, body)
	}
	if status, body := postRoute(t, server, "/accounts/batch/refresh-quotas", "application/json", strings.NewReader(`{"ids":["1"],"provider":"nope"}`)); status != http.StatusBadRequest || !strings.Contains(body, `"code":"invalidProvider"`) {
		t.Fatalf("unknown provider status = %d, body = %s", status, body)
	}
}

func uintString(value uint64) string {
	data, _ := json.Marshal(value)
	return string(data)
}
