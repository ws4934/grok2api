package account

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	cliprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	consoleprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/console"
	webprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/web"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

const (
	acmeProvider       accountdomain.Provider = "acme_chat"
	acmeDeviceProvider accountdomain.Provider = "acme_device"
	acmeCallbackURL                           = "https://callback.example/?code=ok&state=s"
)

func grokTestAdapters() []provider.Adapter {
	return []provider.Adapter{
		cliprovider.NewAdapter(cliprovider.Config{}, nil),
		webprovider.NewAdapter(webprovider.Config{}, nil, nil, nil, nil),
		consoleprovider.NewAdapter(consoleprovider.Config{}, nil, nil, nil),
	}
}

func grokTestRegistry() *provider.Registry {
	return provider.NewRegistry(grokTestAdapters()...)
}

// acmeAdapter is a non-Grok provider with import, state, redirect login and refresh.
type acmeAdapter struct {
	mu           sync.Mutex
	prepared     []string
	exported     []provider.CredentialSeed
	verifiers    []string
	callbacks    []string
	refreshState string
}

type acmeEntry struct {
	Provider string            `json:"provider,omitempty"`
	Name     string            `json:"name"`
	Plan     string            `json:"plan,omitempty"`
	State    map[string]string `json:"state,omitempty"`
}

func (*acmeAdapter) Provider() accountdomain.Provider { return acmeProvider }

func (*acmeAdapter) Definition() provider.Definition {
	return provider.Definition{
		Provider: acmeProvider, Quota: provider.QuotaRemoteWindow,
		Profile:    provider.ProfileSurface{DefaultMaxConcurrent: 2},
		Credential: provider.CredentialSurface{AuthType: accountdomain.AuthTypeOAuth, Import: true, Refresh: true, RedirectOAuth: true},
	}
}

func (*acmeAdapter) ParseImportedCredentials(data []byte) ([]provider.CredentialSeed, error) {
	var entries []acmeEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	seeds := make([]provider.CredentialSeed, 0, len(entries))
	for _, entry := range entries {
		seeds = append(seeds, provider.CredentialSeed{
			Provider: accountdomain.Provider(entry.Provider), Name: entry.Name, SourceKey: "acme:" + entry.Name,
			AccessToken: "access-" + entry.Name, RefreshToken: "refresh-" + entry.Name, Plan: entry.Plan, State: entry.State,
		})
	}
	return seeds, nil
}

func (a *acmeAdapter) MarshalCredentials(values []provider.CredentialSeed) ([]byte, error) {
	a.mu.Lock()
	a.exported = values
	a.mu.Unlock()
	entries := make([]acmeEntry, 0, len(values))
	for _, value := range values {
		entries = append(entries, acmeEntry{Name: value.Name, Plan: value.Plan, State: value.State})
	}
	return json.Marshal(entries)
}

// PrepareImportedCredential resolves identity for every seed, including ones that already carry an access token.
func (a *acmeAdapter) PrepareImportedCredential(_ context.Context, seed provider.CredentialSeed) (provider.CredentialSeed, error) {
	a.mu.Lock()
	a.prepared = append(a.prepared, seed.Name)
	a.mu.Unlock()
	seed.UserID = "user-" + seed.Name
	if seed.Plan == "" {
		seed.Plan = "plus"
	}
	return seed, nil
}

func (*acmeAdapter) StartAuthorization(context.Context) (provider.AuthorizationChallenge, error) {
	return provider.AuthorizationChallenge{Kind: provider.AuthorizationRedirect, State: "pkce-verifier", AuthorizeURL: "https://auth.example/authorize", ExpiresIn: time.Hour}, nil
}

func (a *acmeAdapter) CompleteAuthorization(_ context.Context, challenge provider.AuthorizationChallenge, input string) (provider.CredentialSeed, error) {
	a.mu.Lock()
	a.verifiers = append(a.verifiers, challenge.State)
	a.callbacks = append(a.callbacks, input)
	a.mu.Unlock()
	if input != acmeCallbackURL {
		return provider.CredentialSeed{}, errors.New("callback code rejected")
	}
	return provider.CredentialSeed{
		Name: "redirect", SourceKey: "acme:redirect", UserID: "user-redirect", AccessToken: "access-redirect", RefreshToken: "refresh-redirect",
		ExpiresAt: time.Now().UTC().Add(time.Hour), Plan: "pro", State: map[string]string{"id_token": "id-redirect"},
	}, nil
}

func (a *acmeAdapter) RefreshCredential(context.Context, accountdomain.Credential) (provider.RefreshedCredential, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return provider.RefreshedCredential{EncryptedAccessToken: "rotated-access", ExpiresAt: time.Now().UTC().Add(time.Hour), EncryptedState: a.refreshState}, nil
}

// deviceAdapter replays queued poll outcomes for a device login.
type deviceAdapter struct {
	mu      sync.Mutex
	results []error
	polls   int
}

func (*deviceAdapter) Provider() accountdomain.Provider { return acmeDeviceProvider }

func (*deviceAdapter) Definition() provider.Definition {
	return provider.Definition{Provider: acmeDeviceProvider, Quota: provider.QuotaBilling, Credential: provider.CredentialSurface{AuthType: accountdomain.AuthTypeOAuth, DeviceOAuth: true}}
}

func (*deviceAdapter) StartAuthorization(context.Context) (provider.AuthorizationChallenge, error) {
	return provider.AuthorizationChallenge{Kind: provider.AuthorizationDevice, State: "device-code", UserCode: "ABCD", VerificationURI: "https://verify.example", Interval: 5 * time.Second, ExpiresIn: 30 * time.Minute}, nil
}

func (a *deviceAdapter) CompleteAuthorization(_ context.Context, challenge provider.AuthorizationChallenge, _ string) (provider.CredentialSeed, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.polls++
	if challenge.State != "device-code" {
		return provider.CredentialSeed{}, errors.New("device code was not decrypted")
	}
	if len(a.results) > 0 {
		err := a.results[0]
		a.results = a.results[1:]
		if err != nil {
			return provider.CredentialSeed{}, err
		}
	}
	return provider.CredentialSeed{Name: "device", SourceKey: "device:1", UserID: "device-user", AccessToken: "device-access", RefreshToken: "device-refresh"}, nil
}

type lifecycleFixture struct {
	service  *Service
	accounts *relational.AccountRepository
	sessions *memory.DeviceSessionStore
	cipher   *security.Cipher
}

func newLifecycleFixture(t *testing.T, adapters ...provider.Adapter) lifecycleFixture {
	t.Helper()
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "lifecycle.db"))
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
	accounts := relational.NewAccountRepository(database)
	sessions := memory.NewDeviceSessionStore()
	service := NewService(accounts, relational.NewAuditRepository(database), sessions, memory.NewStickyStore(), provider.NewRegistry(append(grokTestAdapters(), adapters...)...), cipher, nil)
	return lifecycleFixture{service: service, accounts: accounts, sessions: sessions, cipher: cipher}
}

func (f lifecycleFixture) decryptState(t *testing.T, value string) map[string]string {
	t.Helper()
	state, err := f.service.decryptCredentialState(value)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestGenericImportPreparesEverySeedAndRoundTripsStateAndPlan(t *testing.T) {
	ctx := context.Background()
	adapter := &acmeAdapter{}
	fixture := newLifecycleFixture(t, adapter)
	document := []byte(`[{"name":"alpha","state":{"id_token":"id-alpha","device_id":"dev-alpha"}},{"name":"beta","plan":"team"}]`)

	result, err := fixture.service.ImportCredentialDocumentsWithProgress(ctx, acmeProvider, [][]byte{document}, nil, nil)
	if err != nil || result.Created != 2 || result.Failed != 0 {
		t.Fatalf("import result = %#v, err = %v", result, err)
	}
	if len(adapter.prepared) != 2 {
		t.Fatalf("preparer calls = %v, want every seed", adapter.prepared)
	}
	stored := map[string]accountdomain.Credential{}
	for _, id := range result.AccountIDs {
		value, err := fixture.accounts.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		stored[value.Name] = value
	}
	alpha, beta := stored["alpha"], stored["beta"]
	if alpha.Provider != acmeProvider || alpha.MaxConcurrent != 2 || alpha.Plan != "plus" || alpha.UserID != "user-alpha" || alpha.AuthType != accountdomain.AuthTypeOAuth {
		t.Fatalf("alpha = %#v", alpha)
	}
	if alpha.EncryptedState == "" || alpha.EncryptedState == `{"device_id":"dev-alpha","id_token":"id-alpha"}` {
		t.Fatalf("state is not encrypted at rest: %q", alpha.EncryptedState)
	}
	if state := fixture.decryptState(t, alpha.EncryptedState); state["id_token"] != "id-alpha" || state["device_id"] != "dev-alpha" {
		t.Fatalf("alpha state = %#v", state)
	}
	if beta.Plan != "team" || beta.EncryptedState != "" {
		t.Fatalf("beta plan = %q, state = %q", beta.Plan, beta.EncryptedState)
	}

	exported, err := fixture.service.ExportProviderCredentials(ctx, acmeProvider)
	if err != nil || exported.Count != 2 {
		t.Fatalf("export = %#v, err = %v", exported, err)
	}
	for _, seed := range adapter.exported {
		if seed.Provider != acmeProvider || seed.AccessToken != "access-"+seed.Name {
			t.Fatalf("exported seed = %#v", seed)
		}
		if seed.Name == "alpha" && (seed.Plan != "plus" || seed.State["id_token"] != "id-alpha") {
			t.Fatalf("exported alpha = %#v", seed)
		}
	}

	// Re-importing the export keeps the accounts, plan and provider state.
	again, err := fixture.service.ImportCredentialDocumentsWithProgress(ctx, acmeProvider, [][]byte{exported.Data}, nil, nil)
	if err != nil || again.Updated != 2 || again.Created != 0 {
		t.Fatalf("reimport = %#v, err = %v", again, err)
	}
	reloaded, err := fixture.accounts.Get(ctx, alpha.ID)
	if err != nil || reloaded.Plan != "plus" || fixture.decryptState(t, reloaded.EncryptedState)["id_token"] != "id-alpha" {
		t.Fatalf("reimported alpha = %#v, err = %v", reloaded, err)
	}
}

func TestGenericImportRejectsForeignProviderSeeds(t *testing.T) {
	fixture := newLifecycleFixture(t, &acmeAdapter{})
	_, err := fixture.service.ImportCredentialDocumentsWithProgress(context.Background(), acmeProvider, [][]byte{[]byte(`[{"provider":"grok_build","name":"x"}]`)}, nil, nil)
	if !errors.Is(err, ErrInvalidImport) {
		t.Fatalf("err = %v", err)
	}
	if _, err := fixture.service.ImportCredentialDocumentsWithProgress(context.Background(), "unknown", [][]byte{[]byte(`[]`)}, nil, nil); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unknown provider err = %v", err)
	}
}

func TestRedirectAuthorizationIsBoundToAdminAndPersistsAccount(t *testing.T) {
	ctx := context.Background()
	adapter := &acmeAdapter{}
	fixture := newLifecycleFixture(t, adapter)
	now := time.Now().UTC()
	fixture.service.now = func() time.Time { return now }

	started, err := fixture.service.StartAuthorization(ctx, acmeProvider, 7)
	if err != nil {
		t.Fatal(err)
	}
	if started.Kind != "redirect" || started.AuthorizeURL != "https://auth.example/authorize" || started.SessionID == "" || started.ExpiresAt.After(now.Add(redirectAuthSessionTTL)) {
		t.Fatalf("start = %#v", started)
	}
	session, err := fixture.sessions.Get(ctx, started.SessionID, now)
	if err != nil {
		t.Fatal(err)
	}
	if session.AdminID != 7 || session.Provider != acmeProvider || session.EncryptedState == "" || session.EncryptedState == "pkce-verifier" {
		t.Fatalf("session = %#v", session)
	}

	if _, err := fixture.service.CompleteAuthorization(ctx, acmeProvider, started.SessionID, 8, acmeCallbackURL); !errors.Is(err, ErrAuthExpired) {
		t.Fatalf("other admin err = %v", err)
	}
	if _, err := fixture.service.CompleteAuthorization(ctx, accountdomain.ProviderBuild, started.SessionID, 7, acmeCallbackURL); !errors.Is(err, ErrAuthExpired) {
		t.Fatalf("other provider err = %v", err)
	}
	if _, err := fixture.service.CompleteAuthorization(ctx, acmeProvider, started.SessionID, 7, " "); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty callback err = %v", err)
	}
	if _, err := fixture.service.CompleteAuthorization(ctx, acmeProvider, started.SessionID, 7, "https://callback.example/?code=bad"); err == nil || errors.Is(err, ErrAuthExpired) {
		t.Fatalf("rejected callback err = %v", err)
	}
	view, err := fixture.service.CompleteAuthorization(ctx, acmeProvider, started.SessionID, 7, acmeCallbackURL)
	if err != nil {
		t.Fatal(err)
	}
	if view.Credential.Provider != acmeProvider || view.Credential.Plan != "pro" || view.Credential.MaxConcurrent != 2 || fixture.decryptState(t, view.Credential.EncryptedState)["id_token"] != "id-redirect" {
		t.Fatalf("account = %#v", view.Credential)
	}
	if len(adapter.verifiers) != 2 || adapter.verifiers[1] != "pkce-verifier" || adapter.callbacks[1] != acmeCallbackURL {
		t.Fatalf("adapter saw verifiers=%v callbacks=%v", adapter.verifiers, adapter.callbacks)
	}
	if _, err := fixture.service.CompleteAuthorization(ctx, acmeProvider, started.SessionID, 7, acmeCallbackURL); !errors.Is(err, ErrAuthExpired) {
		t.Fatalf("completed session was reusable: %v", err)
	}
}

func TestDeviceAuthorizationKeepsPollPacing(t *testing.T) {
	ctx := context.Background()
	adapter := &deviceAdapter{results: []error{provider.ErrAuthorizationPending, provider.ErrSlowDown, nil}}
	fixture := newLifecycleFixture(t, adapter)
	now := time.Now().UTC()
	fixture.service.now = func() time.Time { return now }

	started, err := fixture.service.StartAuthorization(ctx, acmeDeviceProvider, 3)
	if err != nil {
		t.Fatal(err)
	}
	if started.Kind != "device" || started.UserCode != "ABCD" || started.Interval != 5*time.Second || !started.ExpiresAt.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("start = %#v", started)
	}
	poll := func(advance time.Duration) error {
		now = now.Add(advance)
		_, err := fixture.service.CompleteAuthorization(ctx, acmeDeviceProvider, started.SessionID, 3, "")
		return err
	}
	if err := poll(0); !errors.Is(err, ErrAuthSlowDown) || adapter.polls != 0 {
		t.Fatalf("early poll err = %v, polls = %d", err, adapter.polls)
	}
	if err := poll(5 * time.Second); !errors.Is(err, ErrAuthPending) {
		t.Fatalf("pending err = %v", err)
	}
	if err := poll(5 * time.Second); !errors.Is(err, ErrAuthSlowDown) || adapter.polls != 2 {
		t.Fatalf("slow_down err = %v, polls = %d", err, adapter.polls)
	}
	// slow_down widens the interval to 10s.
	if err := poll(5 * time.Second); !errors.Is(err, ErrAuthSlowDown) || adapter.polls != 2 {
		t.Fatalf("widened interval err = %v, polls = %d", err, adapter.polls)
	}
	if err := poll(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	stored, err := fixture.accounts.ListEnabled(ctx, acmeDeviceProvider)
	if err != nil || len(stored) != 1 || stored[0].MaxConcurrent != accountdomain.DefaultMaxConcurrent {
		t.Fatalf("stored = %#v, err = %v", stored, err)
	}

	adapter.results = []error{provider.ErrAuthorizationDenied}
	denied, err := fixture.service.StartAuthorization(ctx, acmeDeviceProvider, 3)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(5 * time.Second)
	if _, err := fixture.service.CompleteAuthorization(ctx, acmeDeviceProvider, denied.SessionID, 3, ""); !errors.Is(err, ErrAuthExpired) {
		t.Fatalf("denied err = %v", err)
	}
	if _, err := fixture.sessions.Get(ctx, denied.SessionID, now); err == nil {
		t.Fatal("denied session was kept")
	}
}

func TestStartAuthorizationRejectsProvidersWithoutInteractiveLogin(t *testing.T) {
	fixture := newLifecycleFixture(t)
	if _, err := fixture.service.StartAuthorization(context.Background(), accountdomain.ProviderWeb, 1); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v", err)
	}
}

func TestCredentialRefreshPersistsProviderState(t *testing.T) {
	ctx := context.Background()
	adapter := &acmeAdapter{refreshState: "sealed-state-1"}
	fixture := newLifecycleFixture(t, adapter)
	now := time.Now().UTC()
	fixture.service.now = func() time.Time { return now }
	created, _, err := fixture.service.persistSeed(ctx, provider.CredentialSeed{Provider: acmeProvider, Name: "refresh", SourceKey: "acme:refresh", AccessToken: "a", RefreshToken: "r", State: map[string]string{"id_token": "old"}})
	if err != nil {
		t.Fatal(err)
	}
	refreshed, err := fixture.service.EnsureCredential(ctx, created, true)
	if err != nil || refreshed.EncryptedState != "sealed-state-1" || refreshed.EncryptedAccessToken != "rotated-access" {
		t.Fatalf("refreshed = %#v, err = %v", refreshed, err)
	}
	adapter.refreshState = ""
	now = now.Add(time.Minute)
	refreshed, err = fixture.service.EnsureCredential(ctx, refreshed, true)
	if err != nil || refreshed.EncryptedState != "sealed-state-1" {
		t.Fatalf("empty refreshed state replaced the stored state: %#v, err = %v", refreshed, err)
	}
	material, err := fixture.accounts.GetCredentialMaterial(ctx, created.ID, acmeProvider)
	if err != nil || material.EncryptedState != "sealed-state-1" {
		t.Fatalf("material = %#v, err = %v", material, err)
	}
}

func TestCredentialSchedulerCoversEveryRefreshableProvider(t *testing.T) {
	ctx := context.Background()
	fixture := newLifecycleFixture(t, &acmeAdapter{})
	if got := fixture.service.refreshableProviders(); len(got) != 2 || got[0] != accountdomain.ProviderBuild || got[1] != acmeProvider {
		t.Fatalf("refreshable providers = %v", got)
	}
	now := time.Now().UTC()
	ids := map[accountdomain.Provider]uint64{}
	for _, providerValue := range []accountdomain.Provider{accountdomain.ProviderBuild, acmeProvider, accountdomain.ProviderWeb} {
		value, _, err := fixture.accounts.UpsertByIdentity(ctx, accountdomain.Credential{
			Provider: providerValue, AuthType: accountdomain.AuthTypeOAuth, Name: string(providerValue), SourceKey: "due-" + string(providerValue),
			EncryptedAccessToken: "a", EncryptedRefreshToken: "r", ExpiresAt: now, Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids[providerValue] = value.ID
	}
	due, err := fixture.accounts.ListDueCredentialRefreshIDs(ctx, fixture.service.refreshableProviders(), now, 10)
	if err != nil || len(due) != 2 || !containsID(due, ids[accountdomain.ProviderBuild]) || !containsID(due, ids[acmeProvider]) {
		t.Fatalf("due = %v, ids = %v, err = %v", due, ids, err)
	}
	critical, err := fixture.accounts.ListCriticalCredentialRefreshIDs(ctx, fixture.service.refreshableProviders(), now, now.Add(time.Minute), 10)
	if err != nil || len(critical) != 2 || containsID(critical, ids[accountdomain.ProviderWeb]) {
		t.Fatalf("critical = %v, err = %v", critical, err)
	}
	if next, err := fixture.accounts.NextCredentialRefreshDueAt(ctx, nil); err != nil || next != nil {
		t.Fatalf("empty provider set next = %v, err = %v", next, err)
	}
}

func TestCleanupAndSummaryCoverEveryRegisteredProvider(t *testing.T) {
	ctx := context.Background()
	fixture := newLifecycleFixture(t, &acmeAdapter{})
	created, _, err := fixture.accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: acmeProvider, AuthType: accountdomain.AuthTypeOAuth, Name: "disabled", SourceKey: "acme:disabled",
		EncryptedAccessToken: "a", AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	created.Enabled = false
	if _, err := fixture.accounts.Update(ctx, created); err != nil {
		t.Fatal(err)
	}
	summary, err := fixture.service.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, providerValue := range []accountdomain.Provider{accountdomain.ProviderBuild, accountdomain.ProviderWeb, accountdomain.ProviderConsole, acmeProvider} {
		if _, ok := summary.Providers[string(providerValue)]; !ok {
			t.Fatalf("summary providers = %#v, missing %s", summary.Providers, providerValue)
		}
	}
	result, err := fixture.service.CleanupAccounts(ctx, acmeProvider, []CleanupStatus{CleanupStatusDisabled}, nil)
	if err != nil || result.Deleted != 1 {
		t.Fatalf("cleanup = %#v, err = %v", result, err)
	}
	if _, err := fixture.service.CleanupAccounts(ctx, "unregistered", []CleanupStatus{CleanupStatusDisabled}, nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unregistered cleanup err = %v", err)
	}
}

// identityAdapter reports identity and plan for a non-Grok remote-window provider.
type identityAdapter struct{ calls int }

func (*identityAdapter) Provider() accountdomain.Provider { return "acme_identity" }

func (*identityAdapter) Definition() provider.Definition {
	return provider.Definition{Provider: "acme_identity", Quota: provider.QuotaRemoteWindow, Credential: provider.CredentialSurface{AuthType: accountdomain.AuthTypeOAuth}}
}

func (a *identityAdapter) SyncAccountIdentity(context.Context, accountdomain.Credential) (provider.AccountIdentity, error) {
	a.calls++
	return provider.AccountIdentity{Email: "id@example.com", UserID: "id-user", Plan: "team"}, nil
}

func (*identityAdapter) SyncQuota(context.Context, accountdomain.Credential) (provider.QuotaSnapshot, error) {
	return provider.QuotaSnapshot{SyncedAt: time.Now().UTC()}, nil
}

func (*identityAdapter) SyncQuotaMode(context.Context, accountdomain.Credential, string) (accountdomain.QuotaWindow, error) {
	return accountdomain.QuotaWindow{}, errors.New("not used")
}

func TestIdentitySyncRunsForAnyProviderWithIdentityAdapter(t *testing.T) {
	ctx := context.Background()
	adapter := &identityAdapter{}
	fixture := newLifecycleFixture(t, adapter)
	created, _, err := fixture.service.persistSeed(ctx, provider.CredentialSeed{Provider: "acme_identity", Name: "anon", SourceKey: "acme:anon", AccessToken: "a"})
	if err != nil {
		t.Fatal(err)
	}
	// The quota follow-up fills the missing identity, including the plan.
	if _, err := fixture.service.RefreshQuota(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := fixture.accounts.Get(ctx, created.ID)
	if err != nil || adapter.calls != 1 || stored.Plan != "team" || stored.UserID != "id-user" || stored.Email != "id@example.com" {
		t.Fatalf("stored = %#v, calls = %d, err = %v", stored, adapter.calls, err)
	}
	// A complete identity is not fetched again.
	if err := fixture.service.SyncAccountIdentity(ctx, created.ID); err != nil || adapter.calls != 1 {
		t.Fatalf("calls = %d, err = %v", adapter.calls, err)
	}
}

func containsID(values []uint64, id uint64) bool {
	for _, value := range values {
		if value == id {
			return true
		}
	}
	return false
}
