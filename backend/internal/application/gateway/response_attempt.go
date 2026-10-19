package gateway

import (
	"context"
	"errors"
	"net/http"
	"time"

	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/requestmeta"
)

// responseAttempt carries one Responses-family request through route
// resolution, the account attempt loop and the hand-off of the delivered
// upstream response. Its fields are the state those phases share.
type responseAttempt struct {
	s     *Service
	ctx   context.Context
	input Input
	path  string

	egressTrace *infraegress.Trace
	startedAt   time.Time
	firstToken  *firstTokenTimer
	eventID     string

	operation      audit.Operation
	auditOperation audit.Operation
	ownership      *inferencedomain.ResponseOwnership
	route          modeldomain.Route
	publicModel    string
	accountScope   clientkey.AccountScope
	selection      *selectionSession

	timing          *generationTiming
	timingHandedOff bool

	usageSource               audit.UsageSource
	auditBase                 audit.Record
	affinityKey               string
	ownershipPromptCacheKey   string
	reasoningReplayKey        string
	adapter                   provider.ResponseAdapter
	physicalCallCtx           context.Context
	supportsStoredResponses   bool
	holdCfg                   QualityRetryRuntime
	qualityHoldEnabled        bool
	qualityCrossAccountReplay bool
	attemptPolicy             routingAttemptPolicy
	idempotencyID             string
	pricingModel              string
	quotaMode                 string
	failurePolicy             provider.FailurePolicy

	failureAttempts    *failureAttemptRecorder
	normalizedMetadata *provider.NormalizedRequestMetadata
	responseStartedAt  time.Time
	transientRetry     int
	fallback           *responseQualityFallback
}

// newResponseAttempt resolves the route, the previous Response ownership and
// the public model. modelNotAllowed reports a route the client key may not
// use; prepare audits and rejects it once generation timing is running.
func (s *Service) newResponseAttempt(ctx context.Context, input Input, path string) (*responseAttempt, bool, error) {
	ctx, egressTrace := infraegress.WithTrace(ctx)
	a := &responseAttempt{s: s, ctx: ctx, input: input, path: path, egressTrace: egressTrace, startedAt: time.Now()}
	if input.Streaming {
		a.firstToken = newFirstTokenTimer(a.startedAt)
	}
	a.eventID = newAuditEventID()
	a.operation = input.Operation
	if a.operation == "" {
		a.operation = audit.OperationResponses
	}
	a.auditOperation = a.operation
	if input.auditOperation != "" {
		a.auditOperation = input.auditOperation
	}
	routes, aliasEffort, err := s.resolvePublicModelRoutes(ctx, input.PublicModel, input.ClientKey.AllowModelAliases)
	if err != nil {
		return nil, false, ErrModelNotFound
	}
	if err := a.resolveOwnership(routes); err != nil {
		return nil, false, err
	}
	routeErr := a.selectRoute(routes)
	a.publicModel = modeldomain.ExternalPublicID(a.route.Provider, a.route.PublicID)
	a.input.PublicModel = a.publicModel
	if aliasEffort != "" {
		a.input.Body, err = rewriteAliasedModel(a.input.Body, a.publicModel, aliasEffort, a.operation)
		if err != nil {
			return nil, false, err
		}
	}
	if routeErr != nil && !errors.Is(routeErr, clientkeyapp.ErrModelNotAllowed) {
		return nil, false, routeErr
	}
	a.timing = newGenerationTiming(a.publicModel, a.route.Provider)
	return a, errors.Is(routeErr, clientkeyapp.ErrModelNotAllowed), nil
}

// resolveOwnership selects an initial route only to preserve the existing
// stateful/stateless previous_response_id boundary. The actual target is
// chosen from the eligible pool after ownership and account availability are
// known.
func (a *responseAttempt) resolveOwnership(routes []modeldomain.Route) error {
	s := a.s
	initialRoute, routeErr := s.selectConversationRoute(routes, a.input.ClientKey, a.operation, a.path, false, nil)
	if a.input.PreviousResponseID != "" && routeErr == nil {
		if s.providers.SupportsStoredResponses(initialRoute.Provider) {
			value, ownershipErr := s.responses.Get(a.ctx, a.input.PreviousResponseID, a.input.ClientKey.ID, time.Now().UTC())
			if ownershipErr != nil {
				return ErrResponseNotFound
			}
			a.ownership = &value
		} else if s.providers.SupportsStatelessPreviousResponse(initialRoute.Provider) {
			// The provider retains no Response state, so replay the history statelessly here;
			// Provider normalization removes stale Response IDs.
			a.input.PreviousResponseID = ""
		} else {
			return ErrResponseStateUnsupported
		}
	}
	return nil
}

// selectRoute picks the target from the eligible routes and returns the
// eligibility error, if any.
func (a *responseAttempt) selectRoute(routes []modeldomain.Route) error {
	s, input := a.s, a.input
	// Use a server-generated scope so repeated or absent client request IDs
	// cannot accidentally join independent Composer conversations.
	requestSessionScope := a.eventID
	eligibleRoutes, fallbackRoute, routeErr := s.eligibleConversationRoutes(routes, input.ClientKey, a.operation, a.path, a.ownership != nil, a.ownership)
	a.route = fallbackRoute
	orderedRoutes := eligibleRoutes
	if routeErr == nil {
		orderedRoutes = orderConversationRouteTargets(eligibleRoutes, routeTargetSeed(input))
		a.route = orderedRoutes[0]
	}
	a.accountScope = input.ClientKey.AccountScope()
	// Skip targets whose account pool is already known to be unavailable. This
	// gives same-name targets failover before any physical upstream request while
	// preserving pinned Responses and forced administrator probes.
	if routeErr == nil && a.ownership == nil && input.ForcedEgressNodeID == 0 {
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
				a.ctx,
				candidate.Provider,
				candidate.ID,
				candidate.UpstreamModel,
				s.providers.QuotaMode(candidate.Provider, candidate.UpstreamModel),
				affinityKey,
				nil,
				true,
				a.accountScope,
			)
			if selectionErr != nil {
				continue
			}
			a.route = candidate
			a.selection = candidateSession
			break
		}
	}
	return routeErr
}

// prepare builds the audit base, session identity and attempt policies and
// reserves billing. It rejects a model the client key may not use.
func (a *responseAttempt) prepare(modelNotAllowed bool) error {
	s, ctx, route := a.s, a.ctx, a.route
	a.usageSource = audit.UsageSourceUpstream
	if usageKind, _ := s.providers.UsageKind(route.Provider); usageKind == provider.UsageEstimated {
		a.usageSource = audit.UsageSourceEstimated
	}
	mediaSummary, _ := summarizeResponseMedia(a.input.Body)
	logResponseMediaSummary(s.logger, a.input.RequestID, mediaSummary)
	a.auditBase = audit.Record{
		EventID: a.eventID, RequestID: a.input.RequestID, ClientKeyID: a.input.ClientKey.ID, ClientKeyName: a.input.ClientKey.Name,
		ClientIP:     requestmeta.ClientIP(ctx),
		ModelRouteID: route.ID, ModelPublicID: a.publicModel, ModelUpstreamModel: modeldomain.DisplayUpstreamModel(route.Provider, route.UpstreamModel),
		Provider: string(route.Provider), Operation: a.auditOperation, UsageSource: audit.UsageSourceNone, Streaming: a.input.Streaming,
		MediaInputImages: mediaSummary.InputImages,
		RequestMethod:    a.input.Method, RequestPath: a.input.Path, RequestHeaders: a.input.Headers,
	}
	if modelNotAllowed {
		record := a.auditBase
		record.StatusCode = http.StatusForbidden
		record.DurationMS = time.Since(a.startedAt).Milliseconds()
		record.ErrorCode = "model_not_allowed"
		record.CreatedAt = time.Now().UTC()
		applyAuditEgress(&record, a.egressTrace, route.Provider)
		if err := s.audits.Create(ctx, record); err != nil {
			s.logger.Error("request_usage_write_failed", "event_id", record.EventID, "request_id", a.input.RequestID, "error", err)
		}
		return clientkeyapp.ErrModelNotAllowed
	}
	a.applySessionIdentity()
	adapter, ok := s.providers.Responses(route.Provider)
	if !ok {
		return ErrNoAvailableAccount
	}
	a.adapter = adapter
	a.physicalCallCtx = infraegress.WithPhysicalCallTrace(ctx, string(route.Provider), string(a.operation))
	a.supportsStoredResponses = s.providers.SupportsStoredResponses(route.Provider)
	if a.input.PreviousResponseID != "" && !a.supportsStoredResponses {
		return ErrResponseStateUnsupported
	}
	// A lease recovery probe stays on exactly one account and one rendered proxy
	// identity. Retrying the same pinned account would provide neither failover
	// nor new evidence and can multiply a slow/failing probe.
	a.holdCfg = s.qualityRetryConfig()
	a.qualityHoldEnabled = s.providers.Inference(route.Provider).QualityHold && shouldHoldQualityStream(a.input, a.ownership, route, a.operation, a.holdCfg)
	a.qualityCrossAccountReplay = canReplayQualityHoldAcrossAccounts(a.input, a.ownership)
	a.attemptPolicy = newRequestRoutingAttemptPolicy(int(s.maxAttempts.Load()), a.ownership != nil || a.input.ForcedAccountID != 0)
	a.idempotencyID, _ = security.NewOpaqueToken(18)
	a.pricingModel = s.providers.PricingModel(route.Provider, route.UpstreamModel)
	if err := s.checkLedgerReady(); err != nil {
		return err
	}
	if reservation, priced := s.pricing.QuoteTextReservation(route.Provider, a.pricingModel, a.input.Body); priced {
		if _, err := s.clientKeys.ReserveBilling(ctx, a.input.ClientKey, a.eventID, reservation.CostInUSDTicks, s.textBillingReservationTTL()); err != nil {
			return err
		}
	}
	a.quotaMode = s.providers.QuotaMode(route.Provider, route.UpstreamModel)
	a.failurePolicy = s.providers.Failure(route.Provider)
	a.failureAttempts = newFailureAttemptRecorder(http.MethodPost, a.path)
	a.normalizedMetadata = &provider.NormalizedRequestMetadata{}
	a.responseStartedAt = a.startedAt
	return nil
}

// applySessionIdentity derives a stable identity from explicit session
// signals, message anchors, and model. Composer replaces message-only fallback
// identities with an isolated request identity that remains stable across
// retries.
func (a *responseAttempt) applySessionIdentity() {
	s, route := a.s, a.route
	if !s.providers.Inference(route.Provider).SessionIdentity {
		return
	}
	identity := sessionIdentity{}
	if a.ownership != nil && a.ownership.PromptCacheKey != "" {
		// previous_response_id belongs to an existing Response chain and must inherit the root session identity;
		// do not recompute the soft key from this turn's incremental input.
		identity.upstreamID = a.ownership.PromptCacheKey
		identity.replayKey = a.ownership.ReasoningReplayKey
	} else {
		identity = resolveSessionIdentity(
			a.input.ClientKey.ID,
			route.Provider,
			route.UpstreamModel,
			a.input.PromptCacheKey,
			a.input.PromptCacheSeed,
			a.input.Body,
		)
	}
	identity = ensureComposerSessionIdentity(identity, a.input.ClientKey.ID, route.Provider, route.UpstreamModel, a.eventID)
	a.input.PromptCacheKey = identity.upstreamID
	a.affinityKey = identity.affinityKey
	a.ownershipPromptCacheKey = identity.upstreamID
	a.reasoningReplayKey = identity.replayKey
	if identity.upstreamID == "" {
		s.logger.Debug("prompt_cache_session_empty", "request_id", a.input.RequestID, "model", route.UpstreamModel, "provider", route.Provider)
	} else if identity.soft {
		s.logger.Debug("prompt_cache_session_soft", "request_id", a.input.RequestID, "model", route.UpstreamModel)
	} else if identity.isolated {
		s.logger.Debug("prompt_cache_session_isolated", "request_id", a.input.RequestID, "model", route.UpstreamModel)
	}
}

// finishTiming reports the generation timing as failed unless a delivered
// response took it over.
func (a *responseAttempt) finishTiming() {
	if !a.timingHandedOff {
		a.timing.finish(a.s.logger, "failed")
	}
}

// ensureCredential loads a usable credential for the leased account. force is
// set only after the upstream rejected the credential.
func (a *responseAttempt) ensureCredential(credential accountdomain.Credential, force bool) (accountdomain.Credential, error) {
	started := time.Now()
	var result accountdomain.Credential
	var err error
	if force {
		// Called only after the upstream rejected the credential.
		result, err = a.s.accounts.RenewRejectedCredential(a.ctx, credential)
	} else {
		result, err = a.s.accounts.EnsureCredential(a.ctx, credential, false)
	}
	a.failureAttempts.captureCredentialFailure(credential, started, force, err)
	a.timing.markCredential(time.Since(started))
	return result, err
}

// forward sends the request upstream on the leased account.
func (a *responseAttempt) forward(lease *accountLease, credential accountdomain.Credential, billing *accountdomain.Billing) (*provider.Response, error) {
	input, route := a.input, a.route
	started := time.Now()
	a.responseStartedAt = started
	lease.markSelectorUpstreamStarted()
	// Assets the provider saves while answering (e.g. images drawn in chat) carry this request's provenance.
	originCtx := mediadomain.WithOrigin(a.physicalCallCtx, mediadomain.Origin{
		Provider: string(route.Provider), Model: a.publicModel, UpstreamModel: a.auditBase.ModelUpstreamModel,
		ClientKeyID: input.ClientKey.ID, ClientKeyName: input.ClientKey.Name, AccountID: credential.ID, AccountName: credential.Name,
		Operation: string(a.auditOperation), RequestID: input.RequestID,
	})
	response, err := a.adapter.ForwardResponse(originCtx, provider.ResponseResourceRequest{Credential: credential, ForcedEgressNodeID: input.ForcedEgressNodeID, Billing: billing, Method: http.MethodPost, Path: a.path, Model: route.UpstreamModel, PromptCacheKey: input.PromptCacheKey, ReasoningReplayKey: a.reasoningReplayKey, AllowClientToolCacheRoute: input.AllowClientToolCacheRoute, GrokTurnIndex: input.GrokTurnIndex, TransientRetry: a.transientRetry, IdempotencyID: a.idempotencyID, Body: input.Body, Streaming: input.Streaming, NormalizeBody: true, Operation: string(a.operation), NormalizedMetadata: a.normalizedMetadata})
	a.auditBase.ReasoningEffort = a.normalizedMetadata.ReasoningEffort
	err = a.failureAttempts.captureResponse(credential, started, response, err)
	a.timing.markUpstream(time.Since(started))
	return response, err
}
