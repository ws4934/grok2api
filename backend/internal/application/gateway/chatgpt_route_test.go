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
	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/chatgpt"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

// chatgptRouteAdapter keeps the native ChatGPT declaration, routing metadata
// and capability overlay, and replaces only the upstream calls.
type chatgptRouteAdapter struct {
	*chatgpt.Adapter
	mu    sync.Mutex
	calls []string
}

func (a *chatgptRouteAdapter) record(call string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, call)
}

func (a *chatgptRouteAdapter) ListModels(context.Context, account.Credential) ([]string, error) {
	return []string{"gpt-5"}, nil
}

func (a *chatgptRouteAdapter) ForwardResponse(_ context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	a.record(request.Operation + ":" + request.Model)
	body := `{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`
	return &provider.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func (a *chatgptRouteAdapter) GenerateImage(_ context.Context, request provider.ImageGenerationRequest) (*provider.Response, error) {
	a.record("image:" + request.Model)
	return &provider.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"created":1,"data":[{"b64_json":"AA=="}]}`))}, nil
}

func TestChatGPTRoutesServeAllScopeKeyAfterModelSync(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "chatgpt.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo, modelRepo, auditRepo := relational.NewAccountRepository(database), relational.NewModelRepository(database), relational.NewAuditRepository(database)
	cipher := testCipher(t)
	adapter := &chatgptRouteAdapter{Adapter: chatgpt.NewAdapter(chatgpt.Config{ImageConversationModel: "gpt-5"}, nil, cipher, nil)}
	registry := provider.NewRegistry(adapter)
	if err := registry.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.UpsertRoutes(ctx, chatgpt.Routes()); err != nil {
		t.Fatal(err)
	}
	token, err := cipher.Encrypt("AT")
	if err != nil {
		t.Fatal(err)
	}
	credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderChatGPT, AuthType: account.AuthTypeOAuth, Name: "gpt", UserID: "user-1", SourceKey: "gpt",
		EncryptedAccessToken: token, ExpiresAt: time.Now().Add(24 * time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, cipher, nil)
	if _, err := modelapp.NewService(modelRepo, accountRepo, accountService, registry).SyncAccount(ctx, credential.ID); err != nil {
		t.Fatal(err)
	}
	key, err := relational.NewClientKeyRepository(database).Create(ctx, clientkey.Key{
		Name: "all-key", Prefix: "allkey", SecretHash: strings.Repeat("a", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 60, MaxConcurrent: 4, ProviderScope: clientkey.AllProviders(),
	})
	if err != nil {
		t.Fatal(err)
	}
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, relational.NewResponseRepository(database), 1)

	chat, err := service.CreateChatCompletion(ctx, Input{RequestID: "req-chat", ClientKey: key, PublicModel: "gpt-5", Body: []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`)})
	if err != nil {
		t.Fatalf("chat route: %v", err)
	}
	_, _ = io.ReadAll(chat.Body)
	chat.Finalize(Usage{}, "", "")
	_ = chat.Body.Close()

	image, err := service.GenerateImage(ctx, ImageGenerationInput{RequestID: "req-image", ClientKey: key, PublicModel: "gpt-image-2", Prompt: "a fox", Count: 1})
	if err != nil {
		t.Fatalf("image route: %v", err)
	}
	_, _ = io.ReadAll(image.Body)
	image.Finalize(Usage{}, "", "")
	_ = image.Body.Close()

	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if strings.Join(adapter.calls, ",") != "chat:gpt-5,image:gpt-image-2" {
		t.Fatalf("upstream calls = %v", adapter.calls)
	}
}
