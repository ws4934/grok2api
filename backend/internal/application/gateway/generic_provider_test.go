package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	neterrorpkg "github.com/chenyme/grok2api/backend/internal/pkg/neterror"
)

const genericProvider account.Provider = "generic"

// genericAdapter declares no gateway policy and implements no routing hook, so
// every gateway decision for it must take the generic path.
type genericAdapter struct {
	mu        sync.Mutex
	forbidden uint64
	requests  []provider.ResponseResourceRequest
	origins   []mediadomain.Origin
}

func (a *genericAdapter) Provider() account.Provider { return genericProvider }

func (a *genericAdapter) Definition() provider.Definition {
	return provider.Definition{
		Provider: genericProvider, ModelNamespace: genericProvider.ModelNamespace(), ModelCatalog: provider.ModelCatalogRemote,
		Conversation: provider.ConversationSurface{Responses: true, ChatCompletions: true, Messages: true},
		Inference:    provider.InferencePolicy{Usage: provider.UsageUpstream},
	}
}

func (a *genericAdapter) ForwardResponse(ctx context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	origin, _ := mediadomain.OriginFrom(ctx)
	a.mu.Lock()
	a.requests = append(a.requests, request)
	a.origins = append(a.origins, origin)
	a.mu.Unlock()
	if request.Credential.ID == a.forbidden {
		// Request-scoped for Build, but only content-safety 403s are terminal by default.
		return &provider.Response{StatusCode: http.StatusForbidden, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"invalid-argument","error":"bad tool"}`))}, nil
	}
	return &provider.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"resp-generic"}`))}, nil
}

func TestGenericProviderConversationTakesDefaultPath(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "generic-provider.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo, modelRepo, auditRepo := relational.NewAccountRepository(database), relational.NewModelRepository(database), relational.NewAuditRepository(database)
	if err := modelRepo.UpsertDiscovered(ctx, genericProvider, []string{"generic-model"}); err != nil {
		t.Fatal(err)
	}
	credentials := make([]account.Credential, 0, 2)
	for index, name := range []string{"generic-a", "generic-b"} {
		credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
			Provider: genericProvider, AuthType: account.AuthTypeSSO, Name: name, SourceKey: name, EncryptedAccessToken: "encrypted",
			Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 100 - index, MaxConcurrent: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"generic-model"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		credentials = append(credentials, credential)
	}
	adapter := &genericAdapter{forbidden: credentials[0].ID}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, relational.NewResponseRepository(database), 2)
	key := clientkey.Key{ID: 9, Name: "generic-key"}
	body := []byte(`{"model":"generic-model","input":"hello"}`)

	if _, err := service.CreateResponse(ctx, Input{RequestID: "req-previous", ClientKey: key, PublicModel: "generic-model", Body: body, PreviousResponseID: "resp-old"}); !errors.Is(err, ErrResponseStateUnsupported) {
		t.Fatalf("previous_response_id without stored or stateless Responses = %v", err)
	}

	result, err := service.CreateResponse(ctx, Input{RequestID: "req-generic", ClientKey: key, PublicModel: "generic-model", Body: body, PromptCacheKey: "client-cache"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(result.Body)
	result.Finalize(Usage{}, "", "")
	_ = result.Body.Close()
	if result.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the second account's 200", result.StatusCode)
	}

	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if len(adapter.requests) != 2 || adapter.requests[0].Credential.ID != credentials[0].ID || adapter.requests[1].Credential.ID != credentials[1].ID {
		t.Fatalf("a request-scoped 403 must rotate accounts on the generic path: %#v", adapter.requests)
	}
	for _, request := range adapter.requests {
		if request.PromptCacheKey != "client-cache" || request.ReasoningReplayKey != "" {
			t.Fatalf("generic provider got a derived session identity: cache=%q replay=%q", request.PromptCacheKey, request.ReasoningReplayKey)
		}
	}
	want := mediadomain.Origin{
		Provider: string(genericProvider), Model: modeldomain.ExternalPublicID(genericProvider, "generic/generic-model"),
		UpstreamModel: modeldomain.DisplayUpstreamModel(genericProvider, "generic-model"), ClientKeyID: 9, ClientKeyName: "generic-key",
		AccountID: credentials[1].ID, AccountName: "generic-b", Operation: "responses", RequestID: "req-generic",
	}
	if adapter.origins[1] != want {
		t.Fatalf("asset origin = %#v, want %#v", adapter.origins[1], want)
	}
}

func TestGenericProviderSelectorUsesSnapshotRouting(t *testing.T) {
	selector := NewSelector(nil, nil, nil, provider.NewRegistry(&genericAdapter{}), 0, 0, 0)
	selector.UpdateExcludeBuildBotFlaggedFromScheduling(true)
	candidate := account.RoutingCandidate{
		Credential:           account.Credential{ID: 1, Provider: genericProvider, WebTier: account.WebTierBasic, BuildBotFlagSource: 1},
		Billing:              &account.Billing{PlanName: "Free"},
		ModelCapabilityKnown: true,
	}
	if tier := selector.accountTier(genericProvider, candidate); tier != clientkey.AccountTierUnknown {
		t.Fatalf("tier = %v, want unknown", tier)
	}
	if selector.accountScopeAllowsCandidate(genericProvider, clientkey.AccountScope{Providers: clientkey.AllProviders(), Tiers: clientkey.TierScopeFree}, candidate) {
		t.Fatal("an unclassified account entered a Free-only key scope")
	}
	if !selector.accountScopeAllowsCandidate(genericProvider, clientkey.AccountScope{Providers: clientkey.AllProviders(), Tiers: clientkey.TierScopeAll}, candidate) {
		t.Fatal("an unrestricted key scope rejected the account")
	}
	if mode := selector.effectiveQuotaMode(candidate, account.QuotaModeWebImageEdit); mode != account.QuotaModeWebImageEdit {
		t.Fatalf("quota mode = %q, want the route mode", mode)
	}
	if selector.staticModel(genericProvider, account.QuotaModeWebImagePro) || selector.staticCatalog(genericProvider) {
		t.Fatal("generic provider has no static models")
	}
	if selector.candidateSupportsModel(genericProvider, "generic-model", "", candidate) {
		t.Fatal("the capability snapshot must decide model support")
	}
	filtered, err := selector.applyBuildBotFlaggedFilter(context.Background(), genericProvider, []account.RoutingCandidate{candidate})
	if err != nil || len(filtered) != 1 {
		t.Fatalf("generic candidates must not be filtered as a scheduling risk: %#v, %v", filtered, err)
	}
}

func TestGenericProviderVideoAndFailureDefaults(t *testing.T) {
	service := &Service{providers: provider.NewRegistry(&genericAdapter{})}
	if err := service.validateVideoRouteParameters(genericProvider, provider.VideoOperationGenerate, "any-video", "1080p", true, false, 9, 30); err != nil {
		t.Fatalf("generation without a validator = %v", err)
	}
	routes := []modeldomain.Route{{ID: 1, Provider: genericProvider, UpstreamModel: "any-video"}}
	if _, err := service.routesForVideoOperation(routes, provider.VideoOperationEdit); !errors.Is(err, ErrVideoOperationUnsupported) {
		t.Fatalf("edit without a validator = %v", err)
	}
	if mode := service.providers.VideoQuotaMode(genericProvider, account.QuotaModeWebVideo, ""); mode != account.QuotaModeWebVideo {
		t.Fatalf("video quota mode = %q", mode)
	}

	policy := service.providers.Failure(genericProvider)
	if policy != (provider.FailurePolicy{}) || service.providers.Inference(genericProvider) != (provider.InferencePolicy{Usage: provider.UsageUpstream}) {
		t.Fatalf("generic provider policies = %#v / %#v", policy, service.providers.Inference(genericProvider))
	}
	if isTerminalRequestForbidden(policy, &UpstreamFailure{HTTPStatus: http.StatusForbidden, RequestScopedForbidden: true, UpstreamCode: "unauthorized:dpop-required"}) {
		t.Fatal("request-scoped 403 must not be terminal by default")
	}
	if !isTerminalRequestForbidden(policy, &UpstreamFailure{HTTPStatus: http.StatusForbidden, SafetyRejection: true}) {
		t.Fatal("safety rejections stay terminal for every provider")
	}
	veto := &provider.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"X-Should-Retry": {"false"}}}
	if isRetryableResponse(veto, policy) {
		t.Fatal("the upstream retry veto must hold by default")
	}
	if !isRetryableResponse(&provider.Response{StatusCode: 525, Header: make(http.Header)}, policy) {
		t.Fatal("525 is retryable by default")
	}
	if isRetryableResponse(&provider.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header), ReasoningRecoveryFailed: true}, policy) {
		t.Fatal("a 400 is not retryable by default")
	}
	if !isRetryableTransportFailure(policy, neterrorpkg.ErrUpstreamStreamIdleTimeout) || !isRetryableTransportFailure(policy, responseHeaderTimeoutTestError{}) {
		t.Fatal("transport failures keep failover by default")
	}
}
