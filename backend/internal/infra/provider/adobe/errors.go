package adobe

import (
	"fmt"
	"regexp"
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

// upstreamError 是非 2xx 的 Adobe 响应。retryable 触发换账号/换出口；permanent 表示
// 凭据问题（401/403）；quota 表示账号积分耗尽。
type upstreamError struct {
	stage     string
	status    int
	body      string
	retryable bool
	permanent bool
	quota     bool
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

var quotaBodyPattern = regexp.MustCompile(`(?i)quota|credit|balance|insufficient|limit\s*exceeded`)

var chromeVersion = regexp.MustCompile(`Chrome/(\d+)`)

// classifyUpstream 把 Adobe 的 HTTP 状态与响应体归类成 upstreamError。
func classifyUpstream(stage string, status int, body string) *upstreamError {
	err := &upstreamError{stage: stage, status: status, body: truncate(body, 300)}
	switch {
	case status == 401 || status == 403:
		err.permanent = true
	case status == 402 || quotaBodyPattern.MatchString(body):
		err.quota = true
	case status == 429:
		err.retryable = true
	case status == 408 || status == 425 || status == 451 || status >= 500:
		err.retryable = true
	}
	return err
}

// requestError 标记一个换账号/换出口也无法解决的请求级拒绝（返回 4xx）。
type requestError struct{ msg string }

func (e *requestError) Error() string              { return e.msg }
func (e *requestError) RequestScopedFailure() bool { return true }
func (e *requestError) PublicErrorMessage() string { return e.msg }

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max]
}
