package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
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

// A persistent transport failure retries the same account once, not until
// maxAttempts runs out.
func TestVoiceTransportFailureRetriesTheAccountOnce(t *testing.T) {
	service, model := consoleSSOPairService(t, modeldomain.CapabilitySTT)
	service.UpdateMaxAttempts(999) // config default
	calls := 0
	accounts := map[uint64]int{}
	started := time.Now()
	_, err := service.executeVoice(context.Background(), "req-voice-loop", clientkey.Key{ID: 1}, model, audit.OperationSTT, modeldomain.CapabilitySTT, true, nil, "", "", nil, func(account.Provider) bool {
		return true
	}, func(_ context.Context, _ account.Provider, credential account.Credential, _ string) (voiceExecutionResult, error) {
		calls++
		accounts[credential.ID]++
		return voiceExecutionResult{}, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	})
	if calls > 2 || err == nil {
		t.Fatalf("upstream calls = %d (%v) after %s, err = %v", calls, accounts, time.Since(started), err)
	}
}

type refreshVoiceAdapter struct{}

func (refreshVoiceAdapter) Provider() account.Provider { return account.ProviderConsole }
func (refreshVoiceAdapter) Definition() provider.Definition {
	definition := statelessConsoleAdapter{}.Definition()
	definition.Credential.Refresh = true // like ChatGPT (OAuth refresh + TTS/STT)
	return definition
}
func (refreshVoiceAdapter) ForwardResponse(context.Context, provider.ResponseResourceRequest) (*provider.Response, error) {
	return nil, errors.New("unused")
}

// A retried 5xx on account A, then the last allowed attempt fails
// EnsureCredential on B: the client gets A's real failure, audited on A.
func TestVoiceReturnsTheRetriedFailureAfterACredentialFailure(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "voice-stale.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	const voiceModel = "voice-stale-stt"
	if err := modelRepo.UpsertRoutes(ctx, []modeldomain.Route{{PublicID: voiceModel, Provider: account.ProviderConsole, UpstreamModel: voiceModel, Capability: modeldomain.CapabilitySTT, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	expired := time.Now().UTC().Add(-time.Hour)
	for _, spec := range []struct {
		name     string
		priority int
		expires  time.Time
	}{{"voice-a", 100, time.Time{}}, {"voice-b", 1, expired}} {
		credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderConsole, AuthType: account.AuthTypeOAuth, Name: spec.name, SourceKey: spec.name,
			EncryptedAccessToken: "token", ExpiresAt: spec.expires, Enabled: true, AuthStatus: account.AuthStatusActive,
			MaxConcurrent: 1, Priority: spec.priority,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{voiceModel}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	registry := provider.NewRegistry(refreshVoiceAdapter{})
	sticky := memory.NewStickyStore()
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	recorder := &recordingAudits{}
	service := NewService(modelRepo, recorder, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, nil, 2)
	calls := 0
	result, err := service.executeVoice(ctx, "req-voice-stale", clientkey.Key{ID: 1}, voiceModel, audit.OperationSTT, modeldomain.CapabilitySTT, true, nil, "", "", nil, func(account.Provider) bool {
		return true
	}, func(_ context.Context, _ account.Provider, credential account.Credential, _ string) (voiceExecutionResult, error) {
		calls++
		return voiceExecutionResult{response: jsonVoiceResponse(http.StatusInternalServerError, map[string]any{"error": map[string]any{"message": "asr down"}})}, nil
	})
	if err != nil {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	body, readErr := io.ReadAll(result.Body)
	result.Finalize(Usage{}, "", "")
	_ = result.Body.Close()
	if readErr != nil || len(body) == 0 || result.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %q, err = %v", result.StatusCode, body, readErr)
	}
	if len(recorder.records) == 0 || recorder.records[0].AccountID == nil || *recorder.records[0].AccountID == 0 {
		t.Fatalf("failure audit = %#v", recorder.records)
	}
}

type recordingAudits struct{ records []audit.Record }

func (r *recordingAudits) Create(_ context.Context, value audit.Record) error {
	r.records = append(r.records, value)
	return nil
}

// A credential rejected through an error fails over to the next account.
func TestVoiceSSORejectionFailsOver(t *testing.T) {
	calls := map[uint64]int{}
	service, model := consoleSSOPairService(t, modeldomain.CapabilitySTT)
	result, err := service.executeVoice(context.Background(), "req-voice-sso", clientkey.Key{ID: 1}, model, audit.OperationSTT, modeldomain.CapabilitySTT, true, nil, "", "", nil, func(account.Provider) bool {
		return true
	}, func(_ context.Context, _ account.Provider, credential account.Credential, _ string) (voiceExecutionResult, error) {
		calls[credential.ID]++
		if len(calls) == 1 {
			return voiceExecutionResult{}, fmt.Errorf("stt: %w", provider.ErrUnauthorized)
		}
		return voiceExecutionResult{response: jsonVoiceResponse(http.StatusOK, map[string]any{"text": "ok"})}, nil
	})
	if err != nil || result == nil || result.StatusCode != http.StatusOK {
		t.Fatalf("no failover after an SSO rejection: calls = %v, err = %v", calls, err)
	}
	result.Finalize(Usage{}, "", "")
	_ = result.Body.Close()
}
