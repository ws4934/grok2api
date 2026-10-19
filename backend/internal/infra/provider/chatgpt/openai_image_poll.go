// Protocol portions adapted from SkyKoo/gptGrok2api at 15773099641575776025e21d1290b5209b058c5a. See LICENSE.reference.
package chatgpt

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// imagePollInterval spaces the image result polls; imagePollMissing is how
// many consecutive 404s mean the conversation is gone rather than not ready.
var (
	imagePollInterval = 5 * time.Second
	imagePollMissing  = 12
)

func (o *OpenAIImage) pollConversation(ctx context.Context, account wireAccount, conversationID string) ([]string, error) {
	deadline := o.imagePollDeadline(ctx)
	var lastErr error
	missing := 0
	for time.Now().Before(deadline) {
		value, err := o.pollConversationOnce(ctx, account, conversationID)
		var upstream *upstreamError
		if errors.As(err, &upstream) && upstream.Status == http.StatusNotFound {
			if missing++; missing >= imagePollMissing {
				return nil, err
			}
		} else {
			missing = 0
		}
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
		wait := imagePollInterval
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

func isRetryableOpenAIError(err error) bool {
	var upstream *upstreamError
	if errors.As(err, &upstream) {
		return upstream.Status == 404 || upstream.Status == 409 || upstream.Status == 429 || upstream.Status >= 500
	}
	return true
}
