package chatgpt

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
)

// quotaRejection reads explicit error/detail fields, never assistant prose or
// tool output. ChatGPT can report a spent allowance inside a successful SSE
// response instead of an HTTP 429.
func quotaRejection(value any) *upstreamError {
	root, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	for _, key := range []string{"error", "detail"} {
		problem := root[key]
		fields, _ := problem.(map[string]any)
		code := strings.ToLower(aiString(fields, "code", "type"))
		message := detailText(problem)
		text := strings.ToLower(message)
		limited := strings.Contains(code, "rate_limit")
		switch code {
		case "usage_limit_reached", "model_cap_exceeded", "insufficient_quota", "quota_exceeded", "quota_exhausted", "image_generation_limit_exceeded":
			limited = true
		}
		if code == "" && (strings.Contains(text, "you've reached the limit of messages") || strings.Contains(text, "you have reached the limit of messages") || strings.Contains(text, "you've reached your image creation limit") || strings.Contains(text, "you've hit your image generation limit")) {
			limited = true
		}
		if !limited {
			continue
		}
		if code == "" {
			code = "rate_limit_exceeded"
		}
		if message == "" {
			message = "ChatGPT usage limit reached"
		}
		body, _ := json.Marshal(map[string]any{"error": map[string]string{"code": code, "message": message}})
		retryAfter := quotaRetryAfter(fields)
		if retryAfter == "" {
			retryAfter = quotaRetryAfter(root)
		}
		return &upstreamError{Status: http.StatusTooManyRequests, Message: message, Body: string(body), RetryAfter: retryAfter}
	}
	return nil
}

func quotaRetryAfter(fields map[string]any) string {
	for _, key := range []string{"retry_after", "retry_after_seconds", "clears_in"} {
		var seconds float64
		switch value := fields[key].(type) {
		case float64:
			seconds = value
		case int:
			seconds = float64(value)
		case string:
			seconds, _ = strconv.ParseFloat(value, 64)
		}
		if seconds > 0 && seconds <= math.MaxInt32 {
			return strconv.FormatInt(int64(math.Ceil(seconds)), 10)
		}
	}
	return ""
}

// imageSubmissionRejected is evidence that the create operation did not begin
// drawing. A poll/download failure or an uncertain transport failure is not
// permission to submit the image again on another account.
func imageSubmissionRejected(err error) bool {
	var terminal *imageTerminalError
	if errors.As(err, &terminal) {
		return terminal.retryable
	}
	var upstream *upstreamError
	return errors.As(err, &upstream) && upstream.Status >= 400 && upstream.Status < 500 && upstream.Endpoint == "/backend-api/f/conversation"
}
