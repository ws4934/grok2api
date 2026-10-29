package web

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

const minimumFreshWebVideoTime = 10 * time.Second

type webVideoResultError struct {
	reason      string
	elapsed     time.Duration
	upstreamURL string
	completed   bool
}

func (e *webVideoResultError) Error() string {
	if e.reason == "too_fast" {
		return fmt.Sprintf("视频生成结果异常：%d ms 内返回，未超过10秒正常生成下限", e.elapsed.Milliseconds())
	}
	return "视频生成结果异常：生成流未提供可确认完成的有效视频"
}
func (*webVideoResultError) HTTPStatusCode() int { return http.StatusBadGateway }
func (*webVideoResultError) PublicErrorMessage() string {
	return "视频生成结果异常，请重试。"
}

func (e *webVideoResultError) UpstreamDiagnostic() (string, provider.DiagnosticResponse) {
	return e.upstreamURL, provider.DiagnosticResponse{StatusCode: http.StatusOK, Status: http.StatusText(http.StatusOK), Body: []byte(e.Error())}
}

// Only a definitively rejected result can clear the irreversible submission
// marker. A failed durable clear remains uncertain and must never fail over.
func rejectWebVideoResult(request provider.VideoRequest, cause error) error {
	if request.SaveCheckpoint != nil {
		if err := request.SaveCheckpoint(""); err != nil {
			return provider.WrapVideoStage(provider.VideoStageSubmitted, 0, errors.Join(cause, err))
		}
	}
	return provider.WrapVideoStage(provider.VideoStageResultRejected, http.StatusBadGateway, cause)
}

type webVideoModerationError struct{}

func (*webVideoModerationError) Error() string              { return provider.ContentPolicyMessage }
func (*webVideoModerationError) HTTPStatusCode() int        { return http.StatusBadRequest }
func (*webVideoModerationError) RequestScopedFailure() bool { return true }
func (*webVideoModerationError) PublicErrorMessage() string { return provider.ContentPolicyMessage }
