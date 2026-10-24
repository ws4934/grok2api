package muse

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
)

// museError is a failure the gateway can act on: its status drives credential
// refresh (401), quota reconciliation (429) and failover; request-scoped ones
// go back to the client because another account would fail the same way.
type museError struct {
	status        int
	code          string
	message       string
	requestScoped bool
	cause         error
}

func (e *museError) Error() string {
	if e.cause != nil {
		return e.message + ": " + e.cause.Error()
	}
	return e.message
}

func (e *museError) Unwrap() error              { return e.cause }
func (e *museError) HTTPStatusCode() int        { return e.status }
func (e *museError) RequestScopedFailure() bool { return e.requestScoped }
func (e *museError) PublicErrorMessage() string { return e.message }

// Is lets a rejected session match provider.ErrUnauthorized.
func (e *museError) Is(target error) bool {
	return target == provider.ErrUnauthorized && e.status == http.StatusUnauthorized
}

func badRequest(message string) *museError {
	return &museError{status: http.StatusBadRequest, code: "invalid_request", message: message, requestScoped: true}
}

// classify maps an engine or session error to a museError; unknown errors stay
// as they are (transport-like: the gateway penalizes the account and fails over).
func classify(err error) error {
	var known *museError
	var textOnly *textOnlyError
	switch {
	case err == nil || errors.As(err, &known):
		return err
	case errors.As(err, &textOnly):
		return &museError{status: http.StatusUnprocessableEntity, code: "no_media_generated", message: "Muse 没有生成文件，回复：" + truncate(textOnly.Text, 300), requestScoped: true, cause: err}
	case errors.Is(err, errLoggedOut):
		return &museError{status: http.StatusUnauthorized, code: "invalid_credential", message: "Muse 会话已失效，请重新导入 Cookie", cause: err}
	case errors.Is(err, errQuotaExhausted):
		return &museError{status: http.StatusTooManyRequests, code: "usage_limit_reached", message: "Muse 账号本周额度已用完", cause: err}
	case errors.Is(err, errBrowserUnavailable):
		return &museError{status: http.StatusServiceUnavailable, code: "browser_unavailable", message: "Muse 浏览器服务不可用，请检查 provider.muse.browserURL", requestScoped: true, cause: err}
	case errors.Is(err, errNotDelivered):
		return &museError{status: http.StatusBadGateway, code: "prompt_not_delivered", message: "Muse 未确认收到消息", cause: err}
	case errors.Is(err, errTimeout):
		return &museError{status: http.StatusGatewayTimeout, code: "upstream_timeout", message: "Muse 回复超时", cause: err}
	}
	return err
}

// failureResponse renders a classified failure for the gateway; unknown
// errors are returned as errors.
func failureResponse(operation string, err error) (*provider.Response, error) {
	var known *museError
	if !errors.As(classify(err), &known) {
		return nil, err
	}
	return errorResponse(operation, known.status, known.code, known.message), nil
}

// errorResponse renders an OpenAI error, or a Messages-format one for Messages.
func errorResponse(operation string, status int, code, message string) *provider.Response {
	openAIType, messagesType := "invalid_request_error", "invalid_request_error"
	switch {
	case status >= 500:
		openAIType, messagesType = "server_error", "api_error"
	case status == http.StatusUnauthorized:
		openAIType, messagesType = "authentication_error", "authentication_error"
	case status == http.StatusTooManyRequests:
		openAIType, messagesType = "rate_limit_error", "rate_limit_error"
	}
	body := map[string]any{"error": map[string]any{"type": openAIType, "code": code, "message": message, "param": nil}}
	if operation == conversation.OperationMessages {
		body = map[string]any{"type": "error", "error": map[string]any{"type": messagesType, "message": message}}
	}
	return jsonResponse(status, body)
}

func jsonResponse(status int, body any) *provider.Response {
	data, _ := json.Marshal(body)
	header := http.Header{"Content-Type": {"application/json"}}
	return &provider.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)), Header: header, Body: io.NopCloser(bytes.NewReader(data))}
}

// truncate cuts at a rune boundary; the text may end up in audit rows.
func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}
