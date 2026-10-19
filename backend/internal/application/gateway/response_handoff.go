package gateway

import (
	"context"
	"net/http"
	"sync"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
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

// handOff wraps a delivered upstream response in a Result whose Finalize
// releases the lease and writes health, ownership, quota and audit state once.
func (a *responseAttempt) handOff(response *provider.Response, lease *accountLease, credential accountdomain.Credential, upstreamStartedAt time.Time) *Result {
	s, input, route, operation, timing := a.s, a.input, a.route, a.operation, a.timing
	accountID := credential.ID
	var once sync.Once
	finalize := func(usage Usage, responseID, errorCode string) {
		once.Do(func() {
			// HTTP 状态码保留线上真实值；流在 2xx 响应头之后失败时由 errorCode
			// 决定最终结果，避免把协议状态与业务结果混为一谈。
			successful := auditRequestSucceeded(response.StatusCode, errorCode)
			lease.completeSelectorObservation(successful)
			budget := newFinalizationBudget(string(operation), string(route.Provider))
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
			record := a.auditBase
			if usage.Reported {
				record.UsageSource = a.usageSource
			}
			record.AccountID = &accountID
			record.AccountName = credential.Name
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
			if successful && input.Streaming {
				record.FirstTokenMS = a.firstToken.milliseconds()
			}
			record.DurationMS = time.Since(a.startedAt).Milliseconds()
			record.ErrorCode = errorCode
			if !successful && response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
				a.failureAttempts.ensureStreamFailureAttempt(credential, upstreamStartedAt, response, errorCode)
			}
			attempts := a.failureAttempts.snapshot()
			if !successful || len(attempts) > 0 {
				record.Attempts = attempts
			}
			record.CreatedAt = now
			applyAuditEgress(&record, a.egressTrace, route.Provider)
			if s.providers.Inference(credential.Provider).QualityAutoDisable {
				if err := budget.run("quality_disable", finalizationHealthBudget, func(stageCtx context.Context) error {
					return s.applyOutputSpeedPenalty(stageCtx, record, credential, a.holdCfg, input.ForcedEgressNodeID != 0)
				}); err != nil {
					s.logger.Error("quality_degraded_penalty_failed", "request_id", input.RequestID, "account_id", credential.ID, "error", err)
				}
			}
			if a.supportsStoredResponses && operation == audit.OperationResponses && responseID != "" && successful {
				err := budget.run("response_ownership", finalizationOwnershipBudget, func(stageCtx context.Context) error {
					return s.responses.Save(stageCtx, inferencedomain.ResponseOwnership{ResponseID: responseID, AccountID: accountID, ClientKeyID: input.ClientKey.ID, ModelRouteID: route.ID, Provider: route.Provider, PromptCacheKey: a.ownershipPromptCacheKey, ReasoningReplayKey: a.reasoningReplayKey, ExpiresAt: now.Add(responseOwnershipTTL), CreatedAt: now, UpdatedAt: now})
				})
				if err != nil {
					s.logger.Error("response_ownership_save_failed", "response_id", responseID, "client_key_id", input.ClientKey.ID, "account_id", accountID, "provider", route.Provider, "error", err)
				}
			}
			if successful && lease.QuotaMode != "" {
				if lease.QuotaMode != "weekly" {
					units := max(1, response.QuotaUnits)
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
			timing.finish(s.logger, outcome)
		})
	}
	response.Body = &firstByteReadCloser{ReadCloser: response.Body, mark: timing.markFirstBody}
	recordStreamFailure := func(diagnostic StreamFailureDiagnostic) {
		a.failureAttempts.captureStreamFailure(credential, upstreamStartedAt, response, diagnostic)
	}
	var markFirstToken func()
	if a.firstToken != nil {
		markFirstToken = a.firstToken.mark
	}
	a.timingHandedOff = true
	return &Result{StatusCode: response.StatusCode, Status: response.Status, Header: response.Header, Body: &finalizingBody{ReadCloser: response.Body, finalize: func() { finalize(Usage{}, "", "stream_closed") }}, MarkFirstToken: markFirstToken, RecordStreamFailure: recordStreamFailure, Finalize: finalize}
}
