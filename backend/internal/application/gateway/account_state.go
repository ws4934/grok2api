package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

const modelCatalogRefreshTimeout = 30 * time.Second

type accountModelSyncer interface {
	SyncAccount(ctx context.Context, accountID uint64) (int, error)
}

type buildForbiddenReauthPolicy struct {
	enabled bool
	codes   map[string]struct{}
}

// UpdateBuildForbiddenReauthPolicy atomically replaces the Build account invalidation policy.
func (s *Service) UpdateBuildForbiddenReauthPolicy(enabled bool, codes []string) {
	policy := &buildForbiddenReauthPolicy{enabled: enabled, codes: make(map[string]struct{}, len(codes))}
	for _, value := range codes {
		code := normalizeFailureCode(value)
		if code != "" {
			policy.codes[code] = struct{}{}
		}
	}
	s.buildForbiddenReauth.Store(policy)
}

func (s *Service) shouldInvalidateBuildForbidden(failure *UpstreamFailure) bool {
	if failure == nil || failure.HTTPStatus != http.StatusForbidden {
		return false
	}
	// A configured code is only a second factor. The response body must also
	// contain a high-confidence account-scoped signal; permission-denied alone
	// is shared by content, policy, and other request-level failures.
	if !failure.AccountScoped || failure.SafetyRejection || failure.RequestScopedForbidden {
		return false
	}
	policy := s.buildForbiddenReauth.Load()
	if policy == nil || !policy.enabled {
		return false
	}
	_, matched := policy.codes[normalizeFailureCode(failure.UpstreamCode)]
	return matched
}

func (s *Service) markReauthRequired(ctx context.Context, requestID string, credential accountdomain.Credential, reason string) bool {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), accountStateWriteTimeout)
	defer cancel()
	if err := s.accounts.MarkReauthRequired(writeCtx, credential.ID, reason); err != nil {
		s.logger.Error("account_reauth_required_write_failed", "request_id", requestID, "account_id", credential.ID, "provider", credential.Provider, "error", err)
		return false
	}
	s.selector.MarkQuotaStateChanged(credential.Provider)
	return true
}

func isSSOCredentialRejected(err error, credential accountdomain.Credential) bool {
	if credential.AuthType != accountdomain.AuthTypeSSO || err == nil {
		return false
	}
	if errors.Is(err, provider.ErrUnauthorized) {
		return true
	}
	status, ok := provider.ErrorHTTPStatus(err)
	return ok && status == http.StatusUnauthorized
}

func (s *Service) markSSOCredentialRejected(ctx context.Context, credential accountdomain.Credential, reason string) {
	if credential.AuthType != accountdomain.AuthTypeSSO {
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalizationTimeout)
	defer cancel()
	if err := s.accounts.MarkReauthRequired(writeCtx, credential.ID, reason); err != nil {
		s.logger.Error("account_reauth_required_write_failed", "account_id", credential.ID, "provider", credential.Provider, "error", err)
	}
	// Discard the process-local one-second candidate snapshot even if persistence fails,
	// preventing the invalid account from being selected by the next request.
	s.selector.MarkQuotaStateChanged(credential.Provider)
}

func (s *Service) queueAccountModelSync(accountID uint64) {
	syncer, ok := s.models.(accountModelSyncer)
	if !ok || accountID == 0 {
		return
	}
	s.modelSyncMu.Lock()
	if s.modelSyncing == nil {
		s.modelSyncing = make(map[uint64]struct{})
	}
	if _, exists := s.modelSyncing[accountID]; exists {
		s.modelSyncMu.Unlock()
		return
	}
	s.modelSyncing[accountID] = struct{}{}
	s.modelSyncMu.Unlock()

	go func() {
		defer func() {
			s.modelSyncMu.Lock()
			delete(s.modelSyncing, accountID)
			s.modelSyncMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), modelCatalogRefreshTimeout)
		defer cancel()
		logger := s.logger
		if logger == nil {
			logger = slog.Default()
		}
		count, err := syncer.SyncAccount(ctx, accountID)
		if err != nil {
			logger.Warn("model_etag_refresh_failed", "account_id", accountID, "error", err)
			return
		}
		logger.Info("model_etag_refresh_completed", "account_id", accountID, "models", count)
	}()
}

// markPermanentlyUnrefreshableCredentialRejected removes an account from the pool after a real upstream request confirms its access token is invalid.
func (s *Service) markPermanentlyUnrefreshableCredentialRejected(ctx context.Context, credential accountdomain.Credential) bool {
	if !credential.RefreshPermanent {
		return false
	}
	s.markCredentialRejectedAfterPermanentRefresh(ctx, credential)
	return true
}

func (s *Service) markCredentialRejectedAfterPermanentRefresh(ctx context.Context, credential accountdomain.Credential) {
	_ = s.accounts.MarkReauthRequired(ctx, credential.ID, fmt.Sprintf("%s OAuth access token rejected after permanent refresh failure", credential.Provider))
}

// consoleNoCreditsReason is recorded on Console accounts the upstream reports
// as never provisioned. Retrying, cooling down or rotating egress cannot help.
const consoleNoCreditsReason = accountdomain.LastErrorConsoleUnlinked

func isConsoleNoCreditsFailure(upstreamProvider accountdomain.Provider, failure *UpstreamFailure) bool {
	return upstreamProvider == accountdomain.ProviderConsole && failure != nil && failure.NoCreditsOrLicenses
}

// retireConsoleNoCreditsAccount inspects a Console 403 and, when it is the
// permanent no-credits denial, marks the account reauthRequired. The body is
// restored on the response either way.
func (s *Service) retireConsoleNoCreditsAccount(ctx context.Context, requestID string, credential accountdomain.Credential, response *provider.Response) bool {
	if credential.Provider != accountdomain.ProviderConsole || response == nil || response.StatusCode != http.StatusForbidden {
		return false
	}
	body, _ := readRetryableBody(response.Body)
	response.Body = io.NopCloser(bytes.NewReader(body))
	if !provider.IsConsoleNoCreditsText(string(body)) {
		return false
	}
	s.markReauthRequired(ctx, requestID, credential, consoleNoCreditsReason)
	s.logger.Warn("console_account_no_credits", "request_id", requestID, "account_id", credential.ID)
	return true
}

func (s *Service) applyRateLimitReconciliation(ctx context.Context, credential accountdomain.Credential, status int, retryAfter time.Duration, state accountapp.RateLimitReconcileState, reconcileErr error) {
	if reconcileErr == nil && state == accountapp.RateLimitReconcileExhausted {
		return
	}
	if status == http.StatusPaymentRequired && s.providers.Failure(credential.Provider).PaymentRequiredQuota {
		// 402 spending-limit is authoritative even when the usage still shows
		// budget: it is recorded ~45s after execution. Pause until the next
		// probe instead of applying an exponential failure cooldown.
		if err := s.selector.MarkPaymentQuotaExhausted(ctx, credential, quotaRecoveryHints{}); err != nil {
			s.logger.Warn("spending_limit_pause_failed", "provider", credential.Provider, "account_id", credential.ID, "state", state, "error", err)
		}
		return
	}
	if status == http.StatusTooManyRequests && s.providers.Failure(credential.Provider).SoftRateLimit {
		// A 429 with available quota, an in-progress cross-instance probe, or an
		// inconclusive usage request is transient. Isolate the account for this
		// Retry-After window without growing its durable failure count.
		if err := s.selector.markSoftFailure(ctx, credential, status, retryAfter); err != nil {
			s.logger.Warn("rate_limit_soft_cooldown_failed", "provider", credential.Provider, "account_id", credential.ID, "state", state, "error", err)
		}
		return
	}
	s.selector.MarkFailure(ctx, credential, status, retryAfter)
}
