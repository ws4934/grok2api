package gateway

import (
	"context"
	"fmt"
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
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

// staleTokenImageAdapter rejects the stored token with 401 until the gateway forces a refresh.
type staleTokenImageAdapter struct {
	mu    sync.Mutex
	calls []string
}

func (a *staleTokenImageAdapter) Provider() account.Provider { return account.ProviderBuild }

func (a *staleTokenImageAdapter) Definition() provider.Definition {
	return withGrokPolicy(provider.Definition{
		Provider: account.ProviderBuild, ModelNamespace: account.ProviderBuild.ModelNamespace(),
		Credential: provider.CredentialSurface{AuthType: account.AuthTypeOAuth, Refresh: true},
		Media:      provider.MediaSurface{ImageGeneration: true},
	})
}

func (a *staleTokenImageAdapter) RefreshCredential(context.Context, account.Credential) (provider.RefreshedCredential, error) {
	return provider.RefreshedCredential{EncryptedAccessToken: "fresh", EncryptedRefreshToken: "refresh", ExpiresAt: time.Now().UTC().Add(time.Hour)}, nil
}

func (a *staleTokenImageAdapter) GenerateImage(_ context.Context, request provider.ImageGenerationRequest) (*provider.Response, error) {
	a.mu.Lock()
	a.calls = append(a.calls, request.Credential.EncryptedAccessToken)
	a.mu.Unlock()
	if request.Credential.EncryptedAccessToken != "fresh" {
		return &provider.Response{StatusCode: http.StatusUnauthorized, Status: "401 Unauthorized", Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"expired"}}`))}, nil
	}
	return &provider.Response{
		StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"created":1,"data":[{"url":"https://example.com/a.png"}]}`)),
	}, nil
}

func TestGenerateImageRefreshesOAuthCredentialAfterUnauthorized(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "image-oauth-401.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	now := time.Now().UTC()
	credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth,
		Name: "stale-image", SourceKey: "stale-image", EncryptedAccessToken: "stale", EncryptedRefreshToken: "refresh",
		ExpiresAt: now.Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.UpsertRoutes(ctx, []modeldomain.Route{{
		PublicID: "image-oauth-401", Provider: account.ProviderBuild, UpstreamModel: "image-oauth-401",
		Capability: modeldomain.CapabilityImage, Enabled: true,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"image-oauth-401"}, now); err != nil {
		t.Fatal(err)
	}
	adapter := &staleTokenImageAdapter{}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, relational.NewResponseRepository(database), 2)

	result, err := service.GenerateImage(ctx, ImageGenerationInput{
		RequestID: "req-image-oauth-401", ClientKey: clientkey.Key{ID: 1, Name: "image-key"},
		PublicModel: "image-oauth-401", Prompt: "test", Count: 1, ResponseFormat: "url",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(result.Body)
	result.Finalize(Usage{}, "", "")
	_ = result.Body.Close()
	if result.StatusCode != http.StatusOK || len(adapter.calls) != 2 || adapter.calls[0] != "stale" || adapter.calls[1] != "fresh" {
		t.Fatalf("status = %d, calls = %#v", result.StatusCode, adapter.calls)
	}
}

// downloadDeniedError mimics a provider failure on an upstream HTTP response.
type downloadDeniedError struct{}

func (downloadDeniedError) Error() string { return "upstream HTTP 403 at /backend-api/estuary/content" }
func (downloadDeniedError) UpstreamDiagnostic() (string, provider.DiagnosticResponse) {
	return "https://chatgpt.com/backend-api/estuary/content", provider.DiagnosticResponse{StatusCode: 403, Status: "403 Forbidden", Body: []byte(`{"detail":"File stream access denied."}`)}
}

type deniedImageAdapter struct{ staleTokenImageAdapter }

func (a *deniedImageAdapter) GenerateImage(context.Context, provider.ImageGenerationRequest) (*provider.Response, error) {
	return nil, downloadDeniedError{}
}

func TestImageFailureAuditKeepsUpstreamDiagnostic(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "image-diagnostic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: "diag", SourceKey: "diag",
		EncryptedAccessToken: "fresh", EncryptedRefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.UpsertRoutes(ctx, []modeldomain.Route{{PublicID: "image-diag", Provider: account.ProviderBuild, UpstreamModel: "image-diag", Capability: modeldomain.CapabilityImage, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"image-diag"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	registry := provider.NewRegistry(&deniedImageAdapter{})
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, relational.NewResponseRepository(database), 1)

	if _, err := service.GenerateImage(ctx, ImageGenerationInput{RequestID: "req-diag", ClientKey: clientkey.Key{ID: 1, Name: "k"}, PublicModel: "image-diag", Prompt: "fox", Count: 1, Method: http.MethodPost, Path: "/v1/images/generations"}); err == nil {
		t.Fatal("expected the image request to fail")
	}
	records, _, err := auditRepo.List(ctx, 0, 10)
	if err != nil || len(records) != 1 {
		t.Fatalf("records = %d, err = %v", len(records), err)
	}
	stored, err := auditRepo.Get(ctx, records[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Attempts) != 1 {
		t.Fatalf("attempts = %#v", stored.Attempts)
	}
	attempt := stored.Attempts[0]
	if attempt.Source != audit.AttemptSourceUpstreamHTTP || attempt.UpstreamStatusCode == nil || *attempt.UpstreamStatusCode != 403 ||
		attempt.UpstreamURL != "https://chatgpt.com/backend-api/estuary/content" || !strings.Contains(string(attempt.ResponseBody), "File stream access denied") {
		t.Fatalf("attempt = %#v", attempt)
	}
}

// A credential rejected through an error fails over to the next account.
func TestImageSSORejectionFailsOver(t *testing.T) {
	calls := map[uint64]int{}
	service, model := consoleSSOPairService(t, modeldomain.CapabilityImage)
	result, err := service.executeImage(context.Background(), "req-image-sso", clientkey.Key{ID: 1}, model, "draw", audit.OperationImage, modeldomain.CapabilityImage,
		func(account.Provider) bool { return true },
		func(_ context.Context, _ account.Provider, credential account.Credential, _ string) (*provider.Response, error) {
			calls[credential.ID]++
			if len(calls) == 1 {
				return nil, fmt.Errorf("image: %w", provider.ErrUnauthorized)
			}
			return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"data":[]}`))}, nil
		}, provider.ImageRequestOptions{Count: 1}, "", "", nil, false)
	if err != nil || len(calls) != 2 {
		t.Fatalf("no failover after an SSO rejection: calls = %v, err = %v", calls, err)
	}
	result.Finalize(Usage{}, "", "")
	_ = result.Body.Close()
}
