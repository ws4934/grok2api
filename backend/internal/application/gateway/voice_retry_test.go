package gateway

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
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

// sharedBackendError is a failure no other account can fix.
type sharedBackendError struct{}

func (sharedBackendError) Error() string              { return "shared voice backend failed" }
func (sharedBackendError) HTTPStatusCode() int        { return http.StatusBadGateway }
func (sharedBackendError) RequestScopedFailure() bool { return true }
func (sharedBackendError) PublicErrorMessage() string { return "voice backend failed" }

// voiceRetryService serves one STT route from two Console accounts.
// consoleSSOPairService serves one Console route of the given capability from two SSO accounts.
func consoleSSOPairService(t *testing.T, capability modeldomain.Capability) (*Service, string) {
	t.Helper()
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "voice-retry.db"))
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
	voiceModel := "retry-" + string(capability)
	if err := modelRepo.UpsertRoutes(ctx, []modeldomain.Route{{
		PublicID: voiceModel, Provider: account.ProviderConsole, UpstreamModel: voiceModel,
		Capability: capability, Enabled: true,
	}}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"voice-retry-a", "voice-retry-b"} {
		credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderConsole, AuthType: account.AuthTypeSSO,
			Name: name, SourceKey: name, EncryptedAccessToken: "console-token",
			Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{voiceModel}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	registry := provider.NewRegistry(statelessConsoleAdapter{})
	sticky := memory.NewStickyStore()
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	return NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(nil, nil, nil, 60, 4, nil), registry, selector, nil, 5), voiceModel
}

func runVoiceRetry(t *testing.T, execute func() (voiceExecutionResult, error)) (*Result, int, error) {
	t.Helper()
	service, model := consoleSSOPairService(t, modeldomain.CapabilitySTT)
	calls := 0
	result, err := service.executeVoice(context.Background(), "req-voice-retry", clientkey.Key{ID: 1}, model, audit.OperationSTT, modeldomain.CapabilitySTT, true, nil, "", "", nil, func(account.Provider) bool {
		return true
	}, func(context.Context, account.Provider, account.Credential, string) (voiceExecutionResult, error) {
		calls++
		return execute()
	})
	return result, calls, err
}

func TestVoiceDoesNotRetryRequestScopedBackendFailures(t *testing.T) {
	result, calls, err := runVoiceRetry(t, func() (voiceExecutionResult, error) { return voiceExecutionResult{}, sharedBackendError{} })
	if err != nil || calls != 1 || result.StatusCode != http.StatusBadGateway {
		t.Fatalf("calls = %d, result = %+v, err = %v", calls, result, err)
	}
	result.Finalize(Usage{}, "", "")
	_ = result.Body.Close()
}

func TestVoiceReturnsTheLastUpstreamFailureWhenAccountsRunOut(t *testing.T) {
	result, calls, err := runVoiceRetry(t, func() (voiceExecutionResult, error) {
		return voiceExecutionResult{response: jsonVoiceResponse(http.StatusInternalServerError, map[string]any{"error": map[string]any{"message": "asr down"}})}, nil
	})
	if err != nil || calls != 2 || result == nil || result.StatusCode != http.StatusInternalServerError {
		t.Fatalf("calls = %d, result = %+v, err = %v", calls, result, err)
	}
	body, _ := io.ReadAll(result.Body)
	result.Finalize(Usage{}, "", "")
	_ = result.Body.Close()
	if !strings.Contains(string(body), "asr down") {
		t.Fatalf("body = %s", body)
	}
}
