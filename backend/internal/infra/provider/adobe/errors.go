package adobe

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// transportError 标记出口/网络失败：请求没拿到可用的 Adobe 响应，网关可换出口重试。
type transportError struct{ err error }

func (e *transportError) Error() string {
	if e == nil || e.err == nil {
		return "adobe transport error"
	}
	return e.err.Error()
}
func (e *transportError) Unwrap() error { return e.err }

// upstreamError 是非 2xx 的 Adobe 响应。网关按 HTTPStatusCode 决策：401 刷新凭据，
// 429 对账积分窗口，提交前的 4xx 可换账号，提交后的 5xx 视为结果未知。
type upstreamError struct {
	stage  string
	status int
	body   string
	// reason 是 Adobe 响应体里的可读原因（message 等字段），可展示给客户端。
	reason string
	// quota 表示账号积分耗尽，按 429 上报，让网关耗尽该账号的 credits 窗口。
	quota bool
}

func (e *upstreamError) Error() string {
	if e == nil {
		return "adobe upstream error"
	}
	msg := fmt.Sprintf("adobe %s failed with status %d", e.stage, e.status)
	if e.body != "" {
		msg += ": " + e.body
	}
	return msg
}

func (e *upstreamError) HTTPStatusCode() int {
	if e.quota {
		return http.StatusTooManyRequests
	}
	return e.status
}

// Is 让 401 匹配 provider.ErrUnauthorized（视频任务据此换账号）。
func (e *upstreamError) Is(target error) bool {
	return target == provider.ErrUnauthorized && e.status == http.StatusUnauthorized
}

// RequestScopedFailure 标记针对请求本身的拒绝（参数、内容审核）：换账号也会被同样拒绝。
func (e *upstreamError) RequestScopedFailure() bool {
	return !e.quota && (e.status == http.StatusBadRequest || e.status == http.StatusRequestEntityTooLarge || e.status == http.StatusUnprocessableEntity)
}

var quotaBodyPattern = regexp.MustCompile(`(?i)quota|insufficient[ _]credit|not enough credit|credits? (?:exhausted|depleted|exceeded)|limit[ _]exceeded`)

// classifyUpstream 把 Adobe 的 HTTP 状态与响应体归类成 upstreamError。
func classifyUpstream(stage string, status int, body string) *upstreamError {
	return &upstreamError{
		stage: stage, status: status, body: truncate(body, 300), reason: upstreamReason(body),
		quota: status == http.StatusPaymentRequired || (status >= 400 && status < 500 && status != http.StatusUnauthorized && quotaBodyPattern.MatchString(body)),
	}
}

// upstreamReason 取 Adobe JSON 错误体里的可读原因。
func upstreamReason(body string) string {
	var data map[string]any
	if json.Unmarshal([]byte(body), &data) != nil {
		return ""
	}
	if nested, ok := data["error"].(map[string]any); ok {
		data = nested
	}
	return truncate(firstString(data, "message", "error_message", "reason", "detail", "error_code"), 200)
}

// requestError 标记一个换账号/换出口也无法解决的拒绝；status 为 0 时按 400。
type requestError struct {
	msg    string
	status int
}

func (e *requestError) Error() string              { return e.msg }
func (e *requestError) RequestScopedFailure() bool { return true }
func (e *requestError) PublicErrorMessage() string { return e.msg }
func (e *requestError) HTTPStatusCode() int {
	if e.status != 0 {
		return e.status
	}
	return http.StatusBadRequest
}

var (
	// errNoToken 是账号没有 access token：按 401 上报，网关会刷新凭据后重试。
	errNoToken = &upstreamError{stage: "auth", status: http.StatusUnauthorized, body: "account has no access token"}
	// errMintUnavailable 表示 mint 服务没给出 x-arp-session-id：请求未发出，换账号也无济于事。
	errMintUnavailable = &requestError{msg: "Adobe 会话令牌服务（mint）暂不可用", status: http.StatusServiceUnavailable}
)

// truncate 按字节截断并丢弃被切开的 UTF-8 字符（文本会落库，PostgreSQL 拒绝非法 UTF-8）。
func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return strings.ToValidUTF8(value[:max], "")
}

// errorResponse 渲染 OpenAI 风格的错误响应。
func errorResponse(status int, code, message string) *provider.Response {
	errType := "invalid_request_error"
	switch {
	case status >= 500:
		errType = "server_error"
	case status == http.StatusUnauthorized:
		errType = "authentication_error"
	case status == http.StatusForbidden:
		errType = "permission_error"
	case status == http.StatusTooManyRequests:
		errType = "rate_limit_error"
	}
	return jsonResponse(status, map[string]any{"error": map[string]any{"type": errType, "code": code, "message": message, "param": nil}})
}

// syncFailure 决定同步图像失败怎么交给网关：Adobe 尚未接受任务时的明确拒绝（4xx、
// 本地请求错误）转成响应，网关据此刷新凭据、对账积分或换账号；其余（已提交、
// 5xx、传输失败）保持为错误，网关绝不重新提交。
func syncFailure(err error, accepted bool) (*provider.Response, error) {
	status := provider.ErrorHTTPStatusOrZero(err)
	if accepted || status == 0 || (status >= 500 && !provider.IsRequestScopedError(err)) {
		return nil, err
	}
	var ue *upstreamError
	if !errors.As(err, &ue) {
		return errorResponse(status, "invalid_request", err.Error()), nil
	}
	code, message := "upstream_error", "Adobe 拒绝了该请求"
	switch {
	case ue.quota:
		code, message = "insufficient_credits", "Adobe 账号积分不足"
	case ue.status == http.StatusUnauthorized:
		code, message = "invalid_credential", "Adobe 拒绝了账号凭据"
	case ue.status == http.StatusForbidden:
		code, message = "upstream_forbidden", "Adobe 拒绝了该账号的请求"
	case ue.status == http.StatusTooManyRequests:
		code, message = "rate_limit_exceeded", "Adobe 请求过于频繁，请稍后重试"
	case ue.RequestScopedFailure():
		code = "upstream_rejected_request"
	}
	if ue.reason != "" {
		message += "：" + ue.reason
	}
	return errorResponse(status, code, message), nil
}
