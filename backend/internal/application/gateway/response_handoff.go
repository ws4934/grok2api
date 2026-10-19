package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	neterrorpkg "github.com/chenyme/grok2api/backend/internal/pkg/neterror"
)

// responseQualityFallback is the single successful no-thinking stream that
// fail_open retains. The account lease is released immediately; the read pump
// applies upstream backpressure until this response is either delivered or
// replaced by a better one.
type responseQualityFallback struct {
	response          *provider.Response
	lease             *accountLease
	credential        accountdomain.Credential
	usage             Usage
	upstreamStartedAt time.Time
}

// acceptSuccess records a 2xx upstream response, applies the quality hold and
// invalidates accounts whose primary endpoint denied access before a recovered
// response.
func (a *responseAttempt) acceptSuccess(t *responseTry) attemptStep {
	s, ctx, credential := a.s, a.ctx, t.credential
	s.selector.markSuccess(ctx, credential, t.lease.QuotaProbe)
	if a.qualityHoldEnabled {
		if step := a.holdQuality(t); step != attemptProceed {
			return step
		}
	}
	if diagnostic := t.response.RecoveredPrimaryFailure; diagnostic != nil {
		recoveredFailure := newHTTPUpstreamFailure(diagnostic.StatusCode, diagnostic.Body, credential.ID, credential.Name)
		if recoveredFailure.AccountBlocked || (a.failurePolicy.AuthenticatedForbidden && s.shouldInvalidateBuildForbidden(recoveredFailure)) {
			reason := fmt.Sprintf("%s primary endpoint denied account access", credential.Provider)
			if !s.markReauthRequired(ctx, a.input.RequestID, credential, reason) {
				s.selector.MarkModelAccessDenied(ctx, credential, a.route.UpstreamModel, 0)
			}
		}
	}
	return attemptProceed
}

// holdQuality peeks a held stream for missing reasoning and delivers it,
// keeps it as the fail_open fallback, retries another account, or rejects the
// request.
func (a *responseAttempt) holdQuality(t *responseTry) attemptStep {
	s, ctx, requestID, holdCfg, lease, credential := a.s, a.ctx, a.input.RequestID, a.holdCfg, t.lease, t.credential
	replay, verdict, peekUsage, _, peekErr := peekQualityStream(ctx, t.response.Body, qualityProtocolForOperation(a.operation), holdCfg)
	if peekErr != nil {
		return a.qualityPeekFailed(t, replay, peekErr)
	}
	t.response.Body = replay
	hasNextAccount := a.qualityCrossAccountReplay && a.attemptPolicy.hasNext(t.attempt) && a.qualityAccountAttempts < holdCfg.MaxAttempts
	if hasNextAccount {
		hasNextAccount = a.selection != nil && a.selection.hasAvailableCandidate(a.excluded, !a.quotaProbeAttempted)
	}
	commit := CommitQualityHold(verdict, a.qualityAccountAttempts-1, holdCfg.MaxAttempts, hasNextAccount, holdCfg.OnExhausted)
	if verdict == QualityWithhold {
		s.applyMissingThinkingPenalty(ctx, requestID, credential, holdCfg.AccountCooldown, holdCfg.AutoDisable)
	}
	deferFailOpenAudit := commit.Action == QualityActionRetry && holdCfg.OnExhausted == qualityRetryFailOpen
	if commit.Audit && !deferFailOpenAudit {
		s.recordQualityDegraded(ctx, a.auditBase, credential, peekUsage, a.startedAt, a.egressTrace, a.route.Provider)
		a.failureAttempts.captureQualityDegraded(credential, a.responseStartedAt)
	}
	switch commit.Action {
	case QualityActionRetry:
		if deferFailOpenAudit {
			a.discardFallback(true)
			a.fallback = &responseQualityFallback{response: t.response, lease: lease, credential: credential, usage: peekUsage, upstreamStartedAt: a.responseStartedAt}
			lease.completeSelectorObservation(true)
			lease.Release()
		} else {
			_ = t.response.Body.Close()
			lease.Release()
		}
		a.lastErr = errQualityDegraded
		a.lastFailure = qualityDegradedFailure(credential)
		s.logger.Info("quality_degraded_retry", "request_id", requestID, "account_id", credential.ID, "quality_attempt", a.qualityAccountAttempts, "output_tokens", peekUsage.OutputTokens)
		return attemptNext
	case QualityActionReject:
		_ = t.response.Body.Close()
		lease.Release()
		a.lastErr = errQualityDegraded
		a.lastFailure = qualityDegradedFailure(credential)
		s.logger.Info("quality_degraded_rejected", "request_id", requestID, "account_id", credential.ID)
		return attemptStop
	case QualityActionDeliverLast:
		a.discardFallback(true)
		s.logger.Info("quality_degraded_deliver_last", "request_id", requestID, "account_id", credential.ID, "quality_attempt", a.qualityAccountAttempts, "output_tokens", peekUsage.OutputTokens)
	case QualityActionDeliver:
		a.discardFallback(true)
	}
	if !commit.KeepBody {
		_ = t.response.Body.Close()
		lease.Release()
		return attemptStop
	}
	return attemptProceed
}

// qualityPeekFailed records a held stream that failed while it was peeked and
// decides whether another account may be tried.
func (a *responseAttempt) qualityPeekFailed(t *responseTry, replay io.ReadCloser, peekErr error) attemptStep {
	s, ctx, requestID, holdCfg, credential := a.s, a.ctx, a.input.RequestID, a.holdCfg, t.credential
	if replay != nil {
		_ = replay.Close()
	} else {
		_ = t.response.Body.Close()
	}
	t.lease.Release()
	a.lastErr = peekErr
	if isClientRequestCancel(ctx, peekErr) {
		a.lastFailure = a.canceledFailure(credential, peekErr)
		return attemptStop
	}
	a.lastFailure = newTransportUpstreamFailure(peekErr, credential.ID, credential.Name)
	if neterrorpkg.IsUpstreamStreamIdleTimeout(peekErr) || neterrorpkg.IsUpstreamStreamIdleTimeout(context.Cause(ctx)) || errors.Is(peekErr, errQualityEmptyStream) {
		logPrefix := "quality_peek_idle"
		if errors.Is(peekErr, errQualityEmptyStream) {
			logPrefix = "quality_peek_empty"
		}
		writeCtx, writeCancel := context.WithTimeout(context.WithoutCancel(ctx), finalizationTimeout)
		if markErr := s.selector.MarkFailureAfterSuccess(writeCtx, credential, http.StatusGatewayTimeout, holdCfg.IdleAccountCooldown); markErr != nil {
			s.logger.Warn(logPrefix+"_cooldown_failed", "request_id", requestID, "account_id", credential.ID, "error", markErr)
		} else {
			s.logger.Warn(logPrefix+"_retry", "request_id", requestID, "account_id", credential.ID, "cooldown", holdCfg.IdleAccountCooldown)
		}
		writeCancel()
	}
	if !a.qualityCrossAccountReplay || shouldStopForNonAccountFingerprint(a.failureFingerprints, a.lastFailure) {
		return attemptStop
	}
	if isBuildTransientRetryError(peekErr) {
		a.transientRetry++
	}
	return attemptNext
}

// qualityDegradedFailure is the 503 recorded for a response without reasoning.
func qualityDegradedFailure(credential accountdomain.Credential) *UpstreamFailure {
	return &UpstreamFailure{
		HTTPStatus: http.StatusServiceUnavailable, Code: ErrorQualityDegraded,
		PublicMessage: "上游响应缺少推理", AccountID: credential.ID, AccountName: credential.Name,
		Cause: errQualityDegraded,
	}
}

// handOffFallback delivers the retained fail_open stream.
func (a *responseAttempt) handOffFallback() *Result {
	selected := a.fallback
	a.fallback = nil
	a.s.logger.Info("quality_degraded_fallback", "request_id", a.input.RequestID, "account_id", selected.credential.ID, "quality_attempts", a.qualityAccountAttempts)
	return a.handOff(selected.response, selected.lease, selected.credential, selected.upstreamStartedAt)
}

// discardFallback closes the retained fail_open stream, optionally auditing it
// as quality-degraded first.
func (a *responseAttempt) discardFallback(recordDegraded bool) {
	if a.fallback == nil {
		return
	}
	if recordDegraded {
		a.s.recordQualityDegraded(a.ctx, a.auditBase, a.fallback.credential, a.fallback.usage, a.startedAt, a.egressTrace, a.route.Provider)
		a.failureAttempts.captureQualityDegraded(a.fallback.credential, a.fallback.upstreamStartedAt)
	}
	_ = a.fallback.response.Body.Close()
	a.fallback = nil
}

// responseHandoff is a delivered upstream response whose lease, health,
// ownership, quota and audit state are settled once when it finishes.
type responseHandoff struct {
	a                 *responseAttempt
	response          *provider.Response
	lease             *accountLease
	credential        accountdomain.Credential
	accountID         uint64
	upstreamStartedAt time.Time
	once              sync.Once
}

// handOff wraps a delivered upstream response in a Result. Generation timing
// is finished by the Result's Finalize from here on.
func (a *responseAttempt) handOff(response *provider.Response, lease *accountLease, credential accountdomain.Credential, upstreamStartedAt time.Time) *Result {
	h := &responseHandoff{a: a, response: response, lease: lease, credential: credential, accountID: credential.ID, upstreamStartedAt: upstreamStartedAt}
	response.Body = &firstByteReadCloser{ReadCloser: response.Body, mark: a.timing.markFirstBody}
	var markFirstToken func()
	if a.firstToken != nil {
		markFirstToken = a.firstToken.mark
	}
	a.timingHandedOff = true
	return &Result{StatusCode: response.StatusCode, Status: response.Status, Header: response.Header, Body: &finalizingBody{ReadCloser: response.Body, finalize: func() { h.finalize(Usage{}, "", "stream_closed") }}, MarkFirstToken: markFirstToken, RecordStreamFailure: h.recordStreamFailure, Finalize: h.finalize}
}

// recordStreamFailure records a failure event the stream returned after its
// 2xx headers.
func (h *responseHandoff) recordStreamFailure(diagnostic StreamFailureDiagnostic) {
	h.a.failureAttempts.captureStreamFailure(h.credential, h.upstreamStartedAt, h.response, diagnostic)
}

// finalize settles the response once; later calls do nothing.
func (h *responseHandoff) finalize(usage Usage, responseID, errorCode string) {
	h.once.Do(func() { h.settle(usage, responseID, errorCode) })
}

func (h *responseHandoff) settle(usage Usage, responseID, errorCode string) {
	a, s, lease, credential, accountID := h.a, h.a.s, h.lease, h.credential, h.accountID
	input, route := a.input, a.route
	// HTTP 状态码保留线上真实值；流在 2xx 响应头之后失败时由 errorCode
	// 决定最终结果，避免把协议状态与业务结果混为一谈。
	successful := auditRequestSucceeded(h.response.StatusCode, errorCode)
	lease.completeSelectorObservation(successful)
	budget := newFinalizationBudget(string(a.operation), string(route.Provider))
	if isUpstreamStreamFailure(errorCode) {
		status, retryAfter := streamFailureHealthPenalty(errorCode, usage, s.qualityRetryConfig().IdleAccountCooldown)
		if err := budget.run("account_health", finalizationHealthBudget, func(stageCtx context.Context) error {
			return s.selector.MarkFailureAfterSuccess(stageCtx, credential, status, retryAfter)
		}); err != nil {
			s.logger.Warn("stream_failure_health_write_failed", "account_id", credential.ID, "provider", credential.Provider, "error", err)
		}
	}
	lease.Release()
	now := time.Now().UTC()
	record := h.auditRecord(usage, errorCode, successful, now)
	if s.providers.Inference(credential.Provider).QualityAutoDisable {
		if err := budget.run("quality_disable", finalizationHealthBudget, func(stageCtx context.Context) error {
			return s.applyOutputSpeedPenalty(stageCtx, record, credential, a.holdCfg, input.ForcedEgressNodeID != 0)
		}); err != nil {
			s.logger.Error("quality_degraded_penalty_failed", "request_id", input.RequestID, "account_id", credential.ID, "error", err)
		}
	}
	if a.supportsStoredResponses && a.operation == audit.OperationResponses && responseID != "" && successful {
		err := budget.run("response_ownership", finalizationOwnershipBudget, func(stageCtx context.Context) error {
			return s.responses.Save(stageCtx, inferencedomain.ResponseOwnership{ResponseID: responseID, AccountID: accountID, ClientKeyID: input.ClientKey.ID, ModelRouteID: route.ID, Provider: route.Provider, PromptCacheKey: a.ownershipPromptCacheKey, ReasoningReplayKey: a.reasoningReplayKey, ExpiresAt: now.Add(responseOwnershipTTL), CreatedAt: now, UpdatedAt: now})
		})
		if err != nil {
			s.logger.Error("response_ownership_save_failed", "response_id", responseID, "client_key_id", input.ClientKey.ID, "account_id", accountID, "provider", route.Provider, "error", err)
		}
	}
	if successful && lease.QuotaMode != "" && lease.QuotaMode != "weekly" {
		h.decrementQuota(budget)
	}
	if err := budget.run("audit", finalizationAuditBudget, func(stageCtx context.Context) error {
		return s.audits.Create(stageCtx, record)
	}); err != nil {
		s.logger.Error("request_usage_write_failed", "event_id", record.EventID, "request_id", input.RequestID, "error", err)
	}
	if usage.ResponseModel != "" {
		_ = budget.run("observed_model", finalizationMetadataBudget, func(stageCtx context.Context) error {
			return s.accounts.ObserveResponseModel(stageCtx, accountID, usage.ResponseModel)
		})
	}
	if successful && lease.QuotaMode != "" {
		if quotaKind, _ := s.providers.QuotaKind(credential.Provider); quotaKind == provider.QuotaRemoteWindow {
			s.accounts.QueueQuotaRefresh(accountID, lease.QuotaMode)
		}
	}
	outcome := "failed"
	if successful {
		outcome = "success"
	}
	a.timing.finish(s.logger, outcome)
}

// auditRecord builds the request's audit record for the settled response.
func (h *responseHandoff) auditRecord(usage Usage, errorCode string, successful bool, now time.Time) audit.Record {
	a, s, response, route := h.a, h.a.s, h.response, h.a.route
	record := a.auditBase
	if usage.Reported {
		record.UsageSource = a.usageSource
	}
	record.AccountID = &h.accountID
	record.AccountName = h.credential.Name
	record.StatusCode = response.StatusCode
	record.InputTokens = usage.InputTokens
	record.CachedInputTokens = usage.CachedInputTokens
	record.OutputTokens = usage.OutputTokens
	record.ReasoningTokens = usage.ReasoningTokens
	record.TotalTokens = usage.TotalTokens
	record.CostInUSDTicks = usage.CostInUSDTicks
	imagePricing, imagePriced := s.pricing.QuoteImage(route.Provider, a.pricingModel, "", "", response.QuotaUnits)
	if imagePriced {
		record.MediaOutputImages = int64(max(0, response.QuotaUnits))
	}
	tokenPricing, tokenPriced := s.pricing.QuoteText(route.Provider, a.pricingModel, usage.InputTokens, usage.CachedInputTokens, usage.OutputTokens, usage.ContextInputTokens)
	if successful && imagePriced {
		imagePricing.Apply(&record)
	} else if tokenPriced {
		tokenPricing.Apply(&record)
	}
	record.NumSourcesUsed = usage.NumSourcesUsed
	record.NumServerSideToolsUsed = usage.NumServerSideToolsUsed
	record.ContextInputTokens = usage.ContextInputTokens
	record.ContextOutputTokens = usage.ContextOutputTokens
	if successful && a.input.Streaming {
		record.FirstTokenMS = a.firstToken.milliseconds()
	}
	record.DurationMS = time.Since(a.startedAt).Milliseconds()
	record.ErrorCode = errorCode
	if !successful && response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		a.failureAttempts.ensureStreamFailureAttempt(h.credential, h.upstreamStartedAt, response, errorCode)
	}
	attempts := a.failureAttempts.snapshot()
	if !successful || len(attempts) > 0 {
		record.Attempts = attempts
	}
	record.CreatedAt = now
	applyAuditEgress(&record, a.egressTrace, route.Provider)
	return record
}

// decrementQuota consumes the delivered response's quota units on the account.
func (h *responseHandoff) decrementQuota(budget finalizationBudget) {
	s, lease, credential, accountID := h.a.s, h.lease, h.credential, h.accountID
	units := max(1, h.response.QuotaUnits)
	var updated bool
	err := budget.run("quota_decrement", finalizationQuotaBudget, func(stageCtx context.Context) error {
		var decrementErr error
		updated, decrementErr = s.accounts.DecrementQuota(stageCtx, accountID, lease.QuotaMode, units)
		return decrementErr
	})
	if err != nil {
		s.logger.Warn("provider_quota_decrement_failed", "provider", credential.Provider, "account_id", accountID, "mode", lease.QuotaMode, "units", units, "error", err)
	} else if updated {
		s.selector.ConsumeQuota(credential.Provider, accountID, lease.QuotaMode, units)
	}
}
