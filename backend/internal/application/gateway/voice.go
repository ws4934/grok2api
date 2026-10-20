package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/pkg/requestmeta"
)

type TTSInput struct {
	RequestID                string
	ClientKey                clientkey.Key
	PublicModel              string
	Text                     string
	VoiceID                  string
	Language                 string
	OutputFormat             provider.TTSOutputFormat
	Speed                    float64
	OptimizeStreamingLatency int
	TextNormalization        bool
	WithTimestamps           bool
	Method                   string
	Path                     string
	Headers                  map[string][]string
}

type STTInput struct {
	RequestID    string
	ClientKey    clientkey.Key
	PublicModel  string
	FileName     string
	FileMIME     string
	FileData     []byte
	URL          string
	AudioFormat  string
	SampleRate   string
	Language     string
	Format       bool
	Multichannel bool
	Channels     int
	Diarize      bool
	KeyTerms     []string
	FillerWords  bool
	VADThreshold *float64
	// ResponseFormat is empty for the native Console-compatible response, or an
	// OpenAI-compatible format normalized by the HTTP transport.
	ResponseFormat string
	Method         string
	Path           string
	Headers        map[string][]string
}

type VoiceListInput struct {
	RequestID   string
	ClientKey   clientkey.Key
	PublicModel string
}

type VoiceIDInput struct {
	RequestID   string
	ClientKey   clientkey.Key
	PublicModel string
	VoiceID     string
}

type voiceProviderSupport func(accountdomain.Provider) bool

type voiceExecutionResult struct {
	response *provider.Response
	pricing  audit.PricingResult
}

func (s *Service) SynthesizeSpeech(ctx context.Context, input TTSInput) (*Result, error) {
	// The price depends on the selected route's provider, so it is quoted
	// after route selection; reservation and settlement use the same quote.
	quote := func(providerValue accountdomain.Provider, upstream string) audit.PricingResult {
		pricing, _ := s.pricing.QuoteTTS(providerValue, s.providers.PricingModel(providerValue, upstream), input.Text)
		return pricing
	}
	return s.executeVoice(ctx, input.RequestID, input.ClientKey, input.PublicModel, audit.OperationTTS, modeldomain.CapabilityTTS, true, quote, input.Method, input.Path, input.Headers, func(providerValue accountdomain.Provider) bool {
		_, ok := s.providers.TTS(providerValue)
		return ok
	}, func(executionCtx context.Context, providerValue accountdomain.Provider, credential accountdomain.Credential, upstream string) (voiceExecutionResult, error) {
		adapter, ok := s.providers.TTS(providerValue)
		if !ok {
			return voiceExecutionResult{}, ErrNoAvailableAccount
		}
		result, err := adapter.SynthesizeSpeech(executionCtx, provider.TTSRequest{
			Credential: credential, Model: upstream, Text: input.Text, VoiceID: input.VoiceID, Language: input.Language,
			OutputFormat: input.OutputFormat, Speed: input.Speed, OptimizeStreamingLatency: input.OptimizeStreamingLatency,
			TextNormalization: input.TextNormalization, WithTimestamps: input.WithTimestamps,
		})
		if err != nil {
			return voiceExecutionResult{}, err
		}
		if result.JSONEnvelope || input.WithTimestamps {
			payload := map[string]any{
				"audio":        firstNonEmpty(result.Base64Audio, base64.StdEncoding.EncodeToString(result.Audio)),
				"content_type": firstNonEmpty(result.ContentType, "audio/mpeg"),
				"duration":     result.Duration,
			}
			if result.Timestamps != nil {
				times := make([]map[string]any, 0, len(result.Timestamps.GraphTimes))
				for _, item := range result.Timestamps.GraphTimes {
					times = append(times, map[string]any{"start": item.Start, "end": item.End})
				}
				payload["audio_timestamps"] = map[string]any{"graph_chars": result.Timestamps.GraphChars, "graph_times": times}
			}
			return voiceExecutionResult{response: jsonVoiceResponse(http.StatusOK, payload), pricing: quote(providerValue, upstream)}, nil
		}
		header := http.Header{}
		header.Set("Content-Type", firstNonEmpty(result.ContentType, "audio/mpeg"))
		header.Set("Content-Length", fmt.Sprintf("%d", len(result.Audio)))
		return voiceExecutionResult{response: &provider.Response{
			StatusCode: http.StatusOK,
			Status:     fmt.Sprintf("%d %s", http.StatusOK, http.StatusText(http.StatusOK)),
			Header:     header,
			Body:       io.NopCloser(bytes.NewReader(result.Audio)),
			QuotaUnits: 1,
		}, pricing: quote(providerValue, upstream)}, nil
	})
}

func (s *Service) ListTTSVoices(ctx context.Context, input VoiceListInput) (*Result, error) {
	return s.executeVoice(ctx, input.RequestID, input.ClientKey, input.PublicModel, audit.OperationTTS, modeldomain.CapabilityTTS, false, nil, "", "", nil, func(providerValue accountdomain.Provider) bool {
		_, ok := s.providers.TTS(providerValue)
		return ok
	}, func(executionCtx context.Context, providerValue accountdomain.Provider, credential accountdomain.Credential, _ string) (voiceExecutionResult, error) {
		adapter, ok := s.providers.TTS(providerValue)
		if !ok {
			return voiceExecutionResult{}, ErrNoAvailableAccount
		}
		voices, err := adapter.ListTTSVoices(executionCtx, credential)
		if err != nil {
			return voiceExecutionResult{}, err
		}
		items := make([]map[string]any, 0, len(voices))
		for _, voice := range voices {
			item := map[string]any{"voice_id": voice.VoiceID, "name": voice.Name}
			if voice.Language != "" {
				item["language"] = voice.Language
			} else {
				item["language"] = nil
			}
			items = append(items, item)
		}
		response := jsonVoiceResponse(http.StatusOK, map[string]any{"voices": items})
		response.QuotaUnits = 0
		return voiceExecutionResult{response: response}, nil
	})
}

func (s *Service) GetTTSVoice(ctx context.Context, input VoiceIDInput) (*Result, error) {
	return s.executeVoice(ctx, input.RequestID, input.ClientKey, input.PublicModel, audit.OperationTTS, modeldomain.CapabilityTTS, false, nil, "", "", nil, func(providerValue accountdomain.Provider) bool {
		_, ok := s.providers.TTS(providerValue)
		return ok
	}, func(executionCtx context.Context, providerValue accountdomain.Provider, credential accountdomain.Credential, _ string) (voiceExecutionResult, error) {
		adapter, ok := s.providers.TTS(providerValue)
		if !ok {
			return voiceExecutionResult{}, ErrNoAvailableAccount
		}
		voice, err := adapter.GetTTSVoice(executionCtx, credential, input.VoiceID)
		if err != nil {
			return voiceExecutionResult{}, err
		}
		payload := map[string]any{"voice_id": voice.VoiceID, "name": voice.Name}
		if voice.Language != "" {
			payload["language"] = voice.Language
		} else {
			payload["language"] = nil
		}
		response := jsonVoiceResponse(http.StatusOK, payload)
		response.QuotaUnits = 0
		return voiceExecutionResult{response: response}, nil
	})
}

func (s *Service) TranscribeSpeech(ctx context.Context, input STTInput) (*Result, error) {
	return s.executeVoice(ctx, input.RequestID, input.ClientKey, input.PublicModel, audit.OperationSTT, modeldomain.CapabilitySTT, true, nil, input.Method, input.Path, input.Headers, func(providerValue accountdomain.Provider) bool {
		_, ok := s.providers.STT(providerValue)
		return ok
	}, func(executionCtx context.Context, providerValue accountdomain.Provider, credential accountdomain.Credential, upstream string) (voiceExecutionResult, error) {
		adapter, ok := s.providers.STT(providerValue)
		if !ok {
			return voiceExecutionResult{}, ErrNoAvailableAccount
		}
		result, err := adapter.TranscribeSpeech(executionCtx, provider.STTRequest{
			Credential: credential, Model: upstream, FileName: input.FileName, FileMIME: input.FileMIME, FileData: input.FileData,
			URL: input.URL, AudioFormat: input.AudioFormat, SampleRate: input.SampleRate, Language: input.Language, Format: input.Format,
			Multichannel: input.Multichannel, Channels: input.Channels, Diarize: input.Diarize, KeyTerms: input.KeyTerms,
			FillerWords: input.FillerWords, VADThreshold: input.VADThreshold,
		})
		if err != nil {
			return voiceExecutionResult{}, err
		}
		pricing, _ := s.pricing.QuoteSTT(providerValue, s.providers.PricingModel(providerValue, upstream), result.Duration, false)
		return voiceExecutionResult{response: formatSTTResponse(result, input.ResponseFormat), pricing: pricing}, nil
	})
}

func formatSTTResponse(result provider.STTResult, responseFormat string) *provider.Response {
	switch responseFormat {
	case "text":
		data := []byte(result.Text)
		header := http.Header{}
		header.Set("Content-Type", "text/plain; charset=utf-8")
		header.Set("Content-Length", strconv.Itoa(len(data)))
		return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: header, Body: io.NopCloser(bytes.NewReader(data)), QuotaUnits: 1}
	case "json":
		return jsonVoiceResponse(http.StatusOK, map[string]any{"text": result.Text})
	case "verbose_json":
		payload := map[string]any{"task": "transcribe", "text": result.Text, "language": result.Language, "duration": result.Duration}
		if len(result.Words) > 0 {
			words := make([]map[string]any, 0, len(result.Words))
			for _, word := range result.Words {
				item := map[string]any{"word": word.Text, "start": word.Start, "end": word.End}
				if word.Speaker != nil {
					item["speaker"] = *word.Speaker
				}
				words = append(words, item)
			}
			payload["words"] = words
		}
		return jsonVoiceResponse(http.StatusOK, payload)
	}
	if len(result.RawJSON) > 0 {
		header := http.Header{}
		header.Set("Content-Type", "application/json")
		header.Set("Content-Length", strconv.Itoa(len(result.RawJSON)))
		return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: header, Body: io.NopCloser(bytes.NewReader(result.RawJSON)), QuotaUnits: 1}
	}
	payload := map[string]any{"text": result.Text, "language": result.Language, "duration": result.Duration}
	if len(result.Words) > 0 {
		words := make([]map[string]any, 0, len(result.Words))
		for _, word := range result.Words {
			item := map[string]any{"text": word.Text, "start": word.Start, "end": word.End}
			if word.Speaker != nil {
				item["speaker"] = *word.Speaker
			}
			words = append(words, item)
		}
		payload["words"] = words
	}
	if len(result.Channels) > 0 {
		channels := make([]map[string]any, 0, len(result.Channels))
		for _, channel := range result.Channels {
			item := map[string]any{"index": channel.Index, "text": channel.Text}
			if len(channel.Words) > 0 {
				words := make([]map[string]any, 0, len(channel.Words))
				for _, word := range channel.Words {
					wordItem := map[string]any{"text": word.Text, "start": word.Start, "end": word.End}
					if word.Speaker != nil {
						wordItem["speaker"] = *word.Speaker
					}
					words = append(words, wordItem)
				}
				item["words"] = words
			}
			channels = append(channels, item)
		}
		payload["channels"] = channels
	}
	return jsonVoiceResponse(http.StatusOK, payload)
}

func (s *Service) executeVoice(
	ctx context.Context,
	requestID string,
	key clientkey.Key,
	publicModel string,
	operation audit.Operation,
	capability modeldomain.Capability,
	consumesQuota bool,
	quote func(accountdomain.Provider, string) audit.PricingResult,
	method string,
	path string,
	headers map[string][]string,
	supports voiceProviderSupport,
	execute func(context.Context, accountdomain.Provider, accountdomain.Credential, string) (voiceExecutionResult, error),
) (*Result, error) {
	ctx, egressTrace := infraegress.WithTrace(ctx)
	startedAt := time.Now()
	eventID := newAuditEventID()
	routes, err := s.models.GetByPublicIDCandidates(ctx, publicModel)
	if err != nil {
		return nil, ErrModelNotFound
	}
	route, preselectedSession, err := s.selectSchedulableMediaRoute(ctx, routes, key, capability, consumesQuota, supports)
	if err != nil {
		// Keep selection failures observable in request audits when no same-name
		// target is schedulable; capability/scope failures still return directly.
		route, err = s.selectMediaRoute(routes, key, capability, supports)
		if err != nil {
			return nil, err
		}
		preselectedSession = nil
	}
	externalModel := modeldomain.ExternalPublicID(route.Provider, route.PublicID)
	auditBase := audit.Record{
		EventID: eventID, RequestID: requestID, ClientKeyID: key.ID, ClientKeyName: key.Name,
		ClientIP:     requestmeta.ClientIP(ctx),
		ModelRouteID: route.ID, ModelPublicID: externalModel, ModelUpstreamModel: modeldomain.DisplayUpstreamModel(route.Provider, route.UpstreamModel),
		Provider: string(route.Provider), Operation: operation, UsageSource: audit.UsageSourceNone,
		RequestMethod: method, RequestPath: path, RequestHeaders: headers,
	}
	if err := s.checkLedgerReady(); err != nil {
		return nil, err
	}
	var reservation audit.PricingResult
	if quote != nil {
		reservation = quote(route.Provider, route.UpstreamModel)
	}
	if reservation.CostInUSDTicks > 0 {
		if _, err := s.clientKeys.ReserveBilling(ctx, key, eventID, reservation.CostInUSDTicks, mediaBillingReservationTTL); err != nil {
			return nil, err
		}
	}
	writeFailureAudit := func(statusCode int, errorCode string, credential *accountdomain.Credential) {
		record := auditBase
		record.StatusCode = statusCode
		record.ErrorCode = errorCode
		s.persistFailureAudit(record, startedAt, egressTrace, credential)
	}
	quotaMode := ""
	if consumesQuota {
		quotaMode = s.providers.QuotaMode(route.Provider, route.UpstreamModel)
	}
	attemptPolicy := newRoutingAttemptPolicy(int(s.maxAttempts.Load()))
	excluded := make(map[uint64]bool)
	selection := preselectedSession
	var lease *accountLease
	var credential accountdomain.Credential
	var response *provider.Response
	var completedPricing audit.PricingResult
	var responseRequestScoped bool
	var lastCredentialFailure *accountdomain.Credential
	var lastCredentialError error
	// retried keeps the last upstream failure that moved the request to
	// another account, so running out of accounts reports it instead of a
	// generic "no account" error.
	var retried *retriedVoiceResponse
	keepRetried := func() {
		body, _ := readRetryableBody(response.Body)
		retried = &retriedVoiceResponse{status: response.StatusCode, header: response.Header.Clone(), body: body, credential: credential}
	}
	for attempt := 0; attemptPolicy.allows(attempt); attempt++ {
		// A credential or transport failure of the previous attempt must not
		// skip this attempt's acquire or leave its drained response behind.
		err, response = nil, nil
		if selection == nil {
			selection, err = s.selector.beginSelectionSessionForKey(ctx, route.Provider, route.ID, route.UpstreamModel, quotaMode, "", excluded, false, key.AccountScope())
		}
		if err == nil {
			lease, err = selection.Acquire(ctx, excluded, false)
		}
		if err != nil {
			errorCode := "upstream_unavailable"
			var selectionFailure *SelectionUnavailableError
			if errors.As(err, &selectionFailure) {
				errorCode = selectionFailure.Code()
			}
			if retried != nil {
				writeFailureAudit(retried.status, "upstream_error", &retried.credential)
				return retried.result(), nil
			}
			writeFailureAudit(http.StatusServiceUnavailable, errorCode, lastCredentialFailure)
			return nil, fmt.Errorf("%w: %w", ErrNoAvailableAccount, err)
		}
		excluded[lease.Credential.ID] = true
		credential, err = s.accounts.EnsureCredential(ctx, lease.Credential, false)
		if err != nil {
			failedCredential := lease.Credential
			lastCredentialFailure = &failedCredential
			lastCredentialError = err
			lease.Release()
			continue
		}
		lease.markSelectorUpstreamStarted()
		responseRequestScoped = false
		execution, executionErr := execute(ctx, route.Provider, credential, route.UpstreamModel)
		response, completedPricing, err = execution.response, execution.pricing, executionErr
		if err != nil {
			if _, ok := provider.ErrorHTTPStatus(err); ok {
				responseRequestScoped = provider.IsRequestScopedError(err)
				response, err = voiceErrorResponse(err)
			} else if isSSOCredentialRejected(err, credential) {
				s.markSSOCredentialRejected(ctx, credential, fmt.Sprintf("%s SSO credential rejected", credential.Provider))
				failedCredential := credential
				lastCredentialFailure = &failedCredential
				lastCredentialError = provider.ErrUnauthorized
				lease.Release()
				continue
			} else {
				failure := newTransportUpstreamFailure(err, credential.ID, credential.Name)
				lastCredentialError = failure
				if ctx.Err() == nil && isRetryableTransportFailure(s.providers.Failure(credential.Provider), err) && attempt == 0 && attemptPolicy.hasNext(attempt) {
					// Transport failures are normally tied to the selected egress. Retry the
					// same account after the adapter has invalidated/rebuilt that transport,
					// without cooling an otherwise healthy credential.
					delete(excluded, credential.ID)
					if selection != nil {
						selection.RetryAccount(credential.ID)
					}
					lease.Release()
					continue
				}
				lease.Release()
				writeFailureAudit(failure.HTTPStatus, failure.AuditCode(), &credential)
				return nil, failure
			}
			if err != nil {
				lease.Release()
				writeFailureAudit(http.StatusBadGateway, "upstream_unavailable", &credential)
				return nil, err
			}
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
		if s.providers.RetryForbiddenAsEgress(credential.Provider) && response.StatusCode == http.StatusForbidden && !responseRequestScoped && attempt == 0 && attemptPolicy.hasNext(attempt) {
			_, _ = readRetryableBody(response.Body)
			delete(excluded, credential.ID)
			if selection != nil {
				selection.RetryAccount(credential.ID)
			}
			lease.Release()
			continue
		}
		if response.StatusCode == http.StatusPaymentRequired || response.StatusCode == http.StatusTooManyRequests {
			retryAfter := parseRetryAfter(response.Header.Get("Retry-After"), time.Now().UTC())
			if quotaKind, _ := s.providers.QuotaKind(credential.Provider); quotaKind == provider.QuotaRemoteWindow && lease.QuotaMode != "" {
				state, reconcileErr := s.accounts.ReconcileRateLimit(ctx, credential.ID, lease.QuotaMode, retryAfter)
				s.applyRateLimitReconciliation(ctx, credential, response.StatusCode, retryAfter, state, reconcileErr)
			} else {
				s.selector.MarkFailure(ctx, credential, response.StatusCode, retryAfter)
			}
			if attemptPolicy.hasNext(attempt) {
				keepRetried()
				lease.Release()
				continue
			}
		}
		// A request-scoped failure (a shared backend, a bad input) fails the
		// same way on every account.
		if response.StatusCode >= http.StatusInternalServerError && !responseRequestScoped && attemptPolicy.hasNext(attempt) {
			keepRetried()
			lease.Release()
			continue
		}
		break
	}
	if response == nil && retried != nil {
		writeFailureAudit(retried.status, "upstream_error", &retried.credential)
		return retried.result(), nil
	}
	if response == nil {
		writeFailureAudit(http.StatusServiceUnavailable, "upstream_unavailable", lastCredentialFailure)
		if lastCredentialError == nil {
			lastCredentialError = ErrNoAvailableAccount
		}
		return nil, fmt.Errorf("%w: %w", ErrNoAvailableAccount, lastCredentialError)
	}
	accountID := credential.ID
	var once sync.Once
	finalize := func(_ Usage, _ string, errorCode string) {
		once.Do(func() {
			successful := auditRequestSucceeded(response.StatusCode, errorCode)
			lease.completeSelectorObservation(successful)
			lease.Release()
			budget := newFinalizationBudget(string(operation), string(route.Provider))
			if successful {
				_ = budget.run("account_health", finalizationHealthBudget, func(stageCtx context.Context) error {
					s.selector.markSuccess(stageCtx, credential, false)
					return nil
				})
			}
			record := auditBase
			record.AccountID, record.AccountName, record.StatusCode = &accountID, credential.Name, response.StatusCode
			record.ErrorCode = errorCode
			record.DurationMS, record.CreatedAt = time.Since(startedAt).Milliseconds(), time.Now().UTC()
			applyAuditEgress(&record, egressTrace, route.Provider)
			if successful && completedPricing.CostInUSDTicks > 0 {
				completedPricing.Apply(&record)
			}
			if successful && response.QuotaUnits > 0 && quotaMode != "" && quotaMode != "weekly" {
				units := response.QuotaUnits
				var updated bool
				err := budget.run("quota_decrement", finalizationQuotaBudget, func(stageCtx context.Context) error {
					var decrementErr error
					updated, decrementErr = s.accounts.DecrementQuota(stageCtx, accountID, quotaMode, units)
					return decrementErr
				})
				if err != nil {
					s.logger.Warn("voice_quota_decrement_failed", "provider", route.Provider, "account_id", accountID, "mode", quotaMode, "units", units, "error", err)
				} else if updated {
					s.selector.ConsumeQuota(route.Provider, accountID, quotaMode, units)
				}
			}
			if successful && response.QuotaUnits > 0 && quotaMode != "" {
				if quotaKind, _ := s.providers.QuotaKind(route.Provider); quotaKind == provider.QuotaRemoteWindow {
					s.accounts.QueueQuotaRefresh(accountID, quotaMode)
				}
			}
			if err := budget.run("audit", finalizationAuditBudget, func(stageCtx context.Context) error {
				return s.audits.Create(stageCtx, record)
			}); err != nil {
				s.logger.Error("request_usage_write_failed", "event_id", record.EventID, "request_id", requestID, "error", err)
			}
		})
	}
	return &Result{StatusCode: response.StatusCode, Status: response.Status, Header: response.Header, Body: &finalizingBody{ReadCloser: response.Body, finalize: func() { finalize(Usage{}, "", "stream_closed") }}, Finalize: finalize}, nil
}

// retriedVoiceResponse is an upstream failure the request moved past.
type retriedVoiceResponse struct {
	status     int
	header     http.Header
	body       []byte
	credential accountdomain.Credential
}

// result returns the failure as the final response; its audit is already written.
func (r *retriedVoiceResponse) result() *Result {
	r.header.Set("Content-Length", strconv.Itoa(len(r.body)))
	return &Result{
		StatusCode: r.status, Status: fmt.Sprintf("%d %s", r.status, http.StatusText(r.status)), Header: r.header,
		Body: io.NopCloser(bytes.NewReader(r.body)), Finalize: func(Usage, string, string) {},
	}
}

func jsonVoiceResponse(status int, value any) *provider.Response {
	data, _ := json.Marshal(value)
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	header.Set("Content-Length", fmt.Sprintf("%d", len(data)))
	return &provider.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     header,
		Body:       io.NopCloser(bytes.NewReader(data)),
		QuotaUnits: 1,
	}
}

func voiceErrorResponse(err error) (*provider.Response, error) {
	if status, ok := provider.ErrorHTTPStatus(err); ok {
		message, safe := provider.ErrorPublicMessage(err)
		if !safe {
			message = "上游语音服务返回错误"
		}
		response := jsonVoiceResponse(status, map[string]any{"error": map[string]any{"type": "upstream_error", "message": message}})
		if retryAfter := provider.ErrorRetryAfter(err); retryAfter > 0 {
			seconds := max(int64(1), int64((retryAfter+time.Second-1)/time.Second))
			response.Header.Set("Retry-After", strconv.FormatInt(seconds, 10))
		}
		return response, nil
	}
	return nil, err
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
