package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	neterrorpkg "github.com/chenyme/grok2api/backend/internal/pkg/neterror"
	"github.com/chenyme/grok2api/backend/internal/pkg/requestmeta"
)

const responseOwnershipTTL = 30 * 24 * time.Hour

func (s *Service) CreateResponse(ctx context.Context, input Input) (*Result, error) {
	input.Operation = audit.OperationResponses
	switch classifyResponsesCompactionRequest(input.Body) {
	case responsesCompactionTrigger:
		input.Operation = audit.OperationCompaction
	case responsesCompactionTUI:
		// Grok TUI compaction is still a normal Responses request. Keep its
		// routing, Provider normalization, and stored-response behavior intact;
		// only its audit classification and quality-hold policy differ.
		input.auditOperation = audit.OperationCompaction
		input.skipQualityHold = true
	}
	return s.createResponseAt(ctx, input, "/responses")
}

func (s *Service) CreateChatCompletion(ctx context.Context, input Input) (*Result, error) {
	input.Operation = audit.OperationChat
	return s.createResponseAt(ctx, input, "/responses")
}

// CreateMessage executes an Anthropic Messages request through the unified Responses upstream.
func (s *Service) CreateMessage(ctx context.Context, input Input) (*Result, error) {
	input.Operation = audit.OperationMessages
	return s.createResponseAt(ctx, input, "/responses")
}

func (s *Service) CompactResponse(ctx context.Context, input Input) (*Result, error) {
	input.Streaming = false
	input.Operation = audit.OperationCompaction
	return s.createResponseAt(ctx, input, "/responses/compact")
}

func (s *Service) createResponseAt(ctx context.Context, input Input, path string) (*Result, error) {
	ctx, egressTrace := infraegress.WithTrace(ctx)
	startedAt := time.Now()
	var firstToken *firstTokenTimer
	if input.Streaming {
		firstToken = newFirstTokenTimer(startedAt)
	}
	eventID := newAuditEventID()
	// Use a server-generated scope so repeated or absent client request IDs
	// cannot accidentally join independent Composer conversations.
	requestSessionScope := eventID
	operation := input.Operation
	if operation == "" {
		operation = audit.OperationResponses
	}
	auditOperation := operation
	if input.auditOperation != "" {
		auditOperation = input.auditOperation
	}
	routes, aliasEffort, err := s.resolvePublicModelRoutes(ctx, input.PublicModel, input.ClientKey.AllowModelAliases)
	if err != nil {
		return nil, ErrModelNotFound
	}
	// Select an initial route only to preserve the existing stateful/stateless
	// previous_response_id boundary. The actual target is chosen from the eligible
	// pool below after ownership and account availability are known.
	initialRoute, routeErr := s.selectConversationRoute(routes, input.ClientKey, operation, path, false, nil)
	var ownership *inferencedomain.ResponseOwnership
	if input.PreviousResponseID != "" && routeErr == nil {
		if s.providers.SupportsStoredResponses(initialRoute.Provider) {
			value, ownershipErr := s.responses.Get(ctx, input.PreviousResponseID, input.ClientKey.ID, time.Now().UTC())
			if ownershipErr != nil {
				return nil, ErrResponseNotFound
			}
			ownership = &value
		} else if s.providers.SupportsStatelessPreviousResponse(initialRoute.Provider) {
			// The provider retains no Response state, so replay the history statelessly here;
			// Provider normalization removes stale Response IDs.
			input.PreviousResponseID = ""
		} else {
			return nil, ErrResponseStateUnsupported
		}
	}
	eligibleRoutes, fallbackRoute, routeErr := s.eligibleConversationRoutes(routes, input.ClientKey, operation, path, ownership != nil, ownership)
	route := fallbackRoute
	orderedRoutes := eligibleRoutes
	if routeErr == nil {
		orderedRoutes = orderConversationRouteTargets(eligibleRoutes, routeTargetSeed(input))
		route = orderedRoutes[0]
	}
	accountScope := input.ClientKey.AccountScope()
	var preselectedSession *selectionSession
	// Skip targets whose account pool is already known to be unavailable. This
	// gives same-name targets failover before any physical upstream request while
	// preserving pinned Responses and forced administrator probes.
	if routeErr == nil && ownership == nil && input.ForcedEgressNodeID == 0 {
		for _, candidate := range orderedRoutes {
			affinityKey := ""
			if s.providers.Inference(candidate.Provider).SessionIdentity {
				identity := resolveSessionIdentity(
					input.ClientKey.ID,
					candidate.Provider,
					candidate.UpstreamModel,
					input.PromptCacheKey,
					input.PromptCacheSeed,
					input.Body,
				)
				identity = ensureComposerSessionIdentity(identity, input.ClientKey.ID, candidate.Provider, candidate.UpstreamModel, requestSessionScope)
				affinityKey = identity.affinityKey
			}
			candidateSession, selectionErr := s.selector.beginSelectionSessionForKey(
				ctx,
				candidate.Provider,
				candidate.ID,
				candidate.UpstreamModel,
				s.providers.QuotaMode(candidate.Provider, candidate.UpstreamModel),
				affinityKey,
				nil,
				true,
				accountScope,
			)
			if selectionErr != nil {
				continue
			}
			route = candidate
			preselectedSession = candidateSession
			break
		}
	}
	publicModel := modeldomain.ExternalPublicID(route.Provider, route.PublicID)
	input.PublicModel = publicModel
	if aliasEffort != "" {
		input.Body, err = rewriteAliasedModel(input.Body, publicModel, aliasEffort, operation)
		if err != nil {
			return nil, err
		}
	}
	if routeErr != nil && !errors.Is(routeErr, clientkeyapp.ErrModelNotAllowed) {
		return nil, routeErr
	}
	timing := newGenerationTiming(publicModel, route.Provider)
	timingHandedOff := false
	defer func() {
		if !timingHandedOff {
			timing.finish(s.logger, "failed")
		}
	}()
	usageSource := audit.UsageSourceUpstream
	if usageKind, _ := s.providers.UsageKind(route.Provider); usageKind == provider.UsageEstimated {
		usageSource = audit.UsageSourceEstimated
	}
	mediaSummary, _ := summarizeResponseMedia(input.Body)
	logResponseMediaSummary(s.logger, input.RequestID, mediaSummary)
	auditBase := audit.Record{
		EventID: eventID, RequestID: input.RequestID, ClientKeyID: input.ClientKey.ID, ClientKeyName: input.ClientKey.Name,
		ClientIP:     requestmeta.ClientIP(ctx),
		ModelRouteID: route.ID, ModelPublicID: publicModel, ModelUpstreamModel: modeldomain.DisplayUpstreamModel(route.Provider, route.UpstreamModel),
		Provider: string(route.Provider), Operation: auditOperation, UsageSource: audit.UsageSourceNone, Streaming: input.Streaming,
		MediaInputImages: mediaSummary.InputImages,
		RequestMethod:    input.Method, RequestPath: input.Path, RequestHeaders: input.Headers,
	}
	if errors.Is(routeErr, clientkeyapp.ErrModelNotAllowed) {
		record := auditBase
		record.StatusCode = http.StatusForbidden
		record.DurationMS = time.Since(startedAt).Milliseconds()
		record.ErrorCode = "model_not_allowed"
		record.CreatedAt = time.Now().UTC()
		applyAuditEgress(&record, egressTrace, route.Provider)
		if err := s.audits.Create(ctx, record); err != nil {
			s.logger.Error("request_usage_write_failed", "event_id", record.EventID, "request_id", input.RequestID, "error", err)
		}
		return nil, clientkeyapp.ErrModelNotAllowed
	}
	affinityKey := ""
	ownershipPromptCacheKey := ""
	reasoningReplayKey := ""
	if s.providers.Inference(route.Provider).SessionIdentity {
		// Derive a stable identity from explicit session signals, message anchors,
		// and model. Composer replaces message-only fallback identities with an
		// isolated request identity that remains stable across retries.
		identity := sessionIdentity{}
		if ownership != nil && ownership.PromptCacheKey != "" {
			// previous_response_id belongs to an existing Response chain and must inherit the root session identity;
			// do not recompute the soft key from this turn's incremental input.
			identity.upstreamID = ownership.PromptCacheKey
			identity.replayKey = ownership.ReasoningReplayKey
		} else {
			identity = resolveSessionIdentity(
				input.ClientKey.ID,
				route.Provider,
				route.UpstreamModel,
				input.PromptCacheKey,
				input.PromptCacheSeed,
				input.Body,
			)
		}
		identity = ensureComposerSessionIdentity(identity, input.ClientKey.ID, route.Provider, route.UpstreamModel, requestSessionScope)
		input.PromptCacheKey = identity.upstreamID
		affinityKey = identity.affinityKey
		ownershipPromptCacheKey = identity.upstreamID
		reasoningReplayKey = identity.replayKey
		if identity.upstreamID == "" {
			s.logger.Debug("prompt_cache_session_empty", "request_id", input.RequestID, "model", route.UpstreamModel, "provider", route.Provider)
		} else if identity.soft {
			s.logger.Debug("prompt_cache_session_soft", "request_id", input.RequestID, "model", route.UpstreamModel)
		} else if identity.isolated {
			s.logger.Debug("prompt_cache_session_isolated", "request_id", input.RequestID, "model", route.UpstreamModel)
		}
	}
	adapter, ok := s.providers.Responses(route.Provider)
	if !ok {
		return nil, ErrNoAvailableAccount
	}
	physicalCallCtx := infraegress.WithPhysicalCallTrace(ctx, string(route.Provider), string(operation))
	supportsStoredResponses := s.providers.SupportsStoredResponses(route.Provider)
	if input.PreviousResponseID != "" && !supportsStoredResponses {
		return nil, ErrResponseStateUnsupported
	}
	// A lease recovery probe stays on exactly one account and one rendered proxy
	// identity. Retrying the same pinned account would provide neither failover
	// nor new evidence and can multiply a slow/failing probe.
	holdCfg := s.qualityRetryConfig()
	qualityHoldEnabled := s.providers.Inference(route.Provider).QualityHold && shouldHoldQualityStream(input, ownership, route, operation, holdCfg)
	qualityCrossAccountReplay := canReplayQualityHoldAcrossAccounts(input, ownership)
	attemptPolicy := newRequestRoutingAttemptPolicy(int(s.maxAttempts.Load()), ownership != nil || input.ForcedAccountID != 0)
	idempotencyID, _ := security.NewOpaqueToken(18)
	pricingModel := s.providers.PricingModel(route.Provider, route.UpstreamModel)
	if err := s.checkLedgerReady(); err != nil {
		return nil, err
	}
	if reservation, priced := s.pricing.QuoteTextReservation(route.Provider, pricingModel, input.Body); priced {
		if _, err := s.clientKeys.ReserveBilling(ctx, input.ClientKey, eventID, reservation.CostInUSDTicks, s.textBillingReservationTTL()); err != nil {
			return nil, err
		}
	}
	excluded := make(map[uint64]bool)
	failureFingerprints := make(map[string]int)
	authRecoveryAttempted := make(map[uint64]bool)
	// Count accounts that actually reached the upstream. Credential-only skips
	// do not consume the quality retry budget; refreshes stay on the same account.
	qualityAccountAttempts := 0
	quotaMode := s.providers.QuotaMode(route.Provider, route.UpstreamModel)
	failurePolicy := s.providers.Failure(route.Provider)
	quotaProbeAttempted := false
	selection := preselectedSession
	var lastErr error
	var lastFailure *UpstreamFailure
	failureAttempts := newFailureAttemptRecorder(http.MethodPost, path)
	normalizedMetadata := &provider.NormalizedRequestMetadata{}
	responseStartedAt := startedAt
	transientRetry := 0
	forwardResponse := func(lease *accountLease, credential accountdomain.Credential, billing *accountdomain.Billing) (*provider.Response, error) {
		started := time.Now()
		responseStartedAt = started
		lease.markSelectorUpstreamStarted()
		// Assets the provider saves while answering (e.g. images drawn in chat) carry this request's provenance.
		originCtx := mediadomain.WithOrigin(physicalCallCtx, mediadomain.Origin{
			Provider: string(route.Provider), Model: publicModel, UpstreamModel: auditBase.ModelUpstreamModel,
			ClientKeyID: input.ClientKey.ID, ClientKeyName: input.ClientKey.Name, AccountID: credential.ID, AccountName: credential.Name,
			Operation: string(auditOperation), RequestID: input.RequestID,
		})
		response, err := adapter.ForwardResponse(originCtx, provider.ResponseResourceRequest{Credential: credential, ForcedEgressNodeID: input.ForcedEgressNodeID, Billing: billing, Method: http.MethodPost, Path: path, Model: route.UpstreamModel, PromptCacheKey: input.PromptCacheKey, ReasoningReplayKey: reasoningReplayKey, AllowClientToolCacheRoute: input.AllowClientToolCacheRoute, GrokTurnIndex: input.GrokTurnIndex, TransientRetry: transientRetry, IdempotencyID: idempotencyID, Body: input.Body, Streaming: input.Streaming, NormalizeBody: true, Operation: string(operation), NormalizedMetadata: normalizedMetadata})
		auditBase.ReasoningEffort = normalizedMetadata.ReasoningEffort
		err = failureAttempts.captureResponse(credential, started, response, err)
		timing.markUpstream(time.Since(started))
		return response, err
	}
	ensureCredential := func(credential accountdomain.Credential, force bool) (accountdomain.Credential, error) {
		started := time.Now()
		var result accountdomain.Credential
		var err error
		if force {
			// Called only after the upstream rejected the credential.
			result, err = s.accounts.RenewRejectedCredential(ctx, credential)
		} else {
			result, err = s.accounts.EnsureCredential(ctx, credential, false)
		}
		failureAttempts.captureCredentialFailure(credential, started, force, err)
		timing.markCredential(time.Since(started))
		return result, err
	}
	handoffResponse := func(response *provider.Response, lease *accountLease, credential accountdomain.Credential, upstreamStartedAt time.Time) *Result {
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
				record := auditBase
				if usage.Reported {
					record.UsageSource = usageSource
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
				imagePricing, imagePriced := s.pricing.QuoteImage(route.Provider, pricingModel, "", "", response.QuotaUnits)
				if imagePriced {
					record.MediaOutputImages = int64(max(0, response.QuotaUnits))
				}
				tokenPricing, tokenPriced := s.pricing.QuoteText(route.Provider, pricingModel, usage.InputTokens, usage.CachedInputTokens, usage.OutputTokens, usage.ContextInputTokens)
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
					record.FirstTokenMS = firstToken.milliseconds()
				}
				record.DurationMS = time.Since(startedAt).Milliseconds()
				record.ErrorCode = errorCode
				if !successful && response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
					failureAttempts.ensureStreamFailureAttempt(credential, upstreamStartedAt, response, errorCode)
				}
				attempts := failureAttempts.snapshot()
				if !successful || len(attempts) > 0 {
					record.Attempts = attempts
				}
				record.CreatedAt = now
				applyAuditEgress(&record, egressTrace, route.Provider)
				if s.providers.Inference(credential.Provider).QualityAutoDisable {
					if err := budget.run("quality_disable", finalizationHealthBudget, func(stageCtx context.Context) error {
						return s.applyOutputSpeedPenalty(stageCtx, record, credential, holdCfg, input.ForcedEgressNodeID != 0)
					}); err != nil {
						s.logger.Error("quality_degraded_penalty_failed", "request_id", input.RequestID, "account_id", credential.ID, "error", err)
					}
				}
				if supportsStoredResponses && operation == audit.OperationResponses && responseID != "" && successful {
					err := budget.run("response_ownership", finalizationOwnershipBudget, func(stageCtx context.Context) error {
						return s.responses.Save(stageCtx, inferencedomain.ResponseOwnership{ResponseID: responseID, AccountID: accountID, ClientKeyID: input.ClientKey.ID, ModelRouteID: route.ID, Provider: route.Provider, PromptCacheKey: ownershipPromptCacheKey, ReasoningReplayKey: reasoningReplayKey, ExpiresAt: now.Add(responseOwnershipTTL), CreatedAt: now, UpdatedAt: now})
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
			failureAttempts.captureStreamFailure(credential, upstreamStartedAt, response, diagnostic)
		}
		var markFirstToken func()
		if firstToken != nil {
			markFirstToken = firstToken.mark
		}
		timingHandedOff = true
		return &Result{StatusCode: response.StatusCode, Status: response.Status, Header: response.Header, Body: &finalizingBody{ReadCloser: response.Body, finalize: func() { finalize(Usage{}, "", "stream_closed") }}, MarkFirstToken: markFirstToken, RecordStreamFailure: recordStreamFailure, Finalize: finalize}
	}
	// fail_open retains at most one successful no-thinking stream. The account
	// lease is released immediately; the read pump applies upstream backpressure
	// until this response is either delivered or replaced by a better one.
	type qualityFallback struct {
		response          *provider.Response
		lease             *accountLease
		credential        accountdomain.Credential
		usage             Usage
		upstreamStartedAt time.Time
	}
	var fallback *qualityFallback
	discardFallback := func(recordDegraded bool) {
		if fallback == nil {
			return
		}
		if recordDegraded {
			s.recordQualityDegraded(ctx, auditBase, fallback.credential, fallback.usage, startedAt, egressTrace, route.Provider)
			failureAttempts.captureQualityDegraded(fallback.credential, fallback.upstreamStartedAt)
		}
		_ = fallback.response.Body.Close()
		fallback = nil
	}
attemptLoop:
	for attempt := 0; attemptPolicy.allows(attempt); attempt++ {
		if qualityHoldEnabled && qualityAccountAttempts >= holdCfg.MaxAttempts {
			break
		}
		var lease *accountLease
		var err error
		selectionStarted := time.Now()
		if input.ForcedAccountID != 0 {
			if input.ForcedEgressNodeID == 0 {
				err = &SelectionUnavailableError{Reason: SelectionNoAccounts}
			} else {
				lease, err = s.selector.AcquirePinnedForQualityProbe(ctx, route.Provider, input.ForcedAccountID, route.ID, route.UpstreamModel, quotaMode, accountScope)
				// An unbound account can still have reached the observed node through
				// runtime pool selection. The Provider request below carries the forced
				// node explicitly; only a conflicting concrete binding is invalid.
				if err == nil && lease.Credential.EgressNodeID != 0 && lease.Credential.EgressNodeID != input.ForcedEgressNodeID {
					lease.Release()
					lease = nil
					err = &SelectionUnavailableError{Reason: SelectionNoAccounts}
				}
			}
		} else if ownership != nil {
			lease, err = s.selector.AcquirePinnedForKey(ctx, route.Provider, ownership.AccountID, route.ID, route.UpstreamModel, quotaMode, true, accountScope)
		} else if input.ForcedEgressNodeID != 0 {
			lease, err = s.selector.AcquireForKeyOnEgressNode(ctx, route.Provider, route.ID, route.UpstreamModel, quotaMode, affinityKey, excluded, !quotaProbeAttempted, accountScope, input.ForcedEgressNodeID)
		} else {
			if selection == nil {
				selection, err = s.selector.beginSelectionSessionForKey(ctx, route.Provider, route.ID, route.UpstreamModel, quotaMode, affinityKey, excluded, !quotaProbeAttempted, accountScope)
			}
			if err == nil {
				lease, err = selection.Acquire(ctx, excluded, !quotaProbeAttempted)
			}
		}
		timing.markSelection(time.Since(selectionStarted))
		if err != nil {
			if lastFailure == nil {
				lastErr = err
			}
			pinnedID := uint64(0)
			if ownership != nil {
				pinnedID = ownership.AccountID
			} else if input.ForcedAccountID != 0 {
				pinnedID = input.ForcedAccountID
			}
			failureAttempts.captureSelectionFailure(pinnedID, "", err)
			break
		}
		excluded[lease.Credential.ID] = true
		if limited, ok := s.activeTeamModelRateLimit(lease.Credential, route.UpstreamModel, time.Now().UTC()); ok {
			lease.Release()
			lastFailure = &UpstreamFailure{
				HTTPStatus: http.StatusTooManyRequests, Code: "upstream_rate_limited", PublicMessage: "上游请求频率受限",
				AccountID: lease.Credential.ID, AccountName: lease.Credential.Name,
				Fingerprint: "429:team_model_rate_limit", RetryAfter: time.Until(limited.Until),
			}
			lastErr = fmt.Errorf("上游 Team 与模型请求频率受限")
			s.logger.Warn("upstream_team_model_rate_limit_active", "request_id", input.RequestID, "account_id", lease.Credential.ID, "provider", route.Provider, "model", route.UpstreamModel, "team_fingerprint", limited.TeamFingerprint, "retry_after", lastFailure.RetryAfter.Round(time.Second))
			// Stored Responses are pinned to one account. Return the cached 429
			// immediately instead of spinning until the cooldown expires or
			// replaying the request on the same account.
			if ownership != nil || input.ForcedAccountID != 0 {
				break attemptLoop
			}
			attempt--
			continue
		}
		if lease.QuotaProbe {
			quotaProbeAttempted = true
		}
		if lease.QuotaProbeKind == accountdomain.QuotaRecoveryKindPaid {
			recovered, probeErr := s.accounts.ProbePaidQuota(ctx, lease.Credential)
			s.selector.MarkQuotaStateChanged(lease.Credential.Provider, lease.Credential.ID)
			if probeErr != nil || !recovered {
				lease.Release()
				lastErr = firstError(probeErr, fmt.Errorf("付费额度尚未恢复"))
				continue
			}
			lease.QuotaProbe = false
			lease.QuotaProbeKind = ""
			lease.Billing = nil
		}
		credential, err := ensureCredential(lease.Credential, false)
		if err != nil {
			lease.Release()
			lastErr = err
			lastFailure = newCredentialUpstreamFailure(err, lease.Credential.ID, lease.Credential.Name)
			continue
		}
		if qualityHoldEnabled {
			qualityAccountAttempts++
		}
		response, err := forwardResponse(lease, credential, lease.Billing)
		if err != nil {
			lease.Release()
			lastErr = err
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				lastFailure = &UpstreamFailure{HTTPStatus: 499, Code: "request_canceled", PublicMessage: "请求已取消", AccountID: credential.ID, AccountName: credential.Name, Cause: firstError(ctx.Err(), err)}
				break
			}
			if isSSOCredentialRejected(err, credential) {
				s.markSSOCredentialRejected(ctx, credential, fmt.Sprintf("%s SSO credential rejected", credential.Provider))
				lastFailure = newHTTPUpstreamFailure(http.StatusUnauthorized, nil, credential.ID, credential.Name)
				continue
			}
			lastFailure = newTransportUpstreamFailure(err, credential.ID, credential.Name)
			if !isRetryableTransportFailure(failurePolicy, err) {
				break
			}
			responseFailure := false
			if status, retryAfter, classified := upstreamResponseErrorHealthPenalty(err, holdCfg.IdleAccountCooldown); classified {
				responseFailure = true
				writeCtx, writeCancel := context.WithTimeout(context.WithoutCancel(ctx), finalizationTimeout)
				markErr := s.selector.MarkFailureAfterSuccess(writeCtx, credential, status, retryAfter)
				writeCancel()
				if markErr != nil {
					s.logger.Warn("upstream_response_health_write_failed", "request_id", input.RequestID, "account_id", credential.ID, "error", markErr)
				} else {
					s.logger.Warn("upstream_response_health_retry", "request_id", input.RequestID, "account_id", credential.ID, "status", status, "minimum_cooldown", retryAfter)
				}
			} else {
				s.selector.MarkFailure(ctx, credential, 0, 0)
			}
			// The failed response is safe to retry on another account only when
			// doing so cannot repeat an upstream-hosted tool side effect.
			if responseFailure && qualityRequestHasReplayUnsafeHostedTools(input.Body) {
				break attemptLoop
			}
			if shouldStopForNonAccountFingerprint(failureFingerprints, lastFailure) {
				break
			}
			if isBuildTransientRetryError(err) {
				transientRetry++
			}
			continue
		}
	handleResponse:
		if response.ModelCatalogChanged {
			s.queueAccountModelSync(credential.ID)
		}
		if response.StatusCode == http.StatusUnauthorized {
			response.Body.Close()
			if credential.AuthType == accountdomain.AuthTypeSSO {
				s.markSSOCredentialRejected(ctx, credential, fmt.Sprintf("%s SSO credential rejected", credential.Provider))
				lease.Release()
				lastErr = fmt.Errorf("%s SSO 凭据已失效", credential.Provider)
				lastFailure = newHTTPUpstreamFailure(http.StatusUnauthorized, nil, credential.ID, credential.Name)
				continue
			}
			if s.markPermanentlyUnrefreshableCredentialRejected(ctx, credential) {
				lease.Release()
				lastErr = accountapp.ErrCredentialRefreshPermanent
				lastFailure = newHTTPUpstreamFailure(http.StatusUnauthorized, nil, credential.ID, credential.Name)
				continue
			}
			authRecoveryAttempted[credential.ID] = true
			refreshed, refreshErr := ensureCredential(credential, true)
			if refreshErr == nil {
				response, err = forwardResponse(lease, refreshed, lease.Billing)
				credential = refreshed
			}
			if refreshErr != nil || err != nil {
				if errors.Is(refreshErr, accountapp.ErrCredentialRefreshPermanent) {
					s.markCredentialRejectedAfterPermanentRefresh(ctx, credential)
				}
				s.selector.MarkRefreshDeferred(ctx, credential, refreshErr)
				lease.Release()
				lastErr = firstError(refreshErr, err)
				if refreshErr != nil {
					lastFailure = newCredentialUpstreamFailure(refreshErr, credential.ID, credential.Name)
				} else if ctx.Err() != nil || errors.Is(err, context.Canceled) {
					lastFailure = &UpstreamFailure{HTTPStatus: 499, Code: "request_canceled", PublicMessage: "请求已取消", AccountID: credential.ID, AccountName: credential.Name, Cause: firstError(ctx.Err(), err)}
					break
				} else {
					lastFailure = newTransportUpstreamFailure(err, credential.ID, credential.Name)
					if !isRetryableTransportFailure(failurePolicy, err) {
						break attemptLoop
					}
					if shouldStopForNonAccountFingerprint(failureFingerprints, lastFailure) {
						break attemptLoop
					}
					if isBuildTransientRetryError(err) {
						transientRetry++
					}
				}
				continue
			}
			if response.StatusCode == http.StatusUnauthorized {
				body, _ := readRetryableBody(response.Body)
				_ = s.accounts.MarkReauthRequired(ctx, credential.ID, fmt.Sprintf("%s OAuth credential rejected after refresh", credential.Provider))
				s.selector.MarkQuotaStateChanged(credential.Provider, credential.ID)
				lease.Release()
				lastErr = fmt.Errorf("刷新后上游仍返回 401")
				lastFailure = newHTTPUpstreamFailure(http.StatusUnauthorized, body, credential.ID, credential.Name)
				continue
			}
		}
		egressForbidden := s.providers.RetryForbiddenAsEgress(credential.Provider) && response.StatusCode == http.StatusForbidden
		finalEgressForbidden := egressForbidden && (attempt > 0 || !attemptPolicy.hasNext(attempt))
		// Classify 403 bodies before egress retry. Definitive blocked-account signals invalidate and rotate the account;
		// request-level safety rejections are returned as-is without account side effects;
		// all other 403 responses retain the egress retry path without penalizing the account.
		if response.StatusCode == http.StatusForbidden {
			retryAfter := parseRetryAfter(response.Header.Get("Retry-After"), time.Now().UTC())
			body, _ := readRetryableBody(response.Body)
			lastFailure = newHTTPUpstreamFailure(response.StatusCode, body, credential.ID, credential.Name)
			if isTerminalRequestForbidden(failurePolicy, lastFailure) {
				// Deterministic request-scoped 403: restore the original body and return it
				// without OAuth refresh, account rotation, cooldown, or invalidation.
				response.Body = io.NopCloser(bytes.NewReader(body))
				lease.completeSelectorObservation(false)
				lease.Release()
				if lastFailure.SafetyRejection {
					s.logger.Warn("upstream_safety_rejection", "request_id", input.RequestID, "account_id", credential.ID, "provider", credential.Provider, "status", response.StatusCode, "upstream_code", lastFailure.UpstreamCode)
				} else {
					s.logger.Warn("upstream_request_scoped_forbidden", "request_id", input.RequestID, "account_id", credential.ID, "provider", credential.Provider, "status", response.StatusCode, "upstream_code", lastFailure.UpstreamCode)
				}
				// Fall through to the common success/error response path so the client receives the original 403.
			} else if lastFailure.AccountBlocked {
				failureHandled := s.markReauthRequired(ctx, input.RequestID, credential, fmt.Sprintf("%s account is blocked", credential.Provider))
				if lastFailure.AccountScoped && !failureHandled {
					s.selector.MarkFailure(ctx, credential, response.StatusCode, retryAfter)
				}
				lease.Release()
				lastErr = fmt.Errorf("上游返回 %d", response.StatusCode)
				s.logger.Warn("upstream_request_failed", "request_id", input.RequestID, "account_id", credential.ID, "provider", credential.Provider, "status", response.StatusCode, "upstream_code", lastFailure.UpstreamCode, "account_scoped", lastFailure.AccountScoped, "account_blocked", true)
				continue
			} else if egressForbidden && !finalEgressForbidden {
				// A non-blocking 403 is an egress/browser-session failure and must not penalize the account.
				delete(excluded, credential.ID)
				if selection != nil {
					selection.RetryAccount(credential.ID)
				}
				lease.Release()
				lastErr = fmt.Errorf("上游出口会话被拒绝")
				continue
			} else {
				// Restore the consumed final non-blocking 403 body for the common response path.
				response.Body = io.NopCloser(bytes.NewReader(body))
			}
		}
		if isTerminalRequestForbidden(failurePolicy, lastFailure) {
			// already prepared as a terminal 403 response for the client
		} else if isRetryableResponse(response, failurePolicy) && !finalEgressForbidden {
			retryAfter := parseRetryAfter(response.Header.Get("Retry-After"), time.Now().UTC())
			body, _ := readRetryableBody(response.Body)
			lastFailure = newHTTPUpstreamFailure(response.StatusCode, body, credential.ID, credential.Name)
			if response.StatusCode == http.StatusTooManyRequests && response.RateLimit == nil {
				if metadata := provider.ParseRateLimitMetadata(body); metadata != nil {
					response.RateLimit = metadata
					if retryAfter <= 0 && metadata.RetryAfter > 0 {
						retryAfter = metadata.RetryAfter
					}
				}
			}
			forbiddenReauth := failurePolicy.AuthenticatedForbidden && s.shouldInvalidateBuildForbidden(lastFailure)
			if response.StatusCode == http.StatusTooManyRequests && response.RateLimit != nil && response.RateLimit.Model == route.UpstreamModel {
				rateLimitMeta := *response.RateLimit
				if strings.TrimSpace(rateLimitMeta.TeamID) == "" {
					rateLimitMeta.TeamID = strings.TrimSpace(credential.TeamID)
				}
				if rateLimitMeta.TeamID == "" {
					// Team+Model shielding requires a team identity; fall through to account-scoped 429 handling.
					goto afterTeamRateLimit
				}
				limited := s.markTeamModelRateLimit(credential, route.UpstreamModel, rateLimitMeta, time.Now().UTC())
				lastFailure.AccountScoped = false
				lastFailure.Fingerprint = "429:team_model_rate_limit"
				lastFailure.RetryAfter = time.Until(limited.Until)
				lease.Release()
				lastErr = fmt.Errorf("上游 Team 与模型请求频率受限")
				s.logger.Warn("upstream_team_model_rate_limited", "request_id", input.RequestID, "provider", credential.Provider, "model", route.UpstreamModel, "team_fingerprint", limited.TeamFingerprint, "scope", rateLimitMeta.Scope, "actual", rateLimitMeta.Actual, "limit", rateLimitMeta.Limit, "retry_after", lastFailure.RetryAfter)
				continue
			}
		afterTeamRateLimit:
			// An authenticated-forbidden provider treats only HTTP 401 as an OAuth
			// authentication failure. Its 403 must not trigger token rotation or
			// replay the same request with freshly issued credentials.
			if !failurePolicy.AuthenticatedForbidden && s.providers.SupportsCredentialRefresh(credential.Provider) && !authRecoveryAttempted[credential.ID] && credential.EncryptedRefreshToken != "" && !lastFailure.AccountBlocked && (lastFailure.PermanentAccountDenial || lastFailure.CredentialRejected) {
				authRecoveryAttempted[credential.ID] = true
				refreshed, refreshErr := ensureCredential(credential, true)
				if refreshErr != nil {
					s.selector.MarkRefreshDeferred(ctx, credential, refreshErr)
					lease.Release()
					lastErr = refreshErr
					lastFailure = newCredentialUpstreamFailure(refreshErr, credential.ID, credential.Name)
					continue attemptLoop
				}
				response, err = forwardResponse(lease, refreshed, lease.Billing)
				credential = refreshed
				if err != nil {
					lease.Release()
					lastErr = err
					if ctx.Err() != nil || errors.Is(err, context.Canceled) {
						lastFailure = &UpstreamFailure{HTTPStatus: 499, Code: "request_canceled", PublicMessage: "请求已取消", AccountID: credential.ID, AccountName: credential.Name, Cause: firstError(ctx.Err(), err)}
						break attemptLoop
					}
					lastFailure = newTransportUpstreamFailure(err, credential.ID, credential.Name)
					if !isRetryableTransportFailure(failurePolicy, err) {
						break attemptLoop
					}
					if shouldStopForNonAccountFingerprint(failureFingerprints, lastFailure) {
						break attemptLoop
					}
					if isBuildTransientRetryError(err) {
						transientRetry++
					}
					continue attemptLoop
				}
				goto handleResponse
			}
			failureHandled := false
			if lease.QuotaMode != "" && response.StatusCode == http.StatusTooManyRequests {
				state, reconcileErr := s.accounts.ReconcileRateLimit(ctx, credential.ID, lease.QuotaMode, retryAfter)
				s.applyRateLimitReconciliation(ctx, credential, response.StatusCode, retryAfter, state, reconcileErr)
				failureHandled = true
			} else if used, limit, exhausted := parseFreeQuotaExhaustion(body); exhausted {
				// The Free subscription signal is account-scoped, but its billing
				// period is not a reliable reset promise. Probe again after 24 hours.
				s.selector.MarkFreeQuotaExhausted(ctx, credential, used, limit)
				failureHandled = true
			} else if lastFailure.ModelQuotaExhausted {
				s.selector.MarkModelQuotaExhausted(ctx, credential, lease.Billing, route.UpstreamModel, retryAfter)
				failureHandled = true
			} else if lastFailure.FreeQuotaExhausted {
				s.selector.MarkFreeQuotaExhausted(ctx, credential, 0, 0)
				failureHandled = true
			} else if lastFailure.SpendingLimitBlocked || lastFailure.QuotaExhausted {
				err := s.selector.MarkPaymentQuotaExhausted(ctx, credential, quotaRecoveryHints{Billing: lease.Billing})
				failureHandled = err == nil
				if err != nil {
					s.logger.Error("account_quota_recovery_write_failed", "request_id", input.RequestID, "account_id", credential.ID, "provider", credential.Provider, "error", err)
				}
			}
			if lastFailure.AccountBlocked {
				failureHandled = s.markReauthRequired(ctx, input.RequestID, credential, fmt.Sprintf("%s account is blocked", credential.Provider))
			} else if forbiddenReauth {
				failureHandled = s.markReauthRequired(ctx, input.RequestID, credential, fmt.Sprintf("%s upstream error code %s matched the invalidation policy", credential.Provider, lastFailure.UpstreamCode))
			} else if s.providers.SupportsCredentialRefresh(credential.Provider) && lastFailure.PermanentAccountDenial {
				if failurePolicy.AuthenticatedForbidden {
					// 默认 model-scoped，视频拒绝时配额/OAuth 仍可能可用。
					// 开启 markBuildChatDeniedAsReauth 时再额外标 reauth，便于号池摘除。
					// 同时写入模型 block，避免在候选缓存窗口内本请求再次选中。
					modelErr := s.selector.MarkModelAccessDenied(ctx, credential, route.UpstreamModel, retryAfter)
					failureHandled = modelErr == nil
					if modelErr != nil {
						s.logger.Error("account_model_access_denied_write_failed", "request_id", input.RequestID, "account_id", credential.ID, "provider", credential.Provider, "model", route.UpstreamModel, "error", modelErr)
					}
					if s.markBuildChatDeniedAsReauth.Load() {
						reauthHandled := s.markReauthRequired(ctx, input.RequestID, credential, fmt.Sprintf("%s chat endpoint access denied", credential.Provider))
						failureHandled = failureHandled || reauthHandled
					}
				} else {
					failureHandled = s.markReauthRequired(ctx, input.RequestID, credential, fmt.Sprintf("%s chat endpoint access denied", credential.Provider))
				}
			} else if s.providers.SupportsCredentialRefresh(credential.Provider) && lastFailure.CredentialRejected {
				failureHandled = s.markReauthRequired(ctx, input.RequestID, credential, fmt.Sprintf("%s credential rejected", credential.Provider))
			}
			if !failureHandled && (lastFailure.AccountScoped || failurePolicy.AuthenticatedForbidden && response.StatusCode == http.StatusForbidden) {
				s.selector.MarkFailure(ctx, credential, response.StatusCode, retryAfter)
			} else if !lastFailure.AccountScoped && response.StatusCode >= http.StatusInternalServerError {
				// Provider-wide 5xx responses should rotate this request and briefly
				// isolate the account across requests, but must not grow the durable
				// account failure count exponentially. Preserve the real status in
				// health diagnostics while applying the explicit soft policy.
				if markErr := s.selector.markSoftFailure(ctx, credential, response.StatusCode, retryAfter); markErr != nil {
					s.logger.Warn("upstream_soft_cooldown_failed", "request_id", input.RequestID, "account_id", credential.ID, "provider", credential.Provider, "status", response.StatusCode, "error", markErr)
				}
			}
			lease.Release()
			lastErr = fmt.Errorf("上游返回 %d", response.StatusCode)
			s.logger.Warn("upstream_request_failed", "request_id", input.RequestID, "account_id", credential.ID, "provider", credential.Provider, "status", response.StatusCode, "upstream_code", lastFailure.UpstreamCode, "account_scoped", lastFailure.AccountScoped)
			if shouldStopForNonAccountFingerprint(failureFingerprints, lastFailure) {
				break
			}
			if isBuildTransientRetryStatus(response.StatusCode) {
				transientRetry++
			}
			continue
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			s.selector.markSuccess(ctx, credential, lease.QuotaProbe)
			if qualityHoldEnabled {
				replay, verdict, peekUsage, _, peekErr := peekQualityStream(ctx, response.Body, qualityProtocolForOperation(operation), holdCfg)
				if peekErr != nil {
					if replay != nil {
						_ = replay.Close()
					} else {
						_ = response.Body.Close()
					}
					lease.Release()
					lastErr = peekErr
					if isClientRequestCancel(ctx, peekErr) {
						lastFailure = &UpstreamFailure{HTTPStatus: 499, Code: "request_canceled", PublicMessage: "请求已取消", AccountID: credential.ID, AccountName: credential.Name, Cause: firstError(ctx.Err(), peekErr)}
						break
					}
					lastFailure = newTransportUpstreamFailure(peekErr, credential.ID, credential.Name)
					if neterrorpkg.IsUpstreamStreamIdleTimeout(peekErr) || neterrorpkg.IsUpstreamStreamIdleTimeout(context.Cause(ctx)) || errors.Is(peekErr, errQualityEmptyStream) {
						logPrefix := "quality_peek_idle"
						if errors.Is(peekErr, errQualityEmptyStream) {
							logPrefix = "quality_peek_empty"
						}
						writeCtx, writeCancel := context.WithTimeout(context.WithoutCancel(ctx), finalizationTimeout)
						if markErr := s.selector.MarkFailureAfterSuccess(writeCtx, credential, http.StatusGatewayTimeout, holdCfg.IdleAccountCooldown); markErr != nil {
							s.logger.Warn(logPrefix+"_cooldown_failed", "request_id", input.RequestID, "account_id", credential.ID, "error", markErr)
						} else {
							s.logger.Warn(logPrefix+"_retry", "request_id", input.RequestID, "account_id", credential.ID, "cooldown", holdCfg.IdleAccountCooldown)
						}
						writeCancel()
					}
					if !qualityCrossAccountReplay || shouldStopForNonAccountFingerprint(failureFingerprints, lastFailure) {
						break
					}
					if isBuildTransientRetryError(peekErr) {
						transientRetry++
					}
					continue
				}
				response.Body = replay
				hasNextAccount := qualityCrossAccountReplay && attemptPolicy.hasNext(attempt) && qualityAccountAttempts < holdCfg.MaxAttempts
				if hasNextAccount {
					hasNextAccount = selection != nil && selection.hasAvailableCandidate(excluded, !quotaProbeAttempted)
				}
				commit := CommitQualityHold(verdict, qualityAccountAttempts-1, holdCfg.MaxAttempts, hasNextAccount, holdCfg.OnExhausted)
				if verdict == QualityWithhold {
					s.applyMissingThinkingPenalty(ctx, input.RequestID, credential, holdCfg.AccountCooldown, holdCfg.AutoDisable)
				}
				deferFailOpenAudit := commit.Action == QualityActionRetry && holdCfg.OnExhausted == qualityRetryFailOpen
				if commit.Audit && !deferFailOpenAudit {
					s.recordQualityDegraded(ctx, auditBase, credential, peekUsage, startedAt, egressTrace, route.Provider)
					failureAttempts.captureQualityDegraded(credential, responseStartedAt)
				}
				switch commit.Action {
				case QualityActionRetry:
					if deferFailOpenAudit {
						discardFallback(true)
						fallback = &qualityFallback{response: response, lease: lease, credential: credential, usage: peekUsage, upstreamStartedAt: responseStartedAt}
						lease.completeSelectorObservation(true)
						lease.Release()
					} else {
						_ = response.Body.Close()
						lease.Release()
					}
					lastErr = errQualityDegraded
					lastFailure = &UpstreamFailure{
						HTTPStatus: http.StatusServiceUnavailable, Code: ErrorQualityDegraded,
						PublicMessage: "上游响应缺少推理", AccountID: credential.ID, AccountName: credential.Name,
						Cause: errQualityDegraded,
					}
					s.logger.Info("quality_degraded_retry", "request_id", input.RequestID, "account_id", credential.ID, "quality_attempt", qualityAccountAttempts, "output_tokens", peekUsage.OutputTokens)
					continue
				case QualityActionReject:
					_ = response.Body.Close()
					lease.Release()
					lastErr = errQualityDegraded
					lastFailure = &UpstreamFailure{
						HTTPStatus: http.StatusServiceUnavailable, Code: ErrorQualityDegraded,
						PublicMessage: "上游响应缺少推理", AccountID: credential.ID, AccountName: credential.Name,
						Cause: errQualityDegraded,
					}
					s.logger.Info("quality_degraded_rejected", "request_id", input.RequestID, "account_id", credential.ID)
					break attemptLoop
				case QualityActionDeliverLast:
					discardFallback(true)
					s.logger.Info("quality_degraded_deliver_last", "request_id", input.RequestID, "account_id", credential.ID, "quality_attempt", qualityAccountAttempts, "output_tokens", peekUsage.OutputTokens)
				case QualityActionDeliver:
					discardFallback(true)
				}
				if !commit.KeepBody {
					_ = response.Body.Close()
					lease.Release()
					break attemptLoop
				}
			}
			if diagnostic := response.RecoveredPrimaryFailure; diagnostic != nil {
				recoveredFailure := newHTTPUpstreamFailure(diagnostic.StatusCode, diagnostic.Body, credential.ID, credential.Name)
				if recoveredFailure.AccountBlocked || (failurePolicy.AuthenticatedForbidden && s.shouldInvalidateBuildForbidden(recoveredFailure)) {
					reason := fmt.Sprintf("%s primary endpoint denied account access", credential.Provider)
					if !s.markReauthRequired(ctx, input.RequestID, credential, reason) {
						s.selector.MarkModelAccessDenied(ctx, credential, route.UpstreamModel, 0)
					}
				}
			}
		}
		if fallback != nil && holdCfg.OnExhausted == qualityRetryFailOpen {
			_ = response.Body.Close()
			lease.completeSelectorObservation(false)
			lease.Release()
			selected := fallback
			fallback = nil
			s.logger.Info("quality_degraded_fallback", "request_id", input.RequestID, "account_id", selected.credential.ID, "quality_attempts", qualityAccountAttempts)
			return handoffResponse(selected.response, selected.lease, selected.credential, selected.upstreamStartedAt), nil
		}
		return handoffResponse(response, lease, credential, responseStartedAt), nil
	}
	if fallback != nil {
		if ctx.Err() == nil && holdCfg.OnExhausted == qualityRetryFailOpen {
			selected := fallback
			fallback = nil
			s.logger.Info("quality_degraded_fallback", "request_id", input.RequestID, "account_id", selected.credential.ID, "quality_attempts", qualityAccountAttempts)
			return handoffResponse(selected.response, selected.lease, selected.credential, selected.upstreamStartedAt), nil
		}
		discardFallback(true)
	}
	if lastFailure != nil {
		record := auditBase
		record.StatusCode = lastFailure.HTTPStatus
		record.ErrorCode = lastFailure.AuditCode()
		record.Attempts = failureAttempts.snapshot()
		s.persistFailureAudit(record, startedAt, egressTrace, &accountdomain.Credential{ID: lastFailure.AccountID, Name: lastFailure.AccountName})
		return nil, lastFailure
	}
	if lastErr == nil {
		lastErr = ErrNoAvailableAccount
	}
	record := auditBase
	record.StatusCode = http.StatusServiceUnavailable
	record.ErrorCode = "upstream_unavailable"
	failedAccount := accountdomain.Credential{}
	var selectionFailure *SelectionUnavailableError
	if errors.As(lastErr, &selectionFailure) {
		record.StatusCode = selectionFailure.HTTPStatus()
		record.ErrorCode = selectionFailure.Code()
		failedAccount = accountdomain.Credential{ID: selectionFailure.AccountID, Name: selectionFailure.AccountName}
	}
	if failedAccount.ID == 0 && ownership != nil {
		failedAccount.ID = ownership.AccountID
	}
	record.Attempts = failureAttempts.snapshot()
	s.persistFailureAudit(record, startedAt, egressTrace, &failedAccount)
	return nil, fmt.Errorf("%w: %w", ErrNoAvailableAccount, lastErr)
}
