package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	"github.com/chenyme/grok2api/backend/internal/pkg/requestparams"
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

type imageExecution func(context.Context, accountdomain.Provider, accountdomain.Credential, string, provider.ImageRequestOptions) (*provider.Response, error)

// GenerateImage 选择支持图片生成的路由和账号，并返回可统一审计的上游响应。
func (s *Service) GenerateImage(ctx context.Context, input ImageGenerationInput) (*Result, error) {
	ctx = requestparams.WithClient(ctx, generatedImageParameters(input))
	if input.Count > 1 && !input.Streaming {
		return s.drawImageBatches(ctx, input.Count, s.fansOutImages(ctx, input.PublicModel, input.Count, false), func(ctx context.Context, count int, extra bool) (*Result, error) {
			single := input
			single.Count = count
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
	}, func(executionCtx context.Context, providerValue accountdomain.Provider, credential accountdomain.Credential, upstream string, effective provider.ImageRequestOptions) (*provider.Response, error) {
		adapter, ok := s.providers.ImageGeneration(providerValue)
		if !ok {
			return nil, ErrNoAvailableAccount
		}
		return adapter.GenerateImage(executionCtx, provider.ImageGenerationRequest{
			Credential: credential, Model: upstream, Prompt: input.Prompt, Count: effective.Count,
			Size: effective.Size, AspectRatio: effective.AspectRatio, Resolution: effective.Resolution, Quality: effective.Quality,
			ResponseFormat: effective.ResponseFormat, Streaming: effective.Streaming, PartialImages: effective.PartialImages,
			OutputFormat: effective.OutputFormat, Background: effective.Background,
		})
	}, options, input.Method, input.Path, input.Headers, extra)
}

// EditImage 选择支持图片编辑的路由和账号，并返回可统一审计的上游响应。
func (s *Service) EditImage(ctx context.Context, input ImageEditInput) (*Result, error) {
	ctx = requestparams.WithClient(ctx, editedImageParameters(input))
	if input.Count > 1 && !input.Streaming {
		return s.drawImageBatches(ctx, input.Count, s.fansOutImages(ctx, input.PublicModel, input.Count, false), func(ctx context.Context, count int, extra bool) (*Result, error) {
			single := input
			single.Count = count
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
	}, func(executionCtx context.Context, providerValue accountdomain.Provider, credential accountdomain.Credential, upstream string, effective provider.ImageRequestOptions) (*provider.Response, error) {
		adapter, ok := s.providers.ImageEdit(providerValue)
		if !ok {
			return nil, ErrNoAvailableAccount
		}
		return adapter.EditImage(executionCtx, provider.ImageEditRequest{
			Credential: credential, Model: upstream, Prompt: input.Prompt,
			ImageURLs: input.ImageURLs[:effective.InputImages], Count: effective.Count, Size: effective.Size, AspectRatio: effective.AspectRatio,
			Resolution: effective.Resolution, Quality: effective.Quality, ResponseFormat: effective.ResponseFormat,
			Streaming: effective.Streaming, PartialImages: effective.PartialImages,
			OutputFormat: effective.OutputFormat, Background: effective.Background, MaskURL: input.MaskURL,
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
	// extra marks an image drawn next to its siblings: it
	// gives up without an audit when no account is free
	// (errImageAccountsBusy), so the next round can draw it later.
	extra bool,
) (*Result, error) {
	ctx, parameters := requestparams.Start(ctx, imageParameters(publicModel, prompt, options))
	ctx, egressTrace := infraegress.WithTrace(ctx)
	startedAt := time.Now()
	eventID := newAuditEventID()
	streaming := options.Streaming
	routes, err := s.models.GetByPublicIDCandidates(ctx, publicModel)
	if err != nil {
		return nil, ErrModelNotFound
	}
	// Routes whose provider rejects the parameters are dropped; the first
	// rejection surfaces only when no other target remains.
	var rejection error
	effectiveOptions := make(map[uint64]provider.ImageRequestOptions, len(routes))
	eligible := make([]modeldomain.Route, 0, len(routes))
	for _, candidate := range routes {
		if candidate.Capability != capability || !supports(candidate.Provider) {
			continue
		}
		candidateOptions := options
		candidateOptions.UpstreamModel = candidate.UpstreamModel
		normalized, normalizeErr := s.providers.NormalizeImageRequest(candidate.Provider, candidateOptions)
		if normalizeErr != nil {
			if rejection == nil {
				rejection = &ImageRequestRejectedError{Err: normalizeErr}
			}
			continue
		}
		eligible = append(eligible, candidate)
		effectiveOptions[candidate.ID] = normalized
	}
	if len(eligible) == 0 && rejection != nil {
		return nil, rejection
	}
	routes = eligible
	route, preselectedSession, err := s.selectSchedulableMediaRouteWithQuotaMode(ctx, routes, key, capability, true, supports, func(route modeldomain.Route) string {
		return s.providers.ImageQuotaMode(route.Provider, route.UpstreamModel, options.Edit)
	})
	if err != nil && extra {
		return nil, errImageAccountsBusy
	}
	if err != nil {
		// Preserve the established failure-audit path when every eligible target
		// is currently unschedulable. The request loop will reproduce the
		// selection error for the representative route after auditBase exists.
		route, err = s.selectMediaRoute(routes, key, capability, supports)
		if err != nil {
			if rejection != nil && errors.Is(err, ErrNoAvailableAccount) {
				return nil, rejection
			}
			return nil, err
		}
		preselectedSession = nil
	}
	options = effectiveOptions[route.ID]
	parameters.SetEffective(imageParameters(route.UpstreamModel, prompt, options))
	resolution, quality := options.Resolution, options.Quality
	requestedCount, inputImageCount := options.Count, options.InputImages
	externalModel := modeldomain.ExternalPublicID(route.Provider, route.PublicID)
	auditBase := audit.Record{
		EventID: eventID, RequestID: requestID, ClientKeyID: key.ID, ClientKeyName: key.Name,
		ClientIP:     requestmeta.ClientIP(ctx),
		ModelRouteID: route.ID, ModelPublicID: externalModel, ModelUpstreamModel: modeldomain.DisplayUpstreamModel(route.Provider, route.UpstreamModel),
		Provider: string(route.Provider), Operation: operation, UsageSource: audit.UsageSourceNone, Streaming: streaming,
		RequestMethod: method, RequestPath: path, RequestHeaders: headers,
		RequestParameters: parameters,
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
	if definition, _ := s.providers.Definition(route.Provider); (definition.Media.ImageGenerationModelPriced && operation == audit.OperationImage) || (definition.Media.ImageEditModelPriced && operation == audit.OperationImageEdit) {
		// The product is selected through the catalog model and only
		// effective image shape/count are forwarded. Do not reserve or record a price tier
		// derived from resolution/quality compatibility fields.
		pricingResolution, pricingQuality = "", ""
	}
	reserved := false
	reservationCount := 0
	reserveBatch := func(count int) error {
		if count == reservationCount {
			return nil
		}
		if reserved {
			// Only pre-submission retries can resize this batch. Release its
			// previous hold before reserving the newly selected account's count.
			if err := s.clientKeys.CancelBilling(ctx, eventID); err != nil {
				return err
			}
			reserved = false
		}
		var reservation audit.PricingResult
		var priced bool
		switch operation {
		case audit.OperationImage:
			reservation, priced = s.pricing.QuoteImage(route.Provider, pricingModel, pricingResolution, pricingQuality, count)
		case audit.OperationImageEdit:
			reservation, priced = s.pricing.QuoteImageEdit(route.Provider, pricingModel, pricingResolution, pricingQuality, count, inputImageCount)
		}
		if priced {
			var err error
			reserved, err = s.clientKeys.ReserveBilling(ctx, key, eventID, reservation.CostInUSDTicks, mediaBillingReservationTTL)
			if err != nil {
				return err
			}
		}
		reservationCount = count
		return nil
	}
	finalizationOwnsReservation := false
	releaseQuota := func() {}
	defer func() {
		if !finalizationOwnsReservation {
			releaseQuota()
		}
		if reserved && !finalizationOwnsReservation {
			s.cancelBillingReservation(eventID)
		}
	}()
	quotaMode := s.providers.ImageQuotaMode(route.Provider, route.UpstreamModel, options.Edit)
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
	var lastUpstreamFailure *UpstreamFailure
	var lastQuotaFailure error
	var imageBody *imageResultBody
	batchOptions := options
	releaseAttempt := func() {
		releaseQuota()
		releaseQuota = func() {}
		lease.Release()
	}
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
			if lastUpstreamFailure != nil && lastCredentialError == nil {
				writeFailureAudit(lastUpstreamFailure.HTTPStatus, lastUpstreamFailure.Code, lastCredentialFailure)
				return nil, lastUpstreamFailure
			}
			if lastQuotaFailure != nil && lastUpstreamFailure == nil && lastCredentialError == nil {
				writeFailureAudit(http.StatusTooManyRequests, "upstream_quota_exhausted", lastCredentialFailure)
				return nil, lastQuotaFailure
			}
			errorCode := "upstream_unavailable"
			statusCode := http.StatusServiceUnavailable
			var selectionFailure *SelectionUnavailableError
			if errors.As(err, &selectionFailure) {
				errorCode = selectionFailure.Code()
				statusCode = selectionFailure.HTTPStatus()
			}
			writeFailureAudit(statusCode, errorCode, lastCredentialFailure)
			return nil, fmt.Errorf("%w: %w", ErrNoAvailableAccount, err)
		}
		excluded[lease.Credential.ID] = true
		batchOptions, releaseQuota, err = s.reserveImageQuota(lease, options)
		if err != nil {
			lastQuotaFailure = err
			releaseAttempt()
			attempt-- // A local budget check did not contact the upstream.
			continue
		}
		requestedCount = batchOptions.Count
		parameters.SetEffective(imageParameters(route.UpstreamModel, prompt, batchOptions))
		if err := reserveBatch(requestedCount); err != nil {
			releaseAttempt()
			return nil, err
		}
		attemptStarted := time.Now()
		credential, err = s.accounts.EnsureCredential(ctx, lease.Credential, false)
		if err != nil {
			failureAttempts.captureCredentialFailure(lease.Credential, attemptStarted, false, err)
			s.logger.Error("image_credential_failed", "event_id", eventID, "request_id", requestID, "model", externalModel, "provider", route.Provider, "account_id", lease.Credential.ID, "error", err)
			failedCredential := lease.Credential
			lastCredentialFailure = &failedCredential
			lastCredentialError = err
			releaseAttempt()
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
		response, err = execute(originCtx, route.Provider, credential, route.UpstreamModel, batchOptions)
		if err == nil && !streaming && response.StatusCode >= 200 && response.StatusCode < 300 {
			var prepared io.ReadCloser
			prepared, imageBody, err = prefetchImageResult(response.Body)
			if err == nil {
				response.Body = prepared
			} else {
				_ = response.Body.Close()
				err = invalidImageResult{cause: err}
			}
		}
		// Failed attempts (errors and non-2xx responses) appear in the request diagnostics.
		_ = failureAttempts.captureResponse(credential, attemptStarted, response, err)
		if err != nil {
			s.logger.Error("image_upstream_failed", "event_id", eventID, "request_id", requestID, "model", externalModel, "provider", route.Provider, "account_id", credential.ID, "error", err)
			if isSSOCredentialRejected(err, credential) {
				s.markSSOCredentialRejected(ctx, credential, fmt.Sprintf("%s SSO credential rejected", credential.Provider))
				failedCredential := credential
				lastCredentialFailure = &failedCredential
				lastCredentialError = provider.ErrUnauthorized
				releaseAttempt()
				continue
			}
			failure := ProviderUpstreamFailure(err, credential.ID, credential.Name)
			if !provider.IsMediaPostProcessingError(err) && !provider.IsRequestScopedError(err) && (failure == nil || (!failure.SafetyRejection && !failure.RequestScopedForbidden)) {
				if failure != nil && failure.HTTPStatus == http.StatusTooManyRequests && s.providers.Failure(credential.Provider).SoftRateLimit {
					// A typed error may follow an accepted generation or a result
					// fetch: cool down and refresh quota, but never replay it.
					s.applyRateLimitReconciliation(ctx, credential, failure.HTTPStatus, failure.RetryAfter, accountapp.RateLimitReconcileInconclusive, nil)
					s.accounts.QueueQuotaRefresh(credential.ID, lease.QuotaMode)
				} else {
					s.selector.MarkFailure(ctx, credential, 0, 0)
				}
			}
			releaseAttempt()
			errorCode := "upstream_unavailable"
			if provider.IsMediaPostProcessingError(err) {
				errorCode = "media_postprocessing_failed"
			}
			status := http.StatusBadGateway
			if failure != nil && !provider.IsMediaPostProcessingError(err) {
				status, errorCode, err = failure.HTTPStatus, failure.Code, failure
			}
			writeFailureAudit(status, errorCode, &credential)
			return nil, err
		}
		if response.StatusCode == http.StatusUnauthorized && credential.AuthType == accountdomain.AuthTypeSSO {
			_, _ = readRetryableBody(response.Body)
			s.markSSOCredentialRejected(ctx, credential, fmt.Sprintf("%s SSO credential rejected", credential.Provider))
			failedCredential := credential
			lastCredentialFailure = &failedCredential
			lastCredentialError = provider.ErrUnauthorized
			response = nil
			releaseAttempt()
			continue
		}
		if response.StatusCode == http.StatusUnauthorized && s.providers.SupportsCredentialRefresh(credential.Provider) {
			_, _ = readRetryableBody(response.Body)
			response = nil
			failedCredential := credential
			lastCredentialFailure = &failedCredential
			lastCredentialError = provider.ErrUnauthorized
			releaseAttempt()
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
		if s.retireConsoleNoCreditsAccount(ctx, requestID, credential, response) {
			_, _ = readRetryableBody(response.Body)
			failedCredential := credential
			lastCredentialFailure = &failedCredential
			lastCredentialError = fmt.Errorf("%s", consoleNoCreditsReason)
			response = nil
			releaseAttempt()
			continue
		}
		if s.providers.RetryForbiddenAsEgress(credential.Provider) && response.StatusCode == http.StatusForbidden && attempt == 0 && attemptPolicy.hasNext(attempt) {
			_, _ = readRetryableBody(response.Body)
			delete(excluded, credential.ID)
			if selection != nil {
				selection.RetryAccount(credential.ID)
			}
			releaseAttempt()
			continue
		}
		if quotaKind, _ := s.providers.QuotaKind(credential.Provider); quotaKind == provider.QuotaRemoteWindow && response.StatusCode == http.StatusTooManyRequests && lease.QuotaMode != "" {
			retryAfter := parseRetryAfter(response.Header.Get("Retry-After"), time.Now().UTC())
			state, reconcileErr := s.accounts.ReconcileRateLimit(ctx, credential.ID, lease.QuotaMode, retryAfter)
			s.applyRateLimitReconciliation(ctx, credential, response.StatusCode, retryAfter, state, reconcileErr)
			if attemptPolicy.hasNext(attempt) {
				body, _ := readRetryableBody(response.Body)
				lastUpstreamFailure = newHTTPUpstreamFailure(response.StatusCode, body, credential.ID, credential.Name)
				lastUpstreamFailure.RetryAfter = retryAfter
				failedCredential := credential
				lastCredentialFailure, lastCredentialError = &failedCredential, nil
				response = nil
				releaseAttempt()
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
	deliveredImages := func(usage Usage) int {
		if streaming {
			return min(requestedCount, max(0, int(usage.OutputImages)))
		}
		if imageBody != nil {
			return min(requestedCount, imageBody.outputCount())
		}
		return 0
	}
	var upstreamOnce sync.Once
	finishUpstream := func(usage Usage, errorCode string) {
		upstreamOnce.Do(func() {
			defer releaseQuota()
			deliveredCount := deliveredImages(usage)
			if !streaming {
				// Reading EOF does not release every adapter's egress lease.
				// Close it before the next batch needs that same exit/account.
				_ = response.Body.Close()
			}
			successful := auditRequestSucceeded(response.StatusCode, errorCode)
			// A fully read JSON batch completed upstream even if its later
			// transfer to the client fails. It still spent the native allowance.
			if !streaming && deliveredCount > 0 {
				successful = auditRequestSucceeded(response.StatusCode, "")
			}
			lease.completeSelectorObservation(successful)
			lease.Release()
			quotaKind, _ := s.providers.QuotaKind(route.Provider)
			refreshMode, decrementMode, availabilityMode := quotaFinalizationModes(effectiveQuotaMode, quotaRefreshGroup)
			if !auditRequestSucceeded(response.StatusCode, "") || quotaKind != provider.QuotaRemoteWindow || refreshMode == "" {
				return
			}
			quotaUnits := response.QuotaUnits
			if streaming {
				quotaUnits = deliveredCount
			}
			quotaOptions := batchOptions
			quotaOptions.Count = deliveredCount
			if actualQuota, known := s.providers.ImageQuotaUnits(route.Provider, effectiveQuotaMode, quotaOptions); known && quotaUnits == 0 {
				quotaUnits = actualQuota
			}
			if decrementMode != "" && decrementMode != "weekly" && quotaUnits > 0 {
				budget := newFinalizationBudget(string(operation), string(route.Provider))
				var updated bool
				err := budget.run("quota_decrement", finalizationQuotaBudget, func(stageCtx context.Context) error {
					var decrementErr error
					updated, decrementErr = s.decrementImageQuota(stageCtx, route.Provider, accountID, decrementMode, quotaUnits)
					return decrementErr
				})
				if err != nil {
					s.logger.Warn("image_quota_decrement_failed", "account_id", accountID, "mode", decrementMode, "units", quotaUnits, "error", err)
				}
				if updated || err != nil {
					s.selector.ConsumeQuota(route.Provider, accountID, decrementMode, quotaUnits)
				}
			}
			s.accounts.QueueQuotaRefresh(accountID, refreshMode)
			if availabilityMode != "" && availabilityMode != refreshMode {
				s.accounts.QueueQuotaRefresh(accountID, availabilityMode)
			}
		})
	}
	var once sync.Once
	finalize := func(usage Usage, _ string, errorCode string) {
		once.Do(func() {
			finishUpstream(usage, errorCode)
			successful := auditRequestSucceeded(response.StatusCode, errorCode)
			budget := newFinalizationBudget(string(operation), string(route.Provider))
			record := auditBase
			record.AccountID, record.AccountName, record.StatusCode = &accountID, credential.Name, response.StatusCode
			record.ErrorCode = errorCode
			// Earlier failed attempts (other accounts, rejected responses) stay visible on the final record.
			record.Attempts = failureAttempts.snapshot()
			record.DurationMS, record.CreatedAt = time.Since(startedAt).Milliseconds(), time.Now().UTC()
			applyAuditEgress(&record, egressTrace, route.Provider)
			deliveredCount := deliveredImages(usage)
			billed := successful && deliveredCount > 0
			if streaming {
				// A stream bills the images it forwarded, also when it failed
				// after them: the client and the gallery already have them.
				billed = deliveredCount > 0
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
			if err := budget.run("audit", finalizationAuditBudget, func(stageCtx context.Context) error {
				return s.audits.Create(stageCtx, record)
			}); err != nil {
				s.logger.Error("request_usage_write_failed", "event_id", record.EventID, "request_id", requestID, "error", err)
			}
		})
	}
	finalizationOwnsReservation = true
	return &Result{
		StatusCode: response.StatusCode, Status: response.Status, Header: response.Header,
		Body: &finalizingBody{ReadCloser: response.Body, finalize: func() { finalize(Usage{}, "", "stream_closed") }},
		RecordStreamFailure: func(diagnostic StreamFailureDiagnostic) {
			failureAttempts.captureStreamFailure(credential, startedAt, response, diagnostic)
		},
		Finalize:       finalize,
		imageBatchSize: requestedCount,
		completeImage:  func() { finishUpstream(Usage{}, "") },
		imageOutputCount: func() int {
			if imageBody != nil {
				return imageBody.outputCount()
			}
			return 0
		},
	}, nil
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
