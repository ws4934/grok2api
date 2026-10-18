package gateway

import (
	"context"
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
