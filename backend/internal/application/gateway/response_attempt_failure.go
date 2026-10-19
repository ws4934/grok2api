package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// canceledFailure is the 499 recorded when the client request was canceled.
func (a *responseAttempt) canceledFailure(credential accountdomain.Credential, err error) *UpstreamFailure {
	return &UpstreamFailure{HTTPStatus: 499, Code: "request_canceled", PublicMessage: "请求已取消", AccountID: credential.ID, AccountName: credential.Name, Cause: firstError(a.ctx.Err(), err)}
}

// handleForwardError records an upstream call that returned no response and
// decides whether the request may move to another account.
func (a *responseAttempt) handleForwardError(lease *accountLease, credential accountdomain.Credential, err error) attemptStep {
	s, ctx := a.s, a.ctx
	lease.Release()
	a.lastErr = err
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		a.lastFailure = a.canceledFailure(credential, err)
		return attemptStop
	}
	if isSSOCredentialRejected(err, credential) {
		s.markSSOCredentialRejected(ctx, credential, fmt.Sprintf("%s SSO credential rejected", credential.Provider))
		a.lastFailure = newHTTPUpstreamFailure(http.StatusUnauthorized, nil, credential.ID, credential.Name)
		return attemptNext
	}
	a.lastFailure = newTransportUpstreamFailure(err, credential.ID, credential.Name)
	if !isRetryableTransportFailure(a.failurePolicy, err) {
		return attemptStop
	}
	responseFailure := false
	if status, retryAfter, classified := upstreamResponseErrorHealthPenalty(err, a.holdCfg.IdleAccountCooldown); classified {
		responseFailure = true
		writeCtx, writeCancel := context.WithTimeout(context.WithoutCancel(ctx), finalizationTimeout)
		markErr := s.selector.MarkFailureAfterSuccess(writeCtx, credential, status, retryAfter)
		writeCancel()
		if markErr != nil {
			s.logger.Warn("upstream_response_health_write_failed", "request_id", a.input.RequestID, "account_id", credential.ID, "error", markErr)
		} else {
			s.logger.Warn("upstream_response_health_retry", "request_id", a.input.RequestID, "account_id", credential.ID, "status", status, "minimum_cooldown", retryAfter)
		}
	} else {
		s.selector.MarkFailure(ctx, credential, 0, 0)
	}
	// The failed response is safe to retry on another account only when
	// doing so cannot repeat an upstream-hosted tool side effect.
	if responseFailure && qualityRequestHasReplayUnsafeHostedTools(a.input.Body) {
		return attemptStop
	}
	if shouldStopForNonAccountFingerprint(a.failureFingerprints, a.lastFailure) {
		return attemptStop
	}
	if isBuildTransientRetryError(err) {
		a.transientRetry++
	}
	return attemptNext
}

// replayFailed records a replay with a refreshed credential that returned no
// response. The lease is already released.
func (a *responseAttempt) replayFailed(credential accountdomain.Credential, err error) attemptStep {
	if a.ctx.Err() != nil || errors.Is(err, context.Canceled) {
		a.lastFailure = a.canceledFailure(credential, err)
		return attemptStop
	}
	a.lastFailure = newTransportUpstreamFailure(err, credential.ID, credential.Name)
	if !isRetryableTransportFailure(a.failurePolicy, err) {
		return attemptStop
	}
	if shouldStopForNonAccountFingerprint(a.failureFingerprints, a.lastFailure) {
		return attemptStop
	}
	if isBuildTransientRetryError(err) {
		a.transientRetry++
	}
	return attemptNext
}

// recoverUnauthorized handles an upstream 401. SSO and permanently
// unrefreshable credentials rotate the account; others are refreshed once and
// the request is replayed on the same lease.
func (a *responseAttempt) recoverUnauthorized(t *responseTry) attemptStep {
	s, ctx, lease := a.s, a.ctx, t.lease
	t.response.Body.Close()
	if t.credential.AuthType == accountdomain.AuthTypeSSO {
		s.markSSOCredentialRejected(ctx, t.credential, fmt.Sprintf("%s SSO credential rejected", t.credential.Provider))
		lease.Release()
		a.lastErr = fmt.Errorf("%s SSO 凭据已失效", t.credential.Provider)
		a.lastFailure = newHTTPUpstreamFailure(http.StatusUnauthorized, nil, t.credential.ID, t.credential.Name)
		return attemptNext
	}
	if s.markPermanentlyUnrefreshableCredentialRejected(ctx, t.credential) {
		lease.Release()
		a.lastErr = accountapp.ErrCredentialRefreshPermanent
		a.lastFailure = newHTTPUpstreamFailure(http.StatusUnauthorized, nil, t.credential.ID, t.credential.Name)
		return attemptNext
	}
	a.authRecoveryAttempted[t.credential.ID] = true
	refreshed, refreshErr := a.ensureCredential(t.credential, true)
	var err error
	if refreshErr == nil {
		t.response, err = a.forward(lease, refreshed, lease.Billing)
		t.credential = refreshed
	}
	if refreshErr != nil || err != nil {
		if errors.Is(refreshErr, accountapp.ErrCredentialRefreshPermanent) {
			s.markCredentialRejectedAfterPermanentRefresh(ctx, t.credential)
		}
		s.selector.MarkRefreshDeferred(ctx, t.credential, refreshErr)
		lease.Release()
		a.lastErr = firstError(refreshErr, err)
		if refreshErr != nil {
			a.lastFailure = newCredentialUpstreamFailure(refreshErr, t.credential.ID, t.credential.Name)
			return attemptNext
		}
		return a.replayFailed(t.credential, err)
	}
	if t.response.StatusCode == http.StatusUnauthorized {
		body, _ := readRetryableBody(t.response.Body)
		_ = s.accounts.MarkReauthRequired(ctx, t.credential.ID, fmt.Sprintf("%s OAuth credential rejected after refresh", t.credential.Provider))
		s.selector.MarkQuotaStateChanged(t.credential.Provider, t.credential.ID)
		lease.Release()
		a.lastErr = fmt.Errorf("刷新后上游仍返回 401")
		a.lastFailure = newHTTPUpstreamFailure(http.StatusUnauthorized, body, t.credential.ID, t.credential.Name)
		return attemptNext
	}
	// The replayed response is classified on from here; like any response it
	// may report a changed model catalog.
	if t.response.ModelCatalogChanged {
		s.queueAccountModelSync(t.credential.ID)
	}
	return attemptProceed
}

// classifyForbidden classifies a 403 body before egress retry. Definitive
// blocked-account signals invalidate and rotate the account; request-level
// safety rejections are returned as-is without account side effects; all
// other 403 responses retain the egress retry path without penalizing the
// account.
func (a *responseAttempt) classifyForbidden(t *responseTry, egressForbidden, finalEgressForbidden bool) attemptStep {
	s, ctx, requestID, lease, credential, response := a.s, a.ctx, a.input.RequestID, t.lease, t.credential, t.response
	retryAfter := parseRetryAfter(response.Header.Get("Retry-After"), time.Now().UTC())
	body, _ := readRetryableBody(response.Body)
	a.lastFailure = newHTTPUpstreamFailure(response.StatusCode, body, credential.ID, credential.Name)
	if isTerminalRequestForbidden(a.failurePolicy, a.lastFailure) {
		// Deterministic request-scoped 403: restore the original body and return it
		// without OAuth refresh, account rotation, cooldown, or invalidation.
		t.terminalForbidden = true
		response.Body = io.NopCloser(bytes.NewReader(body))
		lease.completeSelectorObservation(false)
		lease.Release()
		if a.lastFailure.SafetyRejection {
			s.logger.Warn("upstream_safety_rejection", "request_id", requestID, "account_id", credential.ID, "provider", credential.Provider, "status", response.StatusCode, "upstream_code", a.lastFailure.UpstreamCode)
		} else {
			s.logger.Warn("upstream_request_scoped_forbidden", "request_id", requestID, "account_id", credential.ID, "provider", credential.Provider, "status", response.StatusCode, "upstream_code", a.lastFailure.UpstreamCode)
		}
		// Fall through to the common success/error response path so the client receives the original 403.
		return attemptProceed
	}
	if a.lastFailure.AccountBlocked {
		failureHandled := s.markReauthRequired(ctx, requestID, credential, fmt.Sprintf("%s account is blocked", credential.Provider))
		if a.lastFailure.AccountScoped && !failureHandled {
			s.selector.MarkFailure(ctx, credential, response.StatusCode, retryAfter)
		}
		lease.Release()
		a.lastErr = fmt.Errorf("上游返回 %d", response.StatusCode)
		s.logger.Warn("upstream_request_failed", "request_id", requestID, "account_id", credential.ID, "provider", credential.Provider, "status", response.StatusCode, "upstream_code", a.lastFailure.UpstreamCode, "account_scoped", a.lastFailure.AccountScoped, "account_blocked", true)
		return attemptNext
	}
	if egressForbidden && !finalEgressForbidden {
		// A non-blocking 403 is an egress/browser-session failure and must not penalize the account.
		delete(a.excluded, credential.ID)
		if a.selection != nil {
			a.selection.RetryAccount(credential.ID)
		}
		lease.Release()
		a.lastErr = fmt.Errorf("上游出口会话被拒绝")
		return attemptNext
	}
	// Restore the consumed final non-blocking 403 body for the common response path.
	response.Body = io.NopCloser(bytes.NewReader(body))
	return attemptProceed
}

// handleRetryableResponse records a retryable upstream status and its account
// side effects, then rotates the account, stops, or replays the request once
// with a refreshed credential.
func (a *responseAttempt) handleRetryableResponse(t *responseTry) attemptStep {
	s, ctx := a.s, a.ctx
	retryAfter := parseRetryAfter(t.response.Header.Get("Retry-After"), time.Now().UTC())
	body, _ := readRetryableBody(t.response.Body)
	a.lastFailure = newHTTPUpstreamFailure(t.response.StatusCode, body, t.credential.ID, t.credential.Name)
	if t.response.StatusCode == http.StatusTooManyRequests && t.response.RateLimit == nil {
		if metadata := provider.ParseRateLimitMetadata(body); metadata != nil {
			t.response.RateLimit = metadata
			if retryAfter <= 0 && metadata.RetryAfter > 0 {
				retryAfter = metadata.RetryAfter
			}
		}
	}
	forbiddenReauth := a.failurePolicy.AuthenticatedForbidden && s.shouldInvalidateBuildForbidden(a.lastFailure)
	if step := a.shieldTeamModelRateLimit(t); step != attemptProceed {
		return step
	}
	if step := a.refreshRejectedCredential(t); step != attemptProceed {
		return step
	}
	failureHandled := a.markQuotaFailure(t, body, retryAfter)
	failureHandled = a.invalidateFailedAccount(t, failureHandled, retryAfter, forbiddenReauth)
	if !failureHandled && (a.lastFailure.AccountScoped || a.failurePolicy.AuthenticatedForbidden && t.response.StatusCode == http.StatusForbidden) {
		s.selector.MarkFailure(ctx, t.credential, t.response.StatusCode, retryAfter)
	} else if !a.lastFailure.AccountScoped && t.response.StatusCode >= http.StatusInternalServerError {
		// Provider-wide 5xx responses should rotate this request and briefly
		// isolate the account across requests, but must not grow the durable
		// account failure count exponentially. Preserve the real status in
		// health diagnostics while applying the explicit soft policy.
		if markErr := s.selector.markSoftFailure(ctx, t.credential, t.response.StatusCode, retryAfter); markErr != nil {
			s.logger.Warn("upstream_soft_cooldown_failed", "request_id", a.input.RequestID, "account_id", t.credential.ID, "provider", t.credential.Provider, "status", t.response.StatusCode, "error", markErr)
		}
	}
	t.lease.Release()
	a.lastErr = fmt.Errorf("上游返回 %d", t.response.StatusCode)
	s.logger.Warn("upstream_request_failed", "request_id", a.input.RequestID, "account_id", t.credential.ID, "provider", t.credential.Provider, "status", t.response.StatusCode, "upstream_code", a.lastFailure.UpstreamCode, "account_scoped", a.lastFailure.AccountScoped)
	if shouldStopForNonAccountFingerprint(a.failureFingerprints, a.lastFailure) {
		return attemptStop
	}
	if isBuildTransientRetryStatus(t.response.StatusCode) {
		a.transientRetry++
	}
	return attemptNext
}

// shieldTeamModelRateLimit turns a 429 for this model into a Team+Model
// cooldown and rotates the account. Without a team identity the 429 falls
// through to account-scoped handling.
func (a *responseAttempt) shieldTeamModelRateLimit(t *responseTry) attemptStep {
	s, route, credential, response := a.s, a.route, t.credential, t.response
	if response.StatusCode != http.StatusTooManyRequests || response.RateLimit == nil || response.RateLimit.Model != route.UpstreamModel {
		return attemptProceed
	}
	rateLimitMeta := *response.RateLimit
	if strings.TrimSpace(rateLimitMeta.TeamID) == "" {
		rateLimitMeta.TeamID = strings.TrimSpace(credential.TeamID)
	}
	if rateLimitMeta.TeamID == "" {
		// Team+Model shielding requires a team identity; fall through to account-scoped 429 handling.
		return attemptProceed
	}
	limited := s.markTeamModelRateLimit(credential, route.UpstreamModel, rateLimitMeta, time.Now().UTC())
	a.lastFailure.AccountScoped = false
	a.lastFailure.Fingerprint = "429:team_model_rate_limit"
	a.lastFailure.RetryAfter = time.Until(limited.Until)
	t.lease.Release()
	a.lastErr = fmt.Errorf("上游 Team 与模型请求频率受限")
	s.logger.Warn("upstream_team_model_rate_limited", "request_id", a.input.RequestID, "provider", credential.Provider, "model", route.UpstreamModel, "team_fingerprint", limited.TeamFingerprint, "scope", rateLimitMeta.Scope, "actual", rateLimitMeta.Actual, "limit", rateLimitMeta.Limit, "retry_after", a.lastFailure.RetryAfter)
	return attemptNext
}

// refreshRejectedCredential refreshes an OAuth credential the upstream
// rejected and replays the request once on the same lease. An
// authenticated-forbidden provider treats only HTTP 401 as an OAuth
// authentication failure. Its 403 must not trigger token rotation or replay
// the same request with freshly issued credentials.
func (a *responseAttempt) refreshRejectedCredential(t *responseTry) attemptStep {
	s := a.s
	refreshable := !a.failurePolicy.AuthenticatedForbidden && s.providers.SupportsCredentialRefresh(t.credential.Provider) && !a.authRecoveryAttempted[t.credential.ID] && t.credential.EncryptedRefreshToken != "" && !a.lastFailure.AccountBlocked && (a.lastFailure.PermanentAccountDenial || a.lastFailure.CredentialRejected)
	if !refreshable {
		return attemptProceed
	}
	a.authRecoveryAttempted[t.credential.ID] = true
	refreshed, refreshErr := a.ensureCredential(t.credential, true)
	if refreshErr != nil {
		s.selector.MarkRefreshDeferred(a.ctx, t.credential, refreshErr)
		t.lease.Release()
		a.lastErr = refreshErr
		a.lastFailure = newCredentialUpstreamFailure(refreshErr, t.credential.ID, t.credential.Name)
		return attemptNext
	}
	var err error
	t.response, err = a.forward(t.lease, refreshed, t.lease.Billing)
	t.credential = refreshed
	if err != nil {
		t.lease.Release()
		a.lastErr = err
		return a.replayFailed(t.credential, err)
	}
	return attemptReplay
}

// markQuotaFailure records the quota or rate-limit state a failed response
// names and reports whether that handled the account failure.
func (a *responseAttempt) markQuotaFailure(t *responseTry, body []byte, retryAfter time.Duration) bool {
	s, ctx, lease, credential := a.s, a.ctx, t.lease, t.credential
	if lease.QuotaMode != "" && t.response.StatusCode == http.StatusTooManyRequests {
		state, reconcileErr := s.accounts.ReconcileRateLimit(ctx, credential.ID, lease.QuotaMode, retryAfter)
		s.applyRateLimitReconciliation(ctx, credential, t.response.StatusCode, retryAfter, state, reconcileErr)
		return true
	}
	if used, limit, exhausted := parseFreeQuotaExhaustion(body); exhausted {
		// The Free subscription signal is account-scoped, but its billing
		// period is not a reliable reset promise. Probe again after 24 hours.
		s.selector.MarkFreeQuotaExhausted(ctx, credential, used, limit)
		return true
	}
	if a.lastFailure.ModelQuotaExhausted {
		s.selector.MarkModelQuotaExhausted(ctx, credential, lease.Billing, a.route.UpstreamModel, retryAfter)
		return true
	}
	if a.lastFailure.FreeQuotaExhausted {
		s.selector.MarkFreeQuotaExhausted(ctx, credential, 0, 0)
		return true
	}
	if a.lastFailure.SpendingLimitBlocked || a.lastFailure.QuotaExhausted {
		err := s.selector.MarkPaymentQuotaExhausted(ctx, credential, quotaRecoveryHints{Billing: lease.Billing})
		if err != nil {
			s.logger.Error("account_quota_recovery_write_failed", "request_id", a.input.RequestID, "account_id", credential.ID, "provider", credential.Provider, "error", err)
		}
		return err == nil
	}
	return false
}

// invalidateFailedAccount marks blocked, denied or rejected accounts for
// reauthorization and returns whether the account failure is handled. handled
// is kept when no invalidation applies.
func (a *responseAttempt) invalidateFailedAccount(t *responseTry, handled bool, retryAfter time.Duration, forbiddenReauth bool) bool {
	s, ctx, requestID, credential, failure := a.s, a.ctx, a.input.RequestID, t.credential, a.lastFailure
	if failure.AccountBlocked {
		return s.markReauthRequired(ctx, requestID, credential, fmt.Sprintf("%s account is blocked", credential.Provider))
	}
	if forbiddenReauth {
		return s.markReauthRequired(ctx, requestID, credential, fmt.Sprintf("%s upstream error code %s matched the invalidation policy", credential.Provider, failure.UpstreamCode))
	}
	if s.providers.SupportsCredentialRefresh(credential.Provider) && failure.PermanentAccountDenial {
		if a.failurePolicy.AuthenticatedForbidden {
			// 默认 model-scoped，视频拒绝时配额/OAuth 仍可能可用。
			// 开启 markBuildChatDeniedAsReauth 时再额外标 reauth，便于号池摘除。
			// 同时写入模型 block，避免在候选缓存窗口内本请求再次选中。
			modelErr := s.selector.MarkModelAccessDenied(ctx, credential, a.route.UpstreamModel, retryAfter)
			handled = modelErr == nil
			if modelErr != nil {
				s.logger.Error("account_model_access_denied_write_failed", "request_id", requestID, "account_id", credential.ID, "provider", credential.Provider, "model", a.route.UpstreamModel, "error", modelErr)
			}
			if s.markBuildChatDeniedAsReauth.Load() {
				reauthHandled := s.markReauthRequired(ctx, requestID, credential, fmt.Sprintf("%s chat endpoint access denied", credential.Provider))
				handled = handled || reauthHandled
			}
			return handled
		}
		return s.markReauthRequired(ctx, requestID, credential, fmt.Sprintf("%s chat endpoint access denied", credential.Provider))
	}
	if s.providers.SupportsCredentialRefresh(credential.Provider) && failure.CredentialRejected {
		return s.markReauthRequired(ctx, requestID, credential, fmt.Sprintf("%s credential rejected", credential.Provider))
	}
	return handled
}
