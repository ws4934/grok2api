// Protocol portions adapted from SkyKoo/gptGrok2api at 15773099641575776025e21d1290b5209b058c5a. See LICENSE.reference.
package chatgpt

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	mathrand "math/rand"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/channel"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/chatgpt/browserprofile"
	"golang.org/x/crypto/sha3"
	_ "golang.org/x/image/webp"
)

// OpenAIImage implements the authenticated ChatGPT Web image flow. It is kept
// separate from Media because ChatGPT JWTs and Grok SSO cookies are different
// credential families and must never share request headers or account pools.
type OpenAIImage struct {
	BaseURL        string
	Client         *http.Client
	RequestTimeout time.Duration
	browserMu      sync.Mutex
	browsers       map[string]*browserHTTP
	pow            chan struct{}
}

type OpenAIImageInput struct {
	Name string
	MIME string
	Data []byte
}

var openAIImageFileIDPattern = regexp.MustCompile(`(?i)^file_[a-z0-9][a-z0-9_-]{5,}$`)

// OpenAIImageStageFunc receives the elapsed time for a completed image-pipeline
// stage. It is intentionally kept in the provider package so callers can add
// observability without coupling the provider to HTTP handlers.
type OpenAIImageStageFunc func(metric string, elapsed time.Duration)

// OpenAIImageEgressFunc receives the proxy URL selected for an upstream call.
// Callers should sanitize it before exposing it in logs or UI.
type OpenAIImageEgressFunc func(proxyURL string)

type openAIImageStageContextKey struct{}
type openAIImageEgressContextKey struct{}

// WithOpenAIImageStage attaches an optional stage observer to ctx.
func WithOpenAIImageStage(ctx context.Context, observer OpenAIImageStageFunc) context.Context {
	if observer == nil {
		return ctx
	}
	return context.WithValue(ctx, openAIImageStageContextKey{}, observer)
}

// WithOpenAIImageEgress attaches an optional egress observer to ctx.
func WithOpenAIImageEgress(ctx context.Context, observer OpenAIImageEgressFunc) context.Context {
	if observer == nil {
		return ctx
	}
	return context.WithValue(ctx, openAIImageEgressContextKey{}, observer)
}

func notifyOpenAIImageStage(ctx context.Context, metric string, started time.Time) {
	if observer, ok := ctx.Value(openAIImageStageContextKey{}).(OpenAIImageStageFunc); ok && observer != nil {
		observer(metric, time.Since(started))
	}
}

func notifyOpenAIImageEgress(ctx context.Context, proxyURL string) {
	if observer, ok := ctx.Value(openAIImageEgressContextKey{}).(OpenAIImageEgressFunc); ok && observer != nil {
		observer(proxyURL)
	}
}

const openAIImageDefaultPollTimeout = 3 * time.Minute
const openAIImageUploadAttemptTimeout = 60 * time.Second
const openAIImagePollAttemptTimeout = 20 * time.Second
const openAIImageReferenceUploadConcurrency = 3

const (
	openAIImageSlowUpload       = 30 * time.Second
	openAIImageSlowBootstrap    = 15 * time.Second
	openAIImageSlowRequirements = 15 * time.Second
	openAIImageSlowPrepare      = 15 * time.Second
	openAIImageSlowDownload     = 20 * time.Second
)

func (o *OpenAIImage) uploadInputs(ctx context.Context, account wireAccount, inputs []OpenAIImageInput) ([]openAIImageReference, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	uploadCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	references := make([]openAIImageReference, len(inputs))
	jobs := make(chan int, len(inputs))
	for index := range inputs {
		jobs <- index
	}
	close(jobs)

	workerCount := min(len(inputs), openAIImageReferenceUploadConcurrency)
	var workers sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	workers.Add(workerCount)
	for worker := 0; worker < workerCount; worker++ {
		go func() {
			defer workers.Done()
			for index := range jobs {
				if uploadCtx.Err() != nil {
					return
				}
				ref, uploadErr := o.uploadInput(uploadCtx, account, inputs[index], index+1)
				if uploadErr != nil {
					errOnce.Do(func() {
						firstErr = uploadErr
						cancel()
					})
					return
				}
				references[index] = ref
			}
		}()
	}
	workers.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return references, nil
}

// NormalizeOpenAIImageSize preserves automatic sizing and existing UI preset
// aliases. Explicit dimensions are hints, not an exact Web output guarantee.
func NormalizeOpenAIImageSize(size string) string {
	switch strings.ToLower(strings.TrimSpace(size)) {
	case "", "auto":
		return "auto"
	case "1024x1365":
		return "1024x1360"
	case "1365x1024":
		return "1360x1024"
	case "1920x1080":
		return "1920x1088"
	case "1080x1920":
		return "1088x1920"
	default:
		return strings.ToLower(strings.TrimSpace(size))
	}
}

type openAIRequirements struct {
	Token      string
	ProofToken string
	Turnstile  string
	SOToken    string
}

type openAIImageReference struct {
	FileID   string
	FileName string
	FileSize int
	MIME     string
	Width    int
	Height   int
}

func (o *OpenAIImage) bootstrap(ctx context.Context, account wireAccount) ([]string, string, error) {
	response, err := o.do(ctx, http.MethodGet, "/", account, nil, nil, false)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, "", readOpenAIHTTPError(response, "bootstrap")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, "", err
	}
	sources := []string{}
	re := regexp.MustCompile(`(?i)<script[^>]+src=["']([^"']+)["']`)
	for _, match := range re.FindAllStringSubmatch(string(raw), -1) {
		if len(match) < 2 {
			continue
		}
		value, unescapeErr := url.QueryUnescape(html.UnescapeString(match[1]))
		if unescapeErr == nil && value != "" {
			sources = append(sources, value)
		}
	}
	if len(sources) == 0 {
		sources = []string{"https://chatgpt.com/backend-api/sentinel/sdk.js"}
	}
	build := ""
	if match := regexp.MustCompile(`data-build=["']([^"']+)["']`).FindStringSubmatch(string(raw)); len(match) == 2 {
		build = match[1]
	}
	return sources, build, nil
}

func (o *OpenAIImage) chatRequirements(ctx context.Context, account wireAccount, scripts []string, build string) (openAIRequirements, error) {
	// Keep the location stable, but obtain a fresh clock sample after uploads.
	// A slow upload must not make the Sentinel timestamp stale.
	zone := currentOpenAITimezone(ctx)
	profile, err := accountBrowserProfile(account.Fields)
	if err != nil {
		return openAIRequirements{}, err
	}
	legacy := buildLegacyRequirementsToken(profile, scripts, build, zone)
	prepareBody := map[string]any{"p": legacy}
	response, err := o.do(ctx, http.MethodPost, "/backend-api/sentinel/chat-requirements/prepare", account, prepareBody, map[string]string{"Content-Type": "application/json"}, false)
	if err != nil {
		return openAIRequirements{}, err
	}
	var prepare map[string]any
	if err := decodeOpenAIJSON(response, &prepare, "chat requirements prepare"); err != nil {
		return openAIRequirements{}, err
	}
	proof := ""
	if info, ok := prepare["proofofwork"].(map[string]any); ok && boolValue(info["required"]) {
		select {
		case o.pow <- struct{}{}:
		case <-ctx.Done():
			return openAIRequirements{}, ctx.Err()
		}
		budget, cancel := context.WithTimeout(ctx, 5*time.Second)
		proof = buildProofToken(budget, stringValue(info["seed"]), stringValue(info["difficulty"]), profile, scripts, build, zone)
		cancel()
		<-o.pow
		if proof == "" {
			return openAIRequirements{}, fmt.Errorf("Sentinel proof budget exhausted or canceled")
		}
	}
	turnstile := ""
	if info, ok := prepare["turnstile"].(map[string]any); ok && boolValue(info["required"]) {
		dx := stringValue(info["dx"])
		if dx == "" {
			return openAIRequirements{}, fmt.Errorf("unsupported Sentinel challenge")
		}
		if dx != "" {
			turnstile, err = solveSentinelTurnstileToken(ctx, dx, legacy, profile)
			if err != nil {
				return openAIRequirements{}, fmt.Errorf("solve OpenAI sentinel Turnstile: %w", err)
			}
		}
	}
	finalizeBody := map[string]any{
		"prepare_token":   stringValue(prepare["prepare_token"]),
		"proof_token":     proof,
		"turnstile_token": turnstile,
	}
	response, err = o.do(ctx, http.MethodPost, "/backend-api/sentinel/chat-requirements/finalize", account, finalizeBody, map[string]string{"Content-Type": "application/json"}, false)
	if err != nil {
		return openAIRequirements{}, err
	}
	var finalized map[string]any
	if err := decodeOpenAIJSON(response, &finalized, "chat requirements finalize"); err != nil {
		return openAIRequirements{}, err
	}
	token := stringValue(finalized["token"])
	if token == "" {
		return openAIRequirements{}, fmt.Errorf("OpenAI chat requirements returned no token")
	}
	return openAIRequirements{Token: token, ProofToken: proof, Turnstile: turnstile, SOToken: stringValue(finalized["so_token"])}, nil
}

func (o *OpenAIImage) prepare(ctx context.Context, account wireAccount, requirements openAIRequirements, prompt, model string) (string, error) {
	payload := map[string]any{
		"action":                "next",
		"fork_from_shared_post": false,
		"parent_message_id":     firstUUID(),
		"model":                 openAIImageModel(model),
		"client_prepare_state":  "success",
		"conversation_mode":     map[string]any{"kind": "primary_assistant"},
		"system_hints":          []any{"picture_v2"},
		"partial_query": map[string]any{
			"id": firstUUID(), "author": map[string]any{"role": "user"},
			"content": map[string]any{"content_type": "text", "parts": []any{prompt}},
		},
		"supports_buffering":     true,
		"supported_encodings":    []any{"v1"},
		"client_contextual_info": map[string]any{"app_name": "chatgpt.com"},
	}
	applyOpenAITimezone(ctx, payload)
	response, err := o.do(ctx, http.MethodPost, "/backend-api/f/conversation/prepare", account, payload, o.requirementHeaders(requirements), false)
	if err != nil {
		return "", err
	}
	var value map[string]any
	if err := decodeOpenAIJSON(response, &value, "image conversation prepare"); err != nil {
		return "", err
	}
	token := stringValue(value["conduit_token"])
	if token == "" {
		return "", fmt.Errorf("OpenAI image prepare returned no conduit token")
	}
	return token, nil
}

func (o *OpenAIImage) start(ctx context.Context, account wireAccount, requirements openAIRequirements, conduit, prompt, model, size, quality string, references []openAIImageReference, masks ...openAIImageReference) (string, []string, error) {
	parts := make([]any, 0, len(references)+1)
	attachments := make([]any, 0, len(references))
	for _, ref := range references {
		parts = append(parts, map[string]any{
			"content_type":  "image_asset_pointer",
			"asset_pointer": "file-service://" + ref.FileID,
			"width":         ref.Width, "height": ref.Height, "size_bytes": ref.FileSize,
		})
		attachments = append(attachments, map[string]any{
			"id": ref.FileID, "mimeType": ref.MIME, "name": ref.FileName,
			"size": ref.FileSize, "width": ref.Width, "height": ref.Height,
		})
	}
	parts = append(parts, prompt)
	contentType := "text"
	if len(references) > 0 {
		contentType = "multimodal_text"
	}
	metadata := map[string]any{
		"developer_mode_connector_ids": []any{},
		"selected_github_repos":        []any{},
		"selected_all_github_repos":    false,
		"system_hints":                 []any{"picture_v2"},
		"serialization_metadata":       map[string]any{"custom_symbol_offsets": []any{}},
	}
	if len(attachments) > 0 {
		metadata["attachments"] = attachments
	}
	if len(masks) > 0 && masks[0].FileID != "" {
		if len(references) != 1 {
			return "", nil, &ImageOptionError{Param: "mask", Message: "mask requires exactly one source image"}
		}
		metadata["dalle"] = map[string]any{"from_client": map[string]any{"operation": map[string]any{
			"type": "inpainting", "original_file_id": references[0].FileID, "mask_file_id": masks[0].FileID,
		}}}
	}
	payload := map[string]any{
		"action": "next",
		"messages": []any{map[string]any{
			"id": firstUUID(), "author": map[string]any{"role": "user"},
			"create_time": float64(time.Now().UnixNano()) / 1e9,
			"content":     map[string]any{"content_type": contentType, "parts": parts},
			"metadata":    metadata,
		}},
		"parent_message_id":                    firstUUID(),
		"model":                                openAIImageModel(model),
		"client_prepare_state":                 "sent",
		"conversation_mode":                    map[string]any{"kind": "primary_assistant"},
		"enable_message_followups":             true,
		"system_hints":                         []any{"picture_v2"},
		"supports_buffering":                   true,
		"supported_encodings":                  []any{"v1"},
		"client_contextual_info":               map[string]any{"app_name": "chatgpt.com"},
		"paragen_cot_summary_display_override": "allow",
		"force_parallel_switch":                "auto",
		"image_generation_quality":             quality,
	}
	// The Web field is only a legacy hint. In auto mode omit it entirely,
	// allowing the prompt and reference composition to guide the result.
	if size = NormalizeOpenAIImageSize(size); size != "auto" {
		payload["image_generation_size"] = size
	}
	headers := o.requirementHeaders(requirements)
	headers["X-Conduit-Token"] = conduit
	headers["Accept"] = "text/event-stream"
	applyOpenAITimezone(ctx, payload)
	if err := channel.ObserveQuotaSend(ctx, "reason"); err != nil {
		return "", nil, err
	}
	if err := channel.ObserveQuotaSend(ctx, "image_gen"); err != nil {
		return "", nil, err
	}
	if save, ok := ctx.Value(checkpointKey{}).(func(channel.Checkpoint) error); ok {
		if err := save(channel.Checkpoint{Version: 1, Submitted: true}); err != nil {
			return "", nil, err
		}
	}
	response, err := o.do(ctx, http.MethodPost, "/backend-api/f/conversation", account, payload, headers, true)
	if err != nil {
		return "", nil, err
	}
	defer response.Body.Close()
	conversationID := ""
	fileIDs := []string{}
	var terminalErr error
	err = scanOpenAISSE(response.Body, func(raw []byte) bool {
		var value any
		if json.Unmarshal(raw, &value) != nil {
			return false
		}
		notifyUpstreamModel(ctx, value)
		if terminalErr = openAIImageTerminalError(value, webSessionSecrets(account.Fields, account.Token)...); terminalErr != nil {
			return true
		}
		collectOpenAIGeneratedImageRefs(value, &conversationID, &fileIDs)
		if conversationID != "" {
			if save, ok := ctx.Value(checkpointKey{}).(func(channel.Checkpoint) error); ok {
				if e := save(channel.Checkpoint{Submitted: true, Version: 1, ConversationID: conversationID, FileIDs: uniqueStrings(fileIDs)}); e != nil {
					terminalErr = e
					return true
				}
			}
		}
		return false
	})
	if terminalErr != nil {
		return "", nil, terminalErr
	}
	if err != nil {
		return "", nil, err
	}
	return conversationID, uniqueStrings(fileIDs), nil
}

func (o *OpenAIImage) pollConversation(ctx context.Context, account wireAccount, conversationID string) ([]string, error) {
	deadline := o.imagePollDeadline(ctx)
	var lastErr error
	for time.Now().Before(deadline) {
		value, err := o.pollConversationOnce(ctx, account, conversationID)
		if err == nil {
			id := ""
			ids := []string{}
			collectOpenAIGeneratedImageRefs(value, &id, &ids)
			if len(ids) > 0 {
				return uniqueStrings(ids), nil
			}
			if terminal := openAIImageTerminalError(value, webSessionSecrets(account.Fields, account.Token)...); terminal != nil {
				return nil, terminal
			}
			if terminal := openAIImageTextOnlyError(value, account.Token); terminal != nil {
				return nil, terminal
			}
			lastErr = errors.New("upstream response contained no image reference")
		} else if !isRetryableOpenAIError(err) {
			return nil, err
		} else {
			lastErr = err
		}
		wait := 5 * time.Second
		if remaining := time.Until(deadline); remaining < wait {
			wait = remaining
		}
		if wait > 0 {
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("OpenAI image result polling deadline exceeded (last poll error: %s): %w", openAIImagePollErrorSummary(lastErr), ctx.Err())
			case <-time.After(wait):
			}
		}
	}
	message := "OpenAI image result polling timed out"
	if lastErr != nil {
		message += " (last poll error: " + openAIImagePollErrorSummary(lastErr) + ")"
	}
	return nil, &upstreamError{Status: http.StatusGatewayTimeout, Message: message, Body: message}
}

func openAIImagePollErrorSummary(err error) string {
	if err == nil {
		return "none"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "request deadline exceeded"
	}
	var upstream *upstreamError
	if errors.As(err, &upstream) {
		return fmt.Sprintf("upstream HTTP %d", upstream.Status)
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "unexpected eof"):
		return "unexpected EOF"
	case strings.Contains(message, "connection reset"):
		return "connection reset by peer"
	case strings.Contains(message, "timeout"):
		return "network timeout"
	case strings.Contains(message, "decode"):
		return "invalid upstream JSON"
	case strings.Contains(message, "no image reference"):
		return "upstream response contained no image reference"
	default:
		return "poll request failed"
	}
}

// Terminal failures normally must not regenerate, regardless of configured HTTP
// retry codes. Only a recognized transient image-tool failure opts into retry.
type imageTerminalError struct {
	err       error
	detail    ImageErrorDetail
	retryable bool
}

func (e *imageTerminalError) Error() string { return e.err.Error() }
func (e *imageTerminalError) Unwrap() error { return e.err }
func IsImageTerminalError(err error) bool {
	var terminal *imageTerminalError
	return errors.As(err, &terminal)
}

func openAIImageTerminalError(value any, secrets ...string) error {
	reason := openAIImageTerminalReason(value)
	if reason == "" {
		return nil
	}
	detail := imageErrorDetail(reason, secrets...)
	message := imageTerminalPrefix + truncateOpenAIImageReason(strings.TrimPrefix(detail.Message, imageTerminalPrefix))
	return &imageTerminalError{err: &upstreamError{Status: http.StatusUnprocessableEntity, Message: message, Body: message}, detail: detail, retryable: false}
}

func openAIImageTerminalReason(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		if author, ok := typed["author"].(map[string]any); ok {
			switch stringValue(author["role"]) {
			case "user", "system", "developer":
				return ""
			}
		}
		if reason := openAIImageMessageError(typed); reason != "" {
			return reason
		}
		// Read an explicit explanation before generic status markers; map iteration
		// order must not replace "quota exhausted" with only "failed".
		if detail, ok := typed["error"].(map[string]any); ok {
			if reason := firstStringValue(detail, "message", "code", "type"); reason != "" {
				return reason
			}
		} else if reason, ok := typed["error"].(string); ok && strings.TrimSpace(reason) != "" {
			return strings.TrimSpace(reason)
		}
		for _, key := range []string{"status", "state", "task_status", "generation_status"} {
			if openAIImageFailureMarker(typed[key]) {
				if reason, ok := typed["message"].(string); ok && strings.TrimSpace(reason) != "" {
					return strings.TrimSpace(reason)
				}
			}
		}
		for key, item := range typed {
			normalizedKey := strings.ToLower(strings.TrimSpace(key))
			text := strings.ToLower(strings.TrimSpace(stringValue(item)))
			switch normalizedKey {
			case "status", "state", "task_status", "generation_status":
				switch text {
				case "failed", "error", "cancelled", "canceled", "blocked", "moderated", "refused", "rejected", "finished_error":
					return text
				}
			case "content_type", "finish_type", "finish_reason":
				switch text {
				case "refusal", "content_filter", "safety", "moderation":
					return text
				}
			case "error":
				if details, ok := item.(map[string]any); ok {
					if message := firstStringValue(details, "message", "code", "type"); message != "" {
						return strings.TrimSpace(message)
					}
				} else if message, ok := item.(string); ok && strings.TrimSpace(message) != "" {
					return strings.TrimSpace(message)
				}
			}
		}
		for _, item := range typed {
			if reason := openAIImageTerminalReason(item); reason != "" {
				return reason
			}
		}
	case []any:
		for _, item := range typed {
			if reason := openAIImageTerminalReason(item); reason != "" {
				return reason
			}
		}
	}
	return ""
}

// Explicit failure markers may accompany a successful transport/message status.
// Prefer the assistant/tool's business explanation to an opaque status keyword.
func openAIImageMessageError(message map[string]any) string {
	author, _ := message["author"].(map[string]any)
	role := strings.ToLower(strings.TrimSpace(stringValue(author["role"])))
	if role != "assistant" && role != "tool" {
		return ""
	}
	metadata, _ := message["metadata"].(map[string]any)
	content, _ := message["content"].(map[string]any)
	finish, _ := metadata["finish_details"].(map[string]any)
	isError, _ := metadata["is_error"].(bool)
	isError = isError || openAIImageFailureMarker(message["status"]) || openAIImageFailureMarker(content["content_type"]) || openAIImageFailureMarker(message["finish_reason"]) || openAIImageFailureMarker(metadata["finish_reason"]) || openAIImageFailureMarker(finish["type"])
	if !isError {
		return ""
	}
	if text := openAIImageMessageText(message); text != "" {
		return text
	}
	// An unfinished SSE message may carry the flag before its text arrives.
	finished, _ := message["end_turn"].(bool)
	if finished || strings.HasPrefix(stringValue(message["status"]), "finished_") {
		return "upstream reported an image generation error without a description"
	}
	return ""
}

func truncateOpenAIImageReason(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > 160 {
		return string(runes[:160]) + "…"
	}
	return value
}

func (o *OpenAIImage) uploadInput(ctx context.Context, account wireAccount, input OpenAIImageInput, index int) (openAIImageReference, error) {
	return o.uploadInputWithPurpose(ctx, account, input, index, false)
}

func (o *OpenAIImage) uploadInputWithPurpose(ctx context.Context, account wireAccount, input OpenAIImageInput, index int, mask bool) (openAIImageReference, error) {
	if len(input.Data) == 0 {
		return openAIImageReference{}, fmt.Errorf("image input %d is empty", index)
	}
	mimeType := strings.TrimSpace(input.MIME)
	if mimeType == "" {
		mimeType = "image/png"
	}
	width, height := imageDimensions(input.Data)
	name := input.Name
	if name == "" {
		name = fmt.Sprintf("image_%d.png", index)
	}
	payload := map[string]any{
		"file_name": name, "file_size": len(input.Data), "use_case": "multimodal",
		"width": width, "height": height,
	}
	if mask {
		payload["use_case"] = "dalle_agent"
		payload["entry_surface"] = "image_edit_mask"
		payload["store_in_library"] = false
	}
	meta, err := o.prepareInputUpload(ctx, account, payload)
	if err != nil {
		return openAIImageReference{}, err
	}
	fileID := stringValue(meta["file_id"])
	uploadURL := stringValue(meta["upload_url"])
	if fileID == "" || uploadURL == "" {
		return openAIImageReference{}, fmt.Errorf("OpenAI image upload returned incomplete metadata")
	}
	err = o.uploadInputBlob(ctx, account, uploadURL, input.Data, map[string]string{
		"Content-Type": mimeType, "x-ms-blob-type": "BlockBlob", "x-ms-version": "2020-04-08",
		"Accept": "application/json, text/plain, */*",
	})
	if err != nil {
		return openAIImageReference{}, err
	}
	response, err := o.do(ctx, http.MethodPost, "/backend-api/files/"+url.PathEscape(fileID)+"/uploaded", account, map[string]any{}, map[string]string{"Content-Type": "application/json", "Accept": "application/json"}, false)
	if err != nil {
		return openAIImageReference{}, err
	}
	_ = response.Body.Close()
	return openAIImageReference{FileID: fileID, FileName: name, FileSize: len(input.Data), MIME: mimeType, Width: width, Height: height}, nil
}

func isRetryableOpenAIUploadError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var upstream *upstreamError
	if errors.As(err, &upstream) {
		return upstream.Status == http.StatusForbidden || upstream.Status == http.StatusRequestTimeout ||
			upstream.Status == http.StatusTooManyRequests || upstream.Status >= http.StatusInternalServerError
	}
	return openAIImageProxyFailure(err)
}

func isRetryableOpenAITransferError(err error) bool {
	return isRetryableOpenAIUploadError(err)
}

// The remote generation has already completed when this error is returned.
// Callers may report failure/refund, but must not start another generation.
type imageDownloadError struct{ err error }

func (e *imageDownloadError) Error() string { return e.err.Error() }
func (e *imageDownloadError) Unwrap() error { return e.err }
func IsImageDownloadError(err error) bool {
	var download *imageDownloadError
	return errors.As(err, &download)
}

func (o *OpenAIImage) downloadFile(ctx context.Context, account wireAccount, fileID string) ([]byte, string, error) {
	response, err := o.do(ctx, http.MethodGet, "/backend-api/files/"+url.PathEscape(fileID)+"/download", account, nil, nil, false)
	if err != nil {
		return nil, "", err
	}
	return o.readImageDownloadResponse(ctx, account, response)
}

func (o *OpenAIImage) downloadImageRef(ctx context.Context, account wireAccount, conversationID, imageRef string) ([]byte, string, error) {
	if strings.HasPrefix(imageRef, "sediment://") {
		attachmentID := strings.TrimPrefix(imageRef, "sediment://")
		if conversationID == "" || attachmentID == "" {
			return nil, "", fmt.Errorf("OpenAI sediment image reference is incomplete")
		}
		path := "/backend-api/conversation/" + url.PathEscape(conversationID) + "/attachment/" + url.PathEscape(attachmentID) + "/download"
		response, err := o.do(ctx, http.MethodGet, path, account, nil, map[string]string{
			"Accept":                "application/json, image/*, */*",
			"Referer":               o.BaseURL + "/c/" + conversationID,
			"X-OpenAI-Target-Route": "/backend-api/conversation/{conversation_id}/attachment/{attachment_id}/download",
		}, false)
		if err != nil {
			return nil, "", err
		}
		return o.readImageDownloadResponse(ctx, account, response)
	}
	return o.downloadFile(ctx, account, strings.TrimPrefix(imageRef, "file-service://"))
}

func (o *OpenAIImage) readImageDownloadResponse(ctx context.Context, account wireAccount, response *http.Response) ([]byte, string, error) {
	var value map[string]any
	raw, err := io.ReadAll(io.LimitReader(response.Body, (64<<20)+1))
	_ = response.Body.Close()
	if err != nil {
		return nil, "", err
	}
	if strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "application/json") || json.Valid(raw) {
		if json.Unmarshal(raw, &value) != nil {
			return nil, "", fmt.Errorf("decode OpenAI image download metadata")
		}
		downloadURL := firstStringValue(value, "download_url", "url")
		if downloadURL == "" {
			return nil, "", fmt.Errorf("OpenAI image download returned no URL")
		}
		response, err = o.doAbsolute(ctx, http.MethodGet, downloadURL, account, nil, nil, false)
		if err != nil {
			return nil, "", err
		}
		raw, err = io.ReadAll(io.LimitReader(response.Body, (64<<20)+1))
		_ = response.Body.Close()
		if err != nil {
			return nil, "", err
		}
	}
	mimeType, err := validatedImageMIME(raw)
	if err != nil {
		return nil, "", err
	}
	return raw, mimeType, nil
}

// Headers and extensions are advisory. A 200 storage error document is not an
// image; decoding prevents XML/HTML, truncated data and spoofed MIME successes.
func validatedImageMIME(raw []byte) (string, error) {
	invalid := func(message string) (string, error) {
		return "", &upstreamError{Status: http.StatusBadGateway, Message: message}
	}
	if len(raw) == 0 || len(raw) > 64<<20 {
		return invalid("OpenAI image download is empty or exceeds 64 MiB")
	}
	var storageError struct {
		XMLName xml.Name
		Code    string `xml:"Code"`
	}
	trimmed := bytes.TrimSpace(bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf}))
	if len(trimmed) > 0 && trimmed[0] == '<' && len(trimmed) <= 64<<10 && xml.Unmarshal(trimmed, &storageError) == nil && storageError.XMLName.Local == "Error" {
		// Never copy arbitrary upstream messages or signed URLs into client errors.
		if storageError.Code == "ServerBusy" {
			return invalid("OpenAI image storage returned ServerBusy instead of image bytes")
		}
		return invalid("OpenAI image storage returned an error document instead of image bytes")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || config.Width <= 0 || config.Height <= 0 || uint64(config.Width) > 64_000_000/uint64(config.Height) {
		return invalid("OpenAI image download is not a supported image or exceeds the pixel limit")
	}
	// DecodeConfig alone accepts files truncated after their dimensions header.
	if _, _, err := image.Decode(bytes.NewReader(raw)); err != nil {
		return invalid("OpenAI image download contains corrupt or truncated image bytes")
	}
	switch format {
	case "png":
		return "image/png", nil
	case "jpeg":
		return "image/jpeg", nil
	case "gif":
		return "image/gif", nil
	case "webp":
		return "image/webp", nil
	default:
		return invalid("OpenAI image download has an unsupported image format")
	}
}

func (o *OpenAIImage) requirementHeaders(requirements openAIRequirements) map[string]string {
	headers := map[string]string{
		"Content-Type": "application/json",
		"Accept":       "application/json",
		"OpenAI-Sentinel-Chat-Requirements-Token": requirements.Token,
	}
	if requirements.ProofToken != "" {
		headers["OpenAI-Sentinel-Proof-Token"] = requirements.ProofToken
	}
	if requirements.Turnstile != "" {
		headers["OpenAI-Sentinel-Turnstile-Token"] = requirements.Turnstile
	}
	if requirements.SOToken != "" {
		headers["OpenAI-Sentinel-SO-Token"] = requirements.SOToken
	}
	return headers
}

func (o *OpenAIImage) do(ctx context.Context, method, path string, account wireAccount, body any, extra map[string]string, stream bool) (*http.Response, error) {
	if payload, ok := body.(map[string]any); ok && strings.Contains(path, "conversation") {
		applyOpenAIBrowserContext(account.Fields, payload)
	}
	raw, err := json.Marshal(body)
	if body != nil {
		if err != nil {
			return nil, err
		}
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(raw)
	}
	return o.doRequest(ctx, method, o.BaseURL+path, path, account, reader, extra, stream, true)
}

func openAIImageProxyFailure(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var upstream *upstreamError
	if errors.As(err, &upstream) {
		return upstream.Status == http.StatusForbidden
	}
	message := strings.ToLower(err.Error())
	for _, fragment := range []string{
		"timeout", "deadline exceeded", "unexpected eof", "connection reset", "broken pipe",
		"proxyconnect", "dial tcp", "tls handshake", "network is unreachable", "no route to host",
		"connection refused", "connection closed", "server misbehaving",
	} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

func decodeOpenAIJSON(response *http.Response, value any, target string) error {
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return readOpenAIHTTPError(response, target)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(value); err != nil {
		return fmt.Errorf("decode %s response: %w", target, err)
	}
	return nil
}

func readOpenAIHTTPError(response *http.Response, target string, secrets ...string) error {
	raw, _ := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if len(raw) > 64<<10 {
		raw = []byte("upstream response body exceeded diagnostic limit")
	}
	message := strings.TrimSpace(string(raw))
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	if message == "" {
		message = fmt.Sprintf("OpenAI endpoint %s returned HTTP %d", target, response.StatusCode)
	}
	endpoint := target
	if response.Request != nil && response.Request.URL != nil {
		endpoint = response.Request.URL.Path
	}
	return &upstreamError{Status: response.StatusCode, Message: message, Body: message, Endpoint: endpoint}
}

func scanOpenAISSE(reader io.Reader, onPayload func([]byte) bool) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	lines := []string{}
	flush := func() bool {
		if len(lines) == 0 {
			return false
		}
		payload := strings.TrimSpace(strings.Join(lines, "\n"))
		lines = nil
		if payload == "" || payload == "[DONE]" {
			return payload == "[DONE]"
		}
		return onPayload([]byte(payload))
	}
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" {
			if flush() {
				break
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			lines = append(lines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	_ = flush()
	return nil
}

func collectOpenAIImageRefs(value any, conversationID *string, fileIDs *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "conversation_id", "conversationid", "conversation-id":
				if *conversationID == "" {
					*conversationID = stringValue(item)
				}
			}
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "asset_pointer", "assetpointer":
				pointer := stringValue(item)
				if strings.HasPrefix(pointer, "file-service://") {
					*fileIDs = append(*fileIDs, strings.TrimPrefix(pointer, "file-service://"))
				} else if strings.HasPrefix(pointer, "sediment://") {
					*fileIDs = append(*fileIDs, pointer)
				}
			case "file_id", "fileid":
				id := stringValue(item)
				if isOpenAIImageFileID(id) {
					*fileIDs = append(*fileIDs, id)
				}
			}
			collectOpenAIImageRefs(item, conversationID, fileIDs)
		}
	case []any:
		for _, item := range typed {
			collectOpenAIImageRefs(item, conversationID, fileIDs)
		}
	case string:
		if *conversationID == "" {
			if match := regexp.MustCompile(`(?i)"conversation[_-]?id"\s*:\s*"([^"]+)"`).FindStringSubmatch(typed); len(match) == 2 {
				*conversationID = strings.TrimSpace(match[1])
			}
		}
		for _, match := range regexp.MustCompile(`file-service://([A-Za-z0-9_-]+)`).FindAllStringSubmatch(typed, -1) {
			if len(match) == 2 {
				*fileIDs = append(*fileIDs, match[1])
			}
		}
		for _, match := range regexp.MustCompile(`sediment://([A-Za-z0-9_-]+)`).FindAllStringSubmatch(typed, -1) {
			if len(match) == 2 {
				*fileIDs = append(*fileIDs, "sediment://"+match[1])
			}
		}
		for _, match := range regexp.MustCompile(`(?i)\b(file_[a-z0-9][a-z0-9_-]{5,})\b`).FindAllStringSubmatch(typed, -1) {
			if len(match) == 2 {
				*fileIDs = append(*fileIDs, match[1])
			}
		}
	}
}

type openAIImageOutputRecord struct {
	messageID string
	createdAt float64
	refs      []string
}

// collectOpenAIGeneratedImageRefs mirrors ChatGPT's conversation boundary: only
// tool and assistant records in mapping can produce downloadable outputs. User
// records contain uploaded references and must never enter the result list.
func collectOpenAIGeneratedImageRefs(value any, conversationID *string, fileIDs *[]string) {
	collectOpenAIConversationID(value, conversationID)
	root, ok := value.(map[string]any)
	if !ok {
		return
	}
	mapping, ok := root["mapping"].(map[string]any)
	if !ok {
		return
	}
	records := make([]openAIImageOutputRecord, 0, len(mapping))
	for messageID, rawNode := range mapping {
		node, ok := rawNode.(map[string]any)
		if !ok {
			continue
		}
		message, ok := node["message"].(map[string]any)
		if !ok {
			continue
		}
		author, _ := message["author"].(map[string]any)
		role := strings.ToLower(strings.TrimSpace(stringValue(author["role"])))
		if role != "tool" && role != "assistant" {
			continue
		}
		content := message["content"]
		metadata := message["metadata"]
		hasAssetPointer := hasOpenAIImageAssetPointer(content) || hasOpenAIImageAssetPointer(metadata)
		refs := []string{}
		if role == "assistant" {
			if !hasAssetPointer {
				continue
			}
			collectOpenAIAssetPointerRefs(content, &refs)
			collectOpenAIAssetPointerRefs(metadata, &refs)
		} else {
			unusedConversationID := ""
			collectOpenAIImageRefs(map[string]any{"content": content, "metadata": metadata}, &unusedConversationID, &refs)
		}
		refs = uniqueStrings(refs)
		filtered := refs[:0]
		for _, ref := range refs {
			if strings.TrimSpace(ref) != "file_upload" {
				filtered = append(filtered, ref)
			}
		}
		if len(filtered) == 0 {
			continue
		}
		createdAt, _ := numberValue(message["create_time"])
		records = append(records, openAIImageOutputRecord{messageID: messageID, createdAt: createdAt, refs: filtered})
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].createdAt == records[j].createdAt {
			return records[i].messageID < records[j].messageID
		}
		return records[i].createdAt < records[j].createdAt
	})
	for _, record := range records {
		*fileIDs = append(*fileIDs, record.refs...)
	}
}

func collectOpenAIConversationID(value any, conversationID *string) {
	if *conversationID != "" {
		return
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			normalized := strings.ToLower(strings.TrimSpace(key))
			if normalized == "conversation_id" || normalized == "conversationid" || normalized == "conversation-id" {
				*conversationID = stringValue(item)
				if *conversationID != "" {
					return
				}
			}
			collectOpenAIConversationID(item, conversationID)
		}
	case []any:
		for _, item := range typed {
			collectOpenAIConversationID(item, conversationID)
		}
	}
}

func hasOpenAIImageAssetPointer(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		if strings.EqualFold(strings.TrimSpace(stringValue(typed["content_type"])), "image_asset_pointer") {
			return true
		}
		pointer := strings.TrimSpace(stringValue(typed["asset_pointer"]))
		if strings.HasPrefix(pointer, "file-service://") || strings.HasPrefix(pointer, "sediment://") {
			return true
		}
		for _, item := range typed {
			if hasOpenAIImageAssetPointer(item) {
				return true
			}
		}
	case []any:
		for _, item := range typed {
			if hasOpenAIImageAssetPointer(item) {
				return true
			}
		}
	}
	return false
}

func collectOpenAIAssetPointerRefs(value any, refs *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		pointer := strings.TrimSpace(stringValue(typed["asset_pointer"]))
		if strings.HasPrefix(pointer, "file-service://") {
			*refs = append(*refs, strings.TrimPrefix(pointer, "file-service://"))
		} else if strings.HasPrefix(pointer, "sediment://") {
			*refs = append(*refs, pointer)
		}
		for _, item := range typed {
			collectOpenAIAssetPointerRefs(item, refs)
		}
	case []any:
		for _, item := range typed {
			collectOpenAIAssetPointerRefs(item, refs)
		}
	}
}

func isOpenAIImageFileID(value string) bool {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "file-service://") {
		value = strings.TrimPrefix(value, "file-service://")
	}
	return openAIImageFileIDPattern.MatchString(value)
}

func openAIImageModel(model string) string {

	return strings.TrimSpace(model)
}

func buildLegacyRequirementsToken(profile browserprofile.Profile, scripts []string, build string, zone openAITimezoneSnapshot) string {
	config := []any{
		fmt.Sprintf("%dx%d", profile.ScreenWidth, profile.ScreenHeight), zone.date(),
		profile.JSHeapSizeLimit, 1, profile.UserAgent, firstStringValue(map[string]any{"script": firstNonEmptyString(scripts, "https://chatgpt.com/backend-api/sentinel/sdk.js")}, "script"),
		build, profile.NavigatorLanguage, profile.NavigatorLanguagesValue(), mathrand.Float64(),
		"webdriver-false", "location", "navigator", mathrand.Float64() * 1000, firstUUID(), "",
		profile.HardwareConcurrency, zone.at.UnixMilli(), 0, 0, 0, 0, 0, 0, 0, 0,
	}
	raw, _ := json.Marshal(config)
	return "gAAAAAC" + base64.StdEncoding.EncodeToString(raw)
}

func buildProofToken(ctx context.Context, seed, difficulty string, profile browserprofile.Profile, scripts []string, build string, zone openAITimezoneSnapshot) string {
	target, err := hexBytes(difficulty)
	if err != nil || len(target) == 0 {
		return ""
	}
	config := []any{
		fmt.Sprintf("%dx%d", profile.ScreenWidth, profile.ScreenHeight), zone.date(), profile.JSHeapSizeLimit, 0, profile.UserAgent,
		firstStringValue(map[string]any{"script": firstNonEmptyString(scripts, "https://chatgpt.com/backend-api/sentinel/sdk.js")}, "script"),
		build, profile.NavigatorLanguage, profile.NavigatorLanguagesValue(), 0.5, "webdriver-false", "location", "navigator",
		0, firstUUID(), "", profile.HardwareConcurrency, zone.at.UnixMilli(), 0, 0, 0, 0, 0, 0, 0, 0,
	}
	for i := 0; i < 500000; i++ {
		if i%256 == 0 && ctx.Err() != nil {
			return ""
		}
		config[3] = i
		config[9] = i >> 1
		raw, _ := json.Marshal(config)
		encoded := base64.StdEncoding.EncodeToString(raw)
		sum := sha3.Sum512(append([]byte(seed), []byte(encoded)...))
		if len(target) <= len(sum) && bytes.Compare(sum[:len(target)], target) <= 0 {
			return "gAAAAAB" + encoded
		}
	}
	return ""
}

func hexBytes(value string) ([]byte, error) {
	if len(value)%2 != 0 {
		return nil, fmt.Errorf("invalid hex difficulty")
	}
	result := make([]byte, len(value)/2)
	for i := range result {
		var parsed uint8
		if _, err := fmt.Sscanf(value[i*2:i*2+2], "%02x", &parsed); err != nil {
			return nil, err
		}
		result[i] = parsed
	}
	return result, nil
}

func imageDimensions(raw []byte) (int, int) {
	config, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return 1024, 1024
	}
	return config.Width, config.Height
}

func randomMediaID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return fmt.Sprintf("%x", raw[:])
	}
	return fmt.Sprintf("%x", time.Now().UnixNano())
}

func firstStringValue(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if text := stringValue(value[key]); text != "" {
			return text
		}
	}
	return ""
}

func firstNonEmptyString(values []string, fallback string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return fallback
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	result := []string{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func minDuration(a, b time.Duration) time.Duration {
	if a <= 0 {
		return b
	}
	if a < b {
		return a
	}
	return b
}

func isRetryableOpenAIError(err error) bool {
	var upstream *upstreamError
	if errors.As(err, &upstream) {
		return upstream.Status == 404 || upstream.Status == 409 || upstream.Status == 429 || upstream.Status >= 500
	}
	return true
}
