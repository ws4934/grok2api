// Protocol portions adapted from SkyKoo/gptGrok2api at 15773099641575776025e21d1290b5209b058c5a. See LICENSE.reference.
package chatgpt

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
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

type openAIImageReference struct {
	FileID   string
	FileName string
	FileSize int
	MIME     string
	Width    int
	Height   int
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
	if err := markSubmitted(ctx); err != nil {
		return "", nil, err
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
		if terminalErr = openAIImageTerminalError(value, account.Token); terminalErr != nil {
			return true
		}
		known := conversationID != ""
		collectOpenAIGeneratedImageRefs(value, &conversationID, &fileIDs)
		if !known && conversationID != "" {
			if terminalErr = observeConversation(ctx, conversationID); terminalErr != nil {
				return true
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
	endpoint, upstreamURL := target, ""
	if response.Request != nil && response.Request.URL != nil {
		endpoint = response.Request.URL.Path
		upstreamURL = (&url.URL{Scheme: response.Request.URL.Scheme, Host: response.Request.URL.Host, Path: response.Request.URL.Path}).String()
	}
	return &upstreamError{URL: upstreamURL, Status: response.StatusCode, Message: message, Body: message, Endpoint: endpoint, Challenge: isCloudflareChallenge(response.StatusCode, response.Header, string(raw)), RetryAfter: response.Header.Get("Retry-After")}
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

func openAIImageModel(model string) string {

	return strings.TrimSpace(model)
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
