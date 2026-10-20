package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/pkg/requestmeta"
)

// ImageGenerationInput 表示图片生成用例已经完成协议校验后的输入。
type ImageGenerationInput struct {
	RequestID      string
	ClientKey      clientkey.Key
	PublicModel    string
	Prompt         string
	Count          int
	Size           string
	AspectRatio    string
	Resolution     string
	Quality        string
	ResponseFormat string
	OutputFormat   string
	Background     string
	Streaming      bool
	PartialImages  int
	Method         string
	Path           string
	Headers        map[string][]string
}

// ImageEditInput 表示图片编辑用例已经完成协议校验后的输入；ImageURLs 与 MaskURL 为 data URI 或 https URL。
type ImageEditInput struct {
	RequestID      string
	ClientKey      clientkey.Key
	PublicModel    string
	Prompt         string
	ImageURLs      []string
	MaskURL        string
	Count          int
	Size           string
	AspectRatio    string
	Resolution     string
	Quality        string
	ResponseFormat string
	OutputFormat   string
	Background     string
	Streaming      bool
	PartialImages  int
	Method         string
	Path           string
	Headers        map[string][]string
}

// ImageRequestRejectedError carries a provider's client-safe rejection of the
// image parameters after every same-name route rejected the request.
type ImageRequestRejectedError struct{ Err error }

func (e *ImageRequestRejectedError) Error() string { return e.Err.Error() }

func (e *ImageRequestRejectedError) Unwrap() error { return e.Err }

type imageProviderSupport func(accountdomain.Provider) bool

type imageExecution func(context.Context, accountdomain.Provider, accountdomain.Credential, string) (*provider.Response, error)

// GenerateImage 选择支持图片生成的路由和账号，并返回可统一审计的上游响应。
func (s *Service) GenerateImage(ctx context.Context, input ImageGenerationInput) (*Result, error) {
	if s.fansOutImages(ctx, input.PublicModel, input.Count, input.Streaming) {
		return s.fanOutImages(ctx, input.Count, func(ctx context.Context, extra bool) (*Result, error) {
			single := input
			single.Count = 1
			return s.generateImage(ctx, single, extra)
		})
	}
	return s.generateImage(ctx, input, false)
}

func (s *Service) generateImage(ctx context.Context, input ImageGenerationInput, extra bool) (*Result, error) {
	options := provider.ImageRequestOptions{
		Count: input.Count, Size: input.Size, AspectRatio: input.AspectRatio, Resolution: input.Resolution, Quality: input.Quality,
		ResponseFormat: input.ResponseFormat, OutputFormat: input.OutputFormat, Background: input.Background,
		Streaming: input.Streaming, PartialImages: input.PartialImages,
	}
	return s.executeImage(ctx, input.RequestID, input.ClientKey, input.PublicModel, input.Prompt, audit.OperationImage, modeldomain.CapabilityImage, func(providerValue accountdomain.Provider) bool {
		_, ok := s.providers.ImageGeneration(providerValue)
		return ok
	}, func(executionCtx context.Context, providerValue accountdomain.Provider, credential accountdomain.Credential, upstream string) (*provider.Response, error) {
		adapter, ok := s.providers.ImageGeneration(providerValue)
		if !ok {
			return nil, ErrNoAvailableAccount
		}
		return adapter.GenerateImage(executionCtx, provider.ImageGenerationRequest{
			Credential: credential, Model: upstream, Prompt: input.Prompt, Count: input.Count,
			Size: input.Size, AspectRatio: input.AspectRatio, Resolution: input.Resolution, Quality: input.Quality,
			ResponseFormat: input.ResponseFormat, Streaming: input.Streaming, PartialImages: input.PartialImages,
			OutputFormat: input.OutputFormat, Background: input.Background,
		})
	}, options, input.Method, input.Path, input.Headers, extra)
}

// EditImage 选择支持图片编辑的路由和账号，并返回可统一审计的上游响应。
func (s *Service) EditImage(ctx context.Context, input ImageEditInput) (*Result, error) {
	if s.fansOutImages(ctx, input.PublicModel, input.Count, input.Streaming) {
		return s.fanOutImages(ctx, input.Count, func(ctx context.Context, extra bool) (*Result, error) {
			single := input
			single.Count = 1
			return s.editImage(ctx, single, extra)
		})
	}
	return s.editImage(ctx, input, false)
}

func (s *Service) editImage(ctx context.Context, input ImageEditInput, extra bool) (*Result, error) {
	options := provider.ImageRequestOptions{
		Edit: true, Count: input.Count, InputImages: len(input.ImageURLs), HasMask: input.MaskURL != "",
		Size: input.Size, AspectRatio: input.AspectRatio, Resolution: input.Resolution, Quality: input.Quality,
		ResponseFormat: input.ResponseFormat, OutputFormat: input.OutputFormat, Background: input.Background,
		Streaming: input.Streaming, PartialImages: input.PartialImages,
	}
	return s.executeImage(ctx, input.RequestID, input.ClientKey, input.PublicModel, input.Prompt, audit.OperationImageEdit, modeldomain.CapabilityImageEdit, func(providerValue accountdomain.Provider) bool {
		_, ok := s.providers.ImageEdit(providerValue)
		return ok
	}, func(executionCtx context.Context, providerValue accountdomain.Provider, credential accountdomain.Credential, upstream string) (*provider.Response, error) {
		adapter, ok := s.providers.ImageEdit(providerValue)
		if !ok {
			return nil, ErrNoAvailableAccount
		}
		return adapter.EditImage(executionCtx, provider.ImageEditRequest{
			Credential: credential, Model: upstream, Prompt: input.Prompt,
			ImageURLs: input.ImageURLs, Count: input.Count, Size: input.Size, AspectRatio: input.AspectRatio,
			Resolution: input.Resolution, Quality: input.Quality, ResponseFormat: input.ResponseFormat,
			Streaming: input.Streaming, PartialImages: input.PartialImages,
			OutputFormat: input.OutputFormat, Background: input.Background, MaskURL: input.MaskURL,
		})
	}, options, input.Method, input.Path, input.Headers, extra)
}

func (s *Service) executeImage(
	ctx context.Context,
	requestID string,
	key clientkey.Key,
	publicModel string,
	prompt string,
	operation audit.Operation,
	capability modeldomain.Capability,
	supports imageProviderSupport,
	execute imageExecution,
	options provider.ImageRequestOptions,
	method string,
	path string,
	headers map[string][]string,
	// extra marks an additional image of a fanned-out request: it gives up
	// without an audit when no account is free (errImageAccountsBusy).
	extra bool,
) (*Result, error) {
	ctx, egressTrace := infraegress.WithTrace(ctx)
	startedAt := time.Now()
	eventID := newAuditEventID()
	streaming, resolution, quality := options.Streaming, options.Resolution, options.Quality
	requestedCount, inputImageCount := options.Count, options.InputImages
	routes, err := s.models.GetByPublicIDCandidates(ctx, publicModel)
	if err != nil {
		return nil, ErrModelNotFound
	}
	// Routes whose provider rejects the parameters are dropped; the first
	// rejection surfaces only when no other target remains.
	var rejection error
	supportsRequest := func(providerValue accountdomain.Provider) bool {
		if !supports(providerValue) {
			return false
		}
		validator, ok := s.providers.ImageRequestValidator(providerValue)
		if !ok {
			return true
		}
		err := validator.ValidateImageRequest(options)
		if err != nil && rejection == nil {
			rejection = &ImageRequestRejectedError{Err: err}
		}
		return err == nil
	}
	route, preselectedSession, err := s.selectSchedulableMediaRoute(ctx, routes, key, capability, true, supportsRequest)
	if err != nil && extra {
		return nil, errImageAccountsBusy
	}
	if err != nil {
		// Preserve the established failure-audit path when every eligible target
		// is currently unschedulable. The request loop will reproduce the
		// selection error for the representative route after auditBase exists.
		route, err = s.selectMediaRoute(routes, key, capability, supportsRequest)
		if err != nil {
			if rejection != nil && errors.Is(err, ErrNoAvailableAccount) {
				return nil, rejection
			}
			return nil, err
		}
		preselectedSession = nil
	}
	externalModel := modeldomain.ExternalPublicID(route.Provider, route.PublicID)
	auditBase := audit.Record{
		EventID: eventID, RequestID: requestID, ClientKeyID: key.ID, ClientKeyName: key.Name,
		ClientIP:     requestmeta.ClientIP(ctx),
		ModelRouteID: route.ID, ModelPublicID: externalModel, ModelUpstreamModel: modeldomain.DisplayUpstreamModel(route.Provider, route.UpstreamModel),
		Provider: string(route.Provider), Operation: operation, UsageSource: audit.UsageSourceNone, Streaming: streaming,
		RequestMethod: method, RequestPath: path, RequestHeaders: headers,
	}
	if operation == audit.OperationImageEdit {
		auditBase.MediaInputImages = int64(max(0, inputImageCount))
	}
	if err := s.checkLedgerReady(); err != nil {
		return nil, err
	}
	failureAttempts := newFailureAttemptRecorder(method, path)
	writeFailureAudit := func(statusCode int, errorCode string, credential *accountdomain.Credential) {
		record := auditBase
		record.StatusCode = statusCode
		record.ErrorCode = errorCode
		record.Attempts = failureAttempts.snapshot()
		s.persistFailureAudit(record, startedAt, egressTrace, credential)
	}
	pricingModel := s.providers.PricingModel(route.Provider, route.UpstreamModel)
	pricingResolution, pricingQuality := resolution, quality
	if definition, _ := s.providers.Definition(route.Provider); definition.Media.ImageGenerationModelPriced && operation == audit.OperationImage {
		// The product is selected through the catalog model and only
		// aspect_ratio/n are forwarded. Do not reserve or record a price tier
		// derived from resolution/quality compatibility fields.
		pricingResolution, pricingQuality = "", ""
	}
	var reservation audit.PricingResult
	var priced bool
	switch operation {
	case audit.OperationImage:
		reservation, priced = s.pricing.QuoteImage(route.Provider, pricingModel, pricingResolution, pricingQuality, requestedCount)
	case audit.OperationImageEdit:
		reservation, priced = s.pricing.QuoteImageEdit(route.Provider, pricingModel, pricingResolution, pricingQuality, requestedCount, inputImageCount)
	}
	reserved := false
	if priced {
		reserved, err = s.clientKeys.ReserveBilling(ctx, key, eventID, reservation.CostInUSDTicks, mediaBillingReservationTTL)
		if err != nil {
			return nil, err
		}
	}
	finalizationOwnsReservation := false
	defer func() {
		if reserved && !finalizationOwnsReservation {
			s.cancelBillingReservation(eventID)
		}
	}()
	quotaMode := s.providers.QuotaMode(route.Provider, route.UpstreamModel)
	quotaRefreshGroup := s.providers.QuotaRefreshGroup(route.Provider, route.UpstreamModel)
	attemptPolicy := newRoutingAttemptPolicy(int(s.maxAttempts.Load()))
	excluded := make(map[uint64]bool)
	authRetried := make(map[uint64]bool)
	selection := preselectedSession
	var lease *accountLease
	var credential accountdomain.Credential
	var response *provider.Response
	var lastCredentialFailure *accountdomain.Credential
	var lastCredentialError error
	for attempt := 0; attemptPolicy.allows(attempt); attempt++ {
		// A skipped attempt must not leave its error or response to the next one.
		err, response = nil, nil
		if selection == nil {
			selection, err = s.selector.beginSelectionSessionForKey(ctx, route.Provider, route.ID, route.UpstreamModel, quotaMode, "", excluded, false, key.AccountScope())
		}
		if err == nil {
			lease, err = selection.Acquire(ctx, excluded, false)
		}
		if err != nil {
			if extra && attempt == 0 {
				return nil, errImageAccountsBusy
			}
			errorCode := "upstream_unavailable"
			var selectionFailure *SelectionUnavailableError
			if errors.As(err, &selectionFailure) {
				errorCode = selectionFailure.Code()
			}
			writeFailureAudit(http.StatusServiceUnavailable, errorCode, lastCredentialFailure)
			return nil, fmt.Errorf("%w: %w", ErrNoAvailableAccount, err)
		}
		excluded[lease.Credential.ID] = true
		attemptStarted := time.Now()
		credential, err = s.accounts.EnsureCredential(ctx, lease.Credential, false)
		if err != nil {
			failureAttempts.captureCredentialFailure(lease.Credential, attemptStarted, false, err)
			s.logger.Error("image_credential_failed", "event_id", eventID, "request_id", requestID, "model", externalModel, "provider", route.Provider, "account_id", lease.Credential.ID, "error", err)
			failedCredential := lease.Credential
			lastCredentialFailure = &failedCredential
			lastCredentialError = err
			lease.Release()
			continue
		}
		lease.markSelectorUpstreamStarted()
		// Assets the provider saves for this attempt carry its provenance.
		originCtx := mediadomain.WithOrigin(ctx, mediadomain.Origin{
			Provider: string(route.Provider), Model: externalModel, UpstreamModel: auditBase.ModelUpstreamModel,
			ClientKeyID: key.ID, ClientKeyName: key.Name, AccountID: credential.ID, AccountName: credential.Name,
			Operation: string(operation), RequestID: requestID, Prompt: prompt,
		})
		attemptStarted = time.Now()
		response, err = execute(originCtx, route.Provider, credential, route.UpstreamModel)
		// Failed attempts (errors and non-2xx responses) appear in the request diagnostics.
		_ = failureAttempts.captureResponse(credential, attemptStarted, response, err)
		if err != nil {
			s.logger.Error("image_upstream_failed", "event_id", eventID, "request_id", requestID, "model", externalModel, "provider", route.Provider, "account_id", credential.ID, "error", err)
			if isSSOCredentialRejected(err, credential) {
				s.markSSOCredentialRejected(ctx, credential, fmt.Sprintf("%s SSO credential rejected", credential.Provider))
				failedCredential := credential
				lastCredentialFailure = &failedCredential
				lastCredentialError = provider.ErrUnauthorized
				lease.Release()
				continue
			}
			if !provider.IsMediaPostProcessingError(err) {
				s.selector.MarkFailure(ctx, credential, 0, 0)
			}
			lease.Release()
			errorCode := "upstream_unavailable"
			if provider.IsMediaPostProcessingError(err) {
				errorCode = "media_postprocessing_failed"
			}
			writeFailureAudit(http.StatusBadGateway, errorCode, &credential)
			return nil, err
		}
		if response.StatusCode == http.StatusUnauthorized && credential.AuthType == accountdomain.AuthTypeSSO {
			_, _ = readRetryableBody(response.Body)
			s.markSSOCredentialRejected(ctx, credential, fmt.Sprintf("%s SSO credential rejected", credential.Provider))
			failedCredential := credential
			lastCredentialFailure = &failedCredential
			lastCredentialError = provider.ErrUnauthorized
			response = nil
			lease.Release()
			continue
		}
		if response.StatusCode == http.StatusUnauthorized && s.providers.SupportsCredentialRefresh(credential.Provider) {
			_, _ = readRetryableBody(response.Body)
			response = nil
			failedCredential := credential
			lastCredentialFailure = &failedCredential
			lastCredentialError = provider.ErrUnauthorized
			lease.Release()
			// Image 401s arrive before submission, so one retry on the same account after a forced refresh is safe.
			if s.markPermanentlyUnrefreshableCredentialRejected(ctx, credential) || authRetried[credential.ID] {
				continue
			}
			authRetried[credential.ID] = true
			if _, refreshErr := s.accounts.RenewRejectedCredential(ctx, credential); refreshErr != nil {
				if errors.Is(refreshErr, accountapp.ErrCredentialRefreshPermanent) {
					s.markCredentialRejectedAfterPermanentRefresh(ctx, credential)
				}
				s.selector.MarkRefreshDeferred(ctx, credential, refreshErr)
				continue
			}
			delete(excluded, credential.ID)
			if selection != nil {
				selection.RetryAccount(credential.ID)
			}
			continue
		}
		if s.providers.RetryForbiddenAsEgress(credential.Provider) && response.StatusCode == http.StatusForbidden && attempt == 0 && attemptPolicy.hasNext(attempt) {
			_, _ = readRetryableBody(response.Body)
			delete(excluded, credential.ID)
			if selection != nil {
				selection.RetryAccount(credential.ID)
			}
			lease.Release()
			continue
		}
		if quotaKind, _ := s.providers.QuotaKind(credential.Provider); quotaKind == provider.QuotaRemoteWindow && response.StatusCode == http.StatusTooManyRequests && lease.QuotaMode != "" {
			retryAfter := parseRetryAfter(response.Header.Get("Retry-After"), time.Now().UTC())
			exhausted, reconcileErr := s.accounts.ReconcileWebRateLimit(ctx, credential.ID, lease.QuotaMode, retryAfter)
			if reconcileErr != nil || !exhausted {
				s.selector.MarkFailure(ctx, credential, response.StatusCode, retryAfter)
			}
			if attemptPolicy.hasNext(attempt) {
				_, _ = readRetryableBody(response.Body)
				lease.Release()
				continue
			}
		}
		break
	}
	if response == nil {
		writeFailureAudit(http.StatusServiceUnavailable, "upstream_unavailable", lastCredentialFailure)
		if lastCredentialError == nil {
			lastCredentialError = ErrNoAvailableAccount
		}
		return nil, fmt.Errorf("%w: %w", ErrNoAvailableAccount, lastCredentialError)
	}
	effectiveQuotaMode := lease.QuotaMode
	accountID := credential.ID
	var once sync.Once
	finalize := func(usage Usage, _ string, errorCode string) {
		once.Do(func() {
			successful := auditRequestSucceeded(response.StatusCode, errorCode)
			lease.completeSelectorObservation(successful)
			lease.Release()
			budget := newFinalizationBudget(string(operation), string(route.Provider))
			record := auditBase
			record.AccountID, record.AccountName, record.StatusCode = &accountID, credential.Name, response.StatusCode
			record.ErrorCode = errorCode
			// Earlier failed attempts (other accounts, rejected responses) stay visible on the final record.
			record.Attempts = failureAttempts.snapshot()
			record.DurationMS, record.CreatedAt = time.Since(startedAt).Milliseconds(), time.Now().UTC()
			applyAuditEgress(&record, egressTrace, route.Provider)
			deliveredCount, quotaUnits, billed := requestedCount, response.QuotaUnits, successful
			if streaming {
				// A stream bills the images it forwarded, also when it failed
				// after them: the client and the gallery already have them.
				deliveredCount = min(requestedCount, int(usage.OutputImages))
				quotaUnits, billed = deliveredCount, deliveredCount > 0
			} else if response.QuotaUnits > 0 {
				// A provider that delivers fewer images than requested reports the
				// delivered count in QuotaUnits; only those are billed.
				deliveredCount = min(requestedCount, response.QuotaUnits)
			}
			if billed {
				record.MediaOutputImages = int64(max(0, deliveredCount))
				var pricing audit.PricingResult
				var priced bool
				switch operation {
				case audit.OperationImage:
					pricing, priced = s.pricing.QuoteImage(route.Provider, pricingModel, pricingResolution, pricingQuality, deliveredCount)
				case audit.OperationImageEdit:
					pricing, priced = s.pricing.QuoteImageEdit(route.Provider, pricingModel, pricingResolution, pricingQuality, deliveredCount, inputImageCount)
				}
				if priced {
					pricing.Apply(&record)
				}
			}
			quotaKind, _ := s.providers.QuotaKind(route.Provider)
			refreshMode, decrementMode, availabilityMode := quotaFinalizationModes(effectiveQuotaMode, quotaRefreshGroup)
			if successful && quotaKind == provider.QuotaRemoteWindow && refreshMode != "" {
				if decrementMode != "" && decrementMode != "weekly" {
					units := max(1, quotaUnits)
					var updated bool
					err := budget.run("quota_decrement", finalizationQuotaBudget, func(stageCtx context.Context) error {
						var decrementErr error
						updated, decrementErr = s.accounts.DecrementWebQuota(stageCtx, accountID, decrementMode, units)
						return decrementErr
					})
					if err != nil {
						s.logger.Warn("web_quota_decrement_failed", "account_id", accountID, "mode", decrementMode, "units", units, "error", err)
					} else if updated {
						s.selector.ConsumeQuota(route.Provider, accountID, decrementMode, units)
					}
				}
				s.accounts.QueueQuotaRefresh(accountID, refreshMode)
				if availabilityMode != "" && availabilityMode != refreshMode {
					s.accounts.QueueQuotaRefresh(accountID, availabilityMode)
				}
			}
			if err := budget.run("audit", finalizationAuditBudget, func(stageCtx context.Context) error {
				return s.audits.Create(stageCtx, record)
			}); err != nil {
				s.logger.Error("request_usage_write_failed", "event_id", record.EventID, "request_id", requestID, "error", err)
			}
		})
	}
	finalizationOwnsReservation = true
	return &Result{StatusCode: response.StatusCode, Status: response.Status, Header: response.Header, Body: &finalizingBody{ReadCloser: response.Body, finalize: func() { finalize(Usage{}, "", "stream_closed") }}, Finalize: finalize}, nil
}

// quotaFinalizationModes separates the immediate local consumption fence from
// the authoritative provider refresh. A refresh group may update several
// upstream windows atomically, while the local fence must charge the exact
// window selected for this account so concurrent media requests cannot
// over-allocate during the short refresh delay.
func quotaFinalizationModes(effectiveMode, refreshGroup string) (refreshMode, decrementMode, availabilityMode string) {
	// Availability-only Imagine products on paid Web tiers are governed by the
	// shared weekly pool. Refresh its numeric counter and also re-read the
	// product group so available=false/nextAvailableAt can install an exact
	// product fence that overrides weekly routing.
	if effectiveMode == "weekly" {
		return effectiveMode, effectiveMode, refreshGroup
	}
	refreshMode = effectiveMode
	if refreshGroup != "" {
		refreshMode = refreshGroup
	}
	return refreshMode, effectiveMode, ""
}
