package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	providerstreamidle "github.com/chenyme/grok2api/backend/internal/infra/provider/streamidle"
	neterrorpkg "github.com/chenyme/grok2api/backend/internal/pkg/neterror"
)

func (a *Adapter) ForwardResponse(ctx context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	if request.NormalizedMetadata != nil {
		*request.NormalizedMetadata = provider.NormalizedRequestMetadata{}
	}
	accessToken, err := a.cipher.Decrypt(request.Credential.EncryptedAccessToken)
	if err != nil {
		return nil, err
	}
	body := request.Body
	var toolCompatibility *responsesToolCompatibility
	var conversationOptions conversation.ResponseOptions
	cacheRoute := buildPromptCacheRoute{}
	compactionRequested := false
	// Resolve the physical inference plane before normalization so Chat and
	// Anthropic conversion can use the same scope for capture and restoration.
	// The scope contains the trusted session seed, model, and plane only; it is
	// intentionally independent of the selected account.
	primaryBase := a.primaryBaseURL()
	base := a.inferenceBaseForOperation(request.Credential, request.Billing, request.Method, request.Path)
	conversationScope := a.conversationReasoningScope(request, base)
	if request.NormalizeBody {
		if request.Operation == conversation.OperationChat || request.Operation == conversation.OperationMessages {
			body, conversationOptions, err = conversation.ConvertRequestWithReasoningReplay(body, request.Model, request.Operation, a.conversationReasoningCache, conversationScope)
			if err == nil && conversationOptions.ReasoningEffortSet && request.NormalizedMetadata != nil {
				request.NormalizedMetadata.ReasoningEffort = conversationOptions.ReasoningEffort
			}
			if err == nil {
				// The Responses branch applies this inside normalizeResponsesRequestWithMetadata.
				body, err = normalizeBuildRequestWithMetadata(body, request.Model, request.Operation, request.NormalizedMetadata)
			}
		} else {
			var driftedCompactions int
			body, driftedCompactions, err = expandGatewayCompactionHistory(body, a.compaction, request.PromptCacheKey)
			if err != nil {
				return invalidResponsesResponse(err), nil
			}
			body, toolCompatibility, err = normalizeResponsesRequestWithMetadata(body, request.Model, request.NormalizedMetadata)
			if toolCompatibility != nil {
				compactionRequested = toolCompatibility.compactionRequested
				if driftedCompactions > 0 {
					toolCompatibility.addWarning("compaction_session_drifted")
				}
			}
		}
		if err != nil {
			if request.Operation == conversation.OperationChat || request.Operation == conversation.OperationMessages {
				return invalidConversationResponse(request.Operation, err), nil
			}
			return invalidResponsesResponse(err), nil
		}
	}
	if request.Operation == conversation.OperationMessages && conversationOptions.AnthropicWebSearch {
		request.ReasoningReplayKey = ""
		conversationOptions = conversationOptions.WithReasoningReplay(nil, "")
	}
	if compactionRequested {
		body, err = prepareGatewayCompactionSample(body)
		if err != nil {
			return invalidResponsesResponse(err), nil
		}
	}
	if len(body) > 0 && request.Method == http.MethodPost {
		if !compactionRequested {
			allowClientTools := request.AllowClientToolCacheRoute || (account.RoutingCandidate{Credential: request.Credential, Billing: request.Billing}).IsKnownFreeBuild()
			body, cacheRoute, err = prepareBuildPromptCacheRoute(body, request.Operation, request.Model, request.PromptCacheKey, allowClientTools)
			if err != nil {
				err = fmt.Errorf("准备 Build prompt cache 路由: %w", err)
				if request.Operation == conversation.OperationChat || request.Operation == conversation.OperationMessages {
					return invalidConversationResponse(request.Operation, err), nil
				}
				return invalidResponsesResponse(err), nil
			}
		}
	}
	if compactionRequested {
		warnings := ""
		if toolCompatibility != nil {
			warnings = toolCompatibility.warningHeader()
		}
		return a.forwardGatewayCompaction(ctx, request, accessToken, body, warnings)
	}
	// Explicit mode wins; in auto mode only confirmed Super accounts with bot_flag_source/bfs in {1,2} default to XAI.
	// Prompt-cache affinity and reasoning replay use separate identities. The
	// native Responses replay remains account-scoped, while the Chat/Messages
	// bridge is scoped to the client session, model, and physical plane only;
	// encrypted reasoning is portable across accounts on that plane.
	replayBaseBody := body
	body, replayKey := a.applyReasoningReplay(ctx, request, replayBaseBody, base)
	call := a.doResponseRequest(ctx, request, accessToken, body, base)
	if call.err != nil {
		return nil, call.err
	}
	if err := normalizeGzipResponse(call.response); err != nil {
		return nil, err
	}
	call, reasoningRecovery, recoveryErr := a.recoverReasoningDecodeFailure(ctx, request, accessToken, body, base, replayKey, call)
	if recoveryErr != nil {
		return nil, recoveryErr
	}
	resp, reqURL := call.response, call.upstreamURL
	var recoveredPrimaryFailure *provider.DiagnosticResponse
	var recoveredPrimaryAttempt *provider.RecoveredAttempt
	// Only eligible operations probe XAI with an equivalent request after the Build primary explicitly returns 403.
	if strings.EqualFold(base, primaryBase) && shouldProbeXAIInferenceFallback(request.Credential, request.Billing, request.Method, request.Path, resp.StatusCode) {
		primaryCall := call
		// Buffer the primary 403 body and replay it unchanged if fallback fails; never issue a second primary POST.
		primaryBody, primaryTruncated, readErr := provider.ReadDiagnosticBody(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		primaryResp := cloneBufferedResponse(resp, primaryBody, primaryTruncated)
		if shouldSkipXAIFallback(primaryBody) {
			resp = primaryResp
		} else {
			fallbackBase := a.fallbackBaseURL()
			if fallbackBase != "" && !strings.EqualFold(fallbackBase, base) {
				fallbackBody, fallbackReplayKey := a.applyReasoningReplay(ctx, request, replayBaseBody, fallbackBase)
				fallbackCtx := infraegress.WithPhysicalCallStage(ctx, "plane_fallback")
				fallbackCall := a.doResponseRequest(fallbackCtx, request, accessToken, fallbackBody, fallbackBase)
				fallbackErr := fallbackCall.err
				if fallbackErr == nil {
					fallbackErr = normalizeGzipResponse(fallbackCall.response)
				}
				fallbackRecovery := reasoningRecoveryOutcome{}
				if fallbackErr == nil {
					fallbackCall, fallbackRecovery, fallbackErr = a.recoverReasoningDecodeFailure(ctx, request, accessToken, fallbackBody, fallbackBase, fallbackReplayKey, fallbackCall)
				}
				if fallbackErr == nil && isHTTPSuccess(fallbackCall.response.StatusCode) {
					recoveredPrimaryFailure = bufferedFailureDiagnostic(primaryResp, primaryBody, primaryTruncated)
					recoveredPrimaryAttempt = &provider.RecoveredAttempt{
						Stage:       "primary_plane_response",
						Result:      "replaced_by_plane_fallback",
						UpstreamURL: primaryCall.upstreamURL,
						StartedAt:   primaryCall.startedAt,
						DurationMS:  primaryCall.durationMS,
						Diagnostic:  *recoveredPrimaryFailure,
					}
					a.activateBuildAPIFallback(ctx, &request.Credential)
					call = fallbackCall
					resp, reqURL, base, replayKey = call.response, call.upstreamURL, fallbackBase, fallbackReplayKey
					conversationOptions = conversationOptions.WithReasoningReplay(a.conversationReasoningCache, a.conversationReasoningScope(request, base))
					reasoningRecovery = reasoningRecovery.merge(fallbackRecovery)
				} else {
					if fallbackErr == nil && fallbackCall.response != nil {
						_ = fallbackCall.response.Body.Close()
					}
					// Preserve the original primary 403 URL and buffered body without requesting the primary again.
					resp = primaryResp
				}
			} else {
				resp = primaryResp
			}
		}
	}
	var rateLimit *provider.RateLimitMetadata
	var rateLimitDiagnostic *provider.DiagnosticResponse
	if resp.StatusCode == http.StatusTooManyRequests {
		body, truncated, readErr := provider.ReadDiagnosticBody(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))
		resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
		rateLimit = provider.RateLimitFromResponse(resp.StatusCode, resp.Header, body)
		if truncated {
			rateLimitDiagnostic = &provider.DiagnosticResponse{
				StatusCode: resp.StatusCode, Status: resp.Status, Header: resp.Header.Clone(),
				Body: append([]byte(nil), body...), BodyTruncated: true,
			}
		}
	}
	if request.Streaming && isHTTPSuccess(resp.StatusCode) && resp.Body != nil {
		resp.Body = wrapBuildSemanticIdle(resp.Body, a.config().StreamIdleTimeout)
	}
	modelCatalogChanged := a.modelCatalogChanged(request.Credential.ID, resp.Header.Get("x-models-etag"))
	// Capture or clear reasoning replay in the upstream Responses shape before protocol conversion.
	if a.shouldCaptureReplay(request, resp, replayKey) {
		resp.Body = a.replay.CaptureBody(resp.Body, request.Model, replayKey, request.Streaming, isCompactPath(request.Path))
	}
	// Replay must read the raw upstream output. Hide xAI native search subcalls from downstream clients
	// only after capture wrapping has completed.
	if isHTTPSuccess(resp.StatusCode) {
		if err := filterBuildPromptCacheResponse(resp, request.Streaming, cacheRoute); err != nil {
			return nil, err
		}
	}
	responsesOperation := request.Operation == "" || request.Operation == conversation.OperationResponses || request.Operation == conversation.OperationCompaction
	if responsesOperation && toolCompatibility != nil {
		if warnings := toolCompatibility.warningHeader(); warnings != "" {
			resp.Header.Set("X-Grok2API-Compatibility-Warnings", warnings)
		}
	}
	reasoningRecovery.appendWarnings(resp.Header)
	if responsesOperation && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if request.Streaming {
			resp.Body = toolCompatibility.normalizeResponseStream(resp.Body)
			resp.Header.Del("Content-Length")
			resp.Header.Set("Content-Type", "text/event-stream")
		} else if toolCompatibility != nil {
			data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxCompatibleResponseBytes+1))
			_ = resp.Body.Close()
			if readErr != nil {
				return nil, readErr
			}
			if len(bytes.TrimSpace(data)) == 0 {
				return nil, neterrorpkg.ErrUpstreamResponseEmpty
			}
			if len(data) > maxCompatibleResponseBytes {
				return nil, fmt.Errorf("上游兼容 Responses 响应超过 128 MiB")
			}
			converted, convertErr := toolCompatibility.normalizeResponseJSON(data)
			if convertErr != nil {
				return nil, convertErr
			}
			resp.Body = io.NopCloser(bytes.NewReader(converted))
			resp.Header.Set("Content-Length", strconv.Itoa(len(converted)))
			resp.Header.Set("Content-Type", "application/json")
		}
	}
	if request.Operation == conversation.OperationChat || request.Operation == conversation.OperationMessages {
		if request.Streaming && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			resp.Body = conversation.ConvertResponseStreamWithOptions(resp.Body, request.Operation, conversationOptions)
			resp.Header.Del("Content-Length")
			resp.Header.Set("Content-Type", "text/event-stream")
		} else {
			var data []byte
			var readErr error
			var diagnosticTruncated bool
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				data, readErr = io.ReadAll(io.LimitReader(resp.Body, (64<<20)+1))
			} else {
				data, diagnosticTruncated, readErr = provider.ReadDiagnosticBody(resp.Body)
			}
			_ = resp.Body.Close()
			if readErr != nil {
				return nil, readErr
			}
			if resp.StatusCode >= 200 && resp.StatusCode < 300 && len(bytes.TrimSpace(data)) == 0 {
				return nil, neterrorpkg.ErrUpstreamResponseEmpty
			}
			if resp.StatusCode >= 200 && resp.StatusCode < 300 && len(data) > 64<<20 {
				return nil, fmt.Errorf("上游对话响应超过 64 MiB")
			}
			var diagnostic *provider.DiagnosticResponse
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				diagnostic = &provider.DiagnosticResponse{StatusCode: resp.StatusCode, Status: resp.Status, Header: resp.Header.Clone(), Body: data, BodyTruncated: diagnosticTruncated || rateLimitDiagnostic != nil}
			}
			converted, convertErr := conversation.ConvertResponseJSONWithOptions(data, request.Operation, conversationOptions)
			if convertErr != nil {
				if diagnostic == nil {
					return nil, convertErr
				}
				return &provider.Response{StatusCode: resp.StatusCode, Status: resp.Status, Header: diagnostic.Header.Clone(), Body: io.NopCloser(bytes.NewReader(data)), UpstreamURL: reqURL, Diagnostic: diagnostic, ReasoningRecoveryFailed: reasoningRecovery.failed, RecoveredPrimaryFailure: recoveredPrimaryFailure, RecoveredAttempts: recoveredAttempts(recoveredPrimaryAttempt, reasoningRecovery), RateLimit: rateLimit, ModelCatalogChanged: modelCatalogChanged}, nil
			}
			resp.Body = io.NopCloser(bytes.NewReader(converted))
			resp.Header.Set("Content-Length", strconv.Itoa(len(converted)))
			resp.Header.Set("Content-Type", "application/json")
			return &provider.Response{StatusCode: resp.StatusCode, Status: resp.Status, Header: resp.Header.Clone(), Body: resp.Body, UpstreamURL: reqURL, Diagnostic: diagnostic, ReasoningRecoveryFailed: reasoningRecovery.failed, RecoveredPrimaryFailure: recoveredPrimaryFailure, RecoveredAttempts: recoveredAttempts(recoveredPrimaryAttempt, reasoningRecovery), RateLimit: rateLimit, ModelCatalogChanged: modelCatalogChanged}, nil
		}
	}
	return &provider.Response{StatusCode: resp.StatusCode, Status: resp.Status, Header: resp.Header.Clone(), Body: resp.Body, UpstreamURL: reqURL, Diagnostic: rateLimitDiagnostic, ReasoningRecoveryFailed: reasoningRecovery.failed, RecoveredPrimaryFailure: recoveredPrimaryFailure, RecoveredAttempts: recoveredAttempts(recoveredPrimaryAttempt, reasoningRecovery), RateLimit: rateLimit, ModelCatalogChanged: modelCatalogChanged}, nil
}

// recoveredAttempts 按真实调用顺序汇总被 fallback 与 reasoning recovery 隐藏的失败。
func recoveredAttempts(primary *provider.RecoveredAttempt, recovery reasoningRecoveryOutcome) []provider.RecoveredAttempt {
	attempts := append([]provider.RecoveredAttempt(nil), recovery.attempts...)
	if primary != nil {
		attempts = append([]provider.RecoveredAttempt{*primary}, attempts...)
	}
	return attempts
}

func (a *Adapter) shouldCaptureReplay(request provider.ResponseResourceRequest, resp *http.Response, replayKey string) bool {
	if a.replay == nil || !a.replay.Enabled() || resp == nil {
		return false
	}
	if request.Method != http.MethodPost || strings.TrimSpace(replayKey) == "" {
		return false
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false
	}
	return true
}

func (a *Adapter) applyReasoningReplay(ctx context.Context, request provider.ResponseResourceRequest, body []byte, base string) ([]byte, string) {
	if a.replay == nil || !a.replay.Enabled() || request.Method != http.MethodPost {
		return body, ""
	}
	key := a.scopedReasoningReplayKey(request, base)
	if key == "" {
		return body, ""
	}
	if isCompactPath(request.Path) {
		// compact does not inject history, but a successful request still clears old replay in the same scope.
		return body, key
	}
	return a.replay.Apply(ctx, request.Model, key, body), key
}

func (a *Adapter) scopedReasoningReplayKey(request provider.ResponseResourceRequest, base string) string {
	seed := strings.TrimSpace(request.ReasoningReplayKey)
	if seed == "" {
		return ""
	}
	plane := "build"
	if fallback := a.fallbackBaseURL(); fallback != "" && strings.EqualFold(strings.TrimRight(base, "/"), fallback) {
		plane = "xai"
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("grok2api:reasoning-replay:v3:%s:%s", seed, plane)))
	return hex.EncodeToString(digest[:])
}

func (a *Adapter) conversationReasoningScope(request provider.ResponseResourceRequest, base string) string {
	seed := strings.TrimSpace(request.ReasoningReplayKey)
	model := strings.ToLower(strings.TrimSpace(request.Model))
	if seed == "" || model == "" {
		return ""
	}
	plane := "build"
	if fallback := a.fallbackBaseURL(); fallback != "" && strings.EqualFold(strings.TrimRight(base, "/"), fallback) {
		plane = "xai"
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("grok2api:conversation-reasoning:v1:%s:%s:%s", seed, model, plane)))
	return hex.EncodeToString(digest[:])
}

func isCompactPath(path string) bool {
	return strings.Contains(strings.ToLower(path), "compact")
}

type responseCall struct {
	response    *http.Response
	upstreamURL string
	startedAt   time.Time
	durationMS  int64
	err         error
}

// doResponseRequest 发起一次真实 Responses 上游调用，并保留该次调用自己的审计来源信息。
func (a *Adapter) doResponseRequest(ctx context.Context, request provider.ResponseResourceRequest, accessToken string, body []byte, base string) (call responseCall) {
	startedAt := time.Now()
	call = responseCall{upstreamURL: a.urlWithBase(base, request.Path), startedAt: startedAt.UTC()}
	defer func() {
		call.durationMS = time.Since(startedAt).Milliseconds()
	}()
	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	requestCtx := infraegress.WithCredential(ctx, request.Credential)
	if request.ForcedEgressNodeID != 0 {
		requestCtx = infraegress.WithEgressNode(requestCtx, request.ForcedEgressNodeID)
	}
	plane := "build"
	if fallback := a.fallbackBaseURL(); fallback != "" && strings.EqualFold(strings.TrimRight(base, "/"), fallback) {
		plane = "xai"
	}
	requestCtx = infraegress.WithPhysicalCallPlane(requestCtx, plane)
	req, err := http.NewRequestWithContext(requestCtx, request.Method, call.upstreamURL, bodyReader)
	if err != nil {
		call.err = err
		return call
	}
	call.upstreamURL = req.URL.String()
	if err := a.applyHeaders(req, request.Credential, accessToken, request.Model, request.PromptCacheKey, true); err != nil {
		call.err = err
		return call
	}
	applyGrokTurnIndexHeader(req, request.GrokTurnIndex)
	if request.TransientRetry > 0 {
		req.Header.Set("x-grok-transient-retry", strconv.Itoa(request.TransientRetry))
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if request.Streaming {
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Accept-Encoding", "identity")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	if request.IdempotencyID != "" {
		req.Header.Set("Idempotency-Key", request.IdempotencyID)
	}
	// Streaming requests already receive transport/semantic idle protection.
	// Non-streaming text inference needs its own cancel-cause-aware body timer:
	// ResponseHeaderTimeout stops once headers arrive and cannot interrupt a
	// server that then leaves the JSON body silent indefinitely. Create the
	// derived context only after every fallible request-construction step so
	// every remaining path either cancels it or transfers ownership to the body.
	var responseIdleCancel context.CancelCauseFunc
	responseIdle := a.config().StreamIdleTimeout
	if !request.Streaming && responseIdle > 0 {
		requestCtx, responseIdleCancel = context.WithCancelCause(requestCtx)
		req = req.WithContext(requestCtx)
	}
	resp, err := a.http.Do(req)
	if err != nil {
		if responseIdleCancel != nil {
			responseIdleCancel(nil)
		}
		call.err = err
		return call
	}
	if responseIdleCancel != nil {
		switch {
		case resp.Body == nil:
			responseIdleCancel(nil)
		case isHTTPSuccess(resp.StatusCode):
			resp.Body = providerstreamidle.New(resp.Body, responseIdle, responseIdleCancel)
		default:
			resp.Body = &cancelOnCloseReadCloser{ReadCloser: resp.Body, cancel: responseIdleCancel}
		}
	}
	call.response = resp
	return call
}

// applyGrokTurnIndexHeader forwards a real client turn only when the request has a stable Grok session.
func applyGrokTurnIndexHeader(request *http.Request, value string) {
	if request.Header.Get("x-grok-session-id") == "" {
		return
	}
	if turnIndex := normalizeGrokTurnIndex(value); turnIndex != "" {
		request.Header.Set("x-grok-turn-idx", turnIndex)
	}
}

// normalizeGrokTurnIndex accepts only non-negative decimal u64 values generated by an official client.
// Empty or invalid values are omitted; the gateway never fabricates turns from history, tool loops, or compaction.
func normalizeGrokTurnIndex(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 20 {
		return ""
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return ""
		}
	}
	if _, err := strconv.ParseUint(value, 10, 64); err != nil {
		return ""
	}
	return value
}

// invalidResponsesResponse converts local protocol validation errors to a standard OpenAI error response,
// avoiding an upstream account retry.
func invalidResponsesResponse(err error) *provider.Response {
	code := "invalid_request"
	param := ""
	message := err.Error()
	var requestErr *responsesRequestError
	if errors.As(err, &requestErr) {
		code = requestErr.Code
		param = requestErr.Param
		message = requestErr.Message
	}
	errorBody := map[string]any{"type": "invalid_request_error", "message": message, "code": code}
	if param != "" {
		errorBody["param"] = param
	}
	data, _ := json.Marshal(map[string]any{"error": errorBody})
	return &provider.Response{
		StatusCode: http.StatusBadRequest, Status: "400 Bad Request",
		Header: http.Header{"Content-Type": []string{"application/json"}, "Content-Length": []string{strconv.Itoa(len(data))}},
		Body:   io.NopCloser(bytes.NewReader(data)),
	}
}

func invalidConversationResponse(operation string, err error) *provider.Response {
	var payload any = map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": err.Error()}}
	if operation == conversation.OperationMessages {
		payload = map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": err.Error()}}
	}
	data, _ := json.Marshal(payload)
	return &provider.Response{
		StatusCode: http.StatusBadRequest, Status: "400 Bad Request",
		Header: http.Header{"Content-Type": []string{"application/json"}, "Content-Length": []string{strconv.Itoa(len(data))}},
		Body:   io.NopCloser(bytes.NewReader(data)),
	}
}
