package gateway

import (
	"context"
	"io"
	"path/filepath"
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
)

// originImageAdapter records the asset origin a provider would save with.
type originImageAdapter struct {
	staleTokenImageAdapter
	origins []mediadomain.Origin
}

func (a *originImageAdapter) GenerateImage(ctx context.Context, request provider.ImageGenerationRequest) (*provider.Response, error) {
	origin, _ := mediadomain.OriginFrom(ctx)
	a.mu.Lock()
	a.origins = append(a.origins, origin)
	a.mu.Unlock()
	return a.staleTokenImageAdapter.GenerateImage(ctx, request)
}

func TestGenerateImageAttachesAssetOrigin(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "image-origin.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo, modelRepo, auditRepo := relational.NewAccountRepository(database), relational.NewModelRepository(database), relational.NewAuditRepository(database)
	now := time.Now().UTC()
	credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth,
		Name: "origin-image", SourceKey: "origin-image", EncryptedAccessToken: "fresh", EncryptedRefreshToken: "refresh",
		ExpiresAt: now.Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.UpsertRoutes(ctx, []modeldomain.Route{{
		PublicID: "image-origin", Provider: account.ProviderBuild, UpstreamModel: "image-origin",
		Capability: modeldomain.CapabilityImage, Enabled: true,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"image-origin"}, now); err != nil {
		t.Fatal(err)
	}
	adapter := &originImageAdapter{}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, relational.NewResponseRepository(database), 2)

	result, err := service.GenerateImage(ctx, ImageGenerationInput{
		RequestID: "req-image-origin", ClientKey: clientkey.Key{ID: 6, Name: "origin-key"},
		PublicModel: "image-origin", Prompt: "a lighthouse", Count: 1, ResponseFormat: "url",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(result.Body)
	result.Finalize(Usage{}, "", "")
	_ = result.Body.Close()
	want := mediadomain.Origin{
		Provider: string(account.ProviderBuild), Model: modeldomain.ExternalPublicID(account.ProviderBuild, "image-origin"),
		UpstreamModel: modeldomain.DisplayUpstreamModel(account.ProviderBuild, "image-origin"), ClientKeyID: 6, ClientKeyName: "origin-key",
		AccountID: credential.ID, AccountName: "origin-image", Operation: "image", RequestID: "req-image-origin", Prompt: "a lighthouse",
	}
	if len(adapter.origins) != 1 || adapter.origins[0] != want {
		t.Fatalf("origins = %#v, want %#v", adapter.origins, want)
	}
}
