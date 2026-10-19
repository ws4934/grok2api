package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

// syncingModels records the account model syncs a changed catalog queues.
type syncingModels struct {
	*relational.ModelRepository
	mu     sync.Mutex
	synced []uint64
}

func (m *syncingModels) SyncAccount(_ context.Context, accountID uint64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.synced = append(m.synced, accountID)
	return 0, nil
}

func (m *syncingModels) syncedAccounts() []uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]uint64(nil), m.synced...)
}

type responseFixture struct {
	service  *Service
	models   *syncingModels
	key      clientkey.Key
	accounts []account.Credential
}

// newResponseFixture serves model "grok-4.6" from n Build accounts (priority descending).
func newResponseFixture(t *testing.T, adapter provider.Adapter, n int) responseFixture {
	t.Helper()
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "response-fix.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo := relational.NewAccountRepository(database)
	models := &syncingModels{ModelRepository: relational.NewModelRepository(database)}
	auditRepo := relational.NewAuditRepository(database)
	if err := models.UpsertDiscovered(ctx, account.ProviderBuild, []string{"grok-4.6"}); err != nil {
		t.Fatal(err)
	}
	fixture := responseFixture{models: models}
	for index := range n {
		name := fmt.Sprintf("fix-%d", index)
		credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderBuild, Name: name, SourceKey: name,
			EncryptedAccessToken: "access-old", EncryptedRefreshToken: "refresh-old", ExpiresAt: time.Now().Add(time.Hour),
			Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 300 - index, MaxConcurrent: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := models.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-4.6"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		fixture.accounts = append(fixture.accounts, credential)
	}
	fixture.key, err = relational.NewClientKeyRepository(database).Create(ctx, clientkey.Key{
		Name: "fix-key", Prefix: "fixkey", SecretHash: strings.Repeat("f", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 120, MaxConcurrent: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	fixture.service = NewService(models, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, relational.NewResponseRepository(database), 2)
	return fixture
}

// trackedBody reports whether the gateway closed it.
type trackedBody struct {
	io.Reader
	closed atomic.Bool
}

func (b *trackedBody) Close() error { b.closed.Store(true); return nil }

// replayAdapter rejects the original token with 401 and answers the refreshed
// one with replay.
type replayAdapter struct {
	replay func() (*provider.Response, error)
}

func (a *replayAdapter) Provider() account.Provider { return account.ProviderBuild }
func (a *replayAdapter) Definition() provider.Definition {
	return testConversationDefinition(account.ProviderBuild)
}
func (a *replayAdapter) ForwardResponse(_ context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	if request.Credential.EncryptedAccessToken == "access-old" {
		return &provider.Response{StatusCode: http.StatusUnauthorized, Status: "401 Unauthorized", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"expired"}`))}, nil
	}
	return a.replay()
}
func (a *replayAdapter) RefreshCredential(context.Context, account.Credential) (provider.RefreshedCredential, error) {
	return provider.RefreshedCredential{EncryptedAccessToken: "access-new", EncryptedRefreshToken: "refresh-new", ExpiresAt: time.Now().Add(6 * time.Hour)}, nil
}

// A replay after a 401 refresh that fails must still close the body the
// adapter returned with its error.
func TestFailedReplayAfterRefreshClosesTheResponse(t *testing.T) {
	body := &trackedBody{Reader: strings.NewReader("partial")}
	adapter := &replayAdapter{replay: func() (*provider.Response, error) {
		return &provider.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, errors.New("stream reset")
	}}
	f := newResponseFixture(t, adapter, 1)
	if _, err := f.service.CreateResponse(context.Background(), Input{
		RequestID: "req-replay-error", ClientKey: f.key, PublicModel: "grok-4.6", Body: []byte(`{"model":"grok-4.6","input":"hi"}`),
	}); err == nil {
		t.Fatal("a failed replay was delivered")
	}
	if !body.closed.Load() {
		t.Fatal("the body returned with the replay error was not closed")
	}
}

// A replayed response that reports a changed model catalog queues the
// account's model sync, like a first response does.
func TestReplayAfterRefreshQueuesAChangedCatalog(t *testing.T) {
	adapter := &replayAdapter{replay: func() (*provider.Response, error) {
		return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok")), ModelCatalogChanged: true}, nil
	}}
	f := newResponseFixture(t, adapter, 1)
	result, err := f.service.CreateResponse(context.Background(), Input{
		RequestID: "req-replay-catalog", ClientKey: f.key, PublicModel: "grok-4.6", Body: []byte(`{"model":"grok-4.6","input":"hi"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(result.Body)
	result.Finalize(Usage{}, "", "")
	_ = result.Body.Close()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && len(f.models.syncedAccounts()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if synced := f.models.syncedAccounts(); len(synced) != 1 || synced[0] != f.accounts[0].ID {
		t.Fatalf("model syncs = %v", synced)
	}
}

// A content-safety 403 reaches the client unchanged even when an earlier
// degraded answer is held as the fail_open fallback.
func TestTerminalForbiddenIsNotReplacedByTheFailOpenFallback(t *testing.T) {
	content := strings.Repeat("fallback", 24)
	adapter := &scriptedBuildAdapter{responses: map[uint64][]scriptedBuildResponse{}}
	f := newResponseFixture(t, adapter, 2)
	adapter.responses[f.accounts[0].ID] = []scriptedBuildResponse{{status: http.StatusOK, body: sse(
		`data: {"choices":[{"delta":{"content":"`+content+`"}}]}`,
		`data: {"usage":{"completion_tokens":48,"completion_tokens_details":{"reasoning_tokens":0}}}`,
		"data: [DONE]",
	)}}
	adapter.responses[f.accounts[1].ID] = []scriptedBuildResponse{{status: http.StatusForbidden, body: `{"error":{"code":"forbidden","message":"Content violates usage guidelines"}}`}}
	f.service.UpdateQualityRetry(QualityRetryRuntime{Enabled: true, MaxAttempts: 6, MinOutputTokens: 32, OnExhausted: qualityRetryFailOpen, HoldTimeout: time.Second})

	result, err := f.service.CreateChatCompletion(context.Background(), Input{
		RequestID: "req-terminal-403", ClientKey: f.key, PublicModel: "grok-4.6", Streaming: true,
		Body: []byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"hi"}],"stream":true}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(result.Body)
	result.Finalize(Usage{}, "", "")
	_ = result.Body.Close()
	if result.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "usage guidelines") || strings.Contains(string(body), content) {
		t.Fatalf("status %d, body %s", result.StatusCode, body)
	}
}
