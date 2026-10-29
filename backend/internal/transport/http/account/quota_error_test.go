package account

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/gin-gonic/gin"
)

type blockedQuotaError struct{}

func (blockedQuotaError) Error() string                     { return "private Cookie: sso=secret-value" }
func (blockedQuotaError) Unwrap() error                     { return provider.ErrEgressBlocked }
func (blockedQuotaError) RetryAfterDuration() time.Duration { return 1500 * time.Millisecond }
func TestQuotaEgressErrorPreservesProtocolAndSafeRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Set("requestId", "req-1")
	new(Handler).writeServiceError(ctx, "quotaRefreshFailed", blockedQuotaError{}, 502, "同步账号额度失败")
	body := recorder.Body.String()
	if recorder.Code != 503 || recorder.Header().Get("Retry-After") != "2" || !strings.Contains(body, `"code":"quotaSyncEgressBlocked"`) || !strings.Contains(body, `"requestId":"req-1"`) || !strings.Contains(body, "Cloudflare") || strings.Contains(body, "secret-value") {
		t.Fatalf("status=%d headers=%v body=%s", recorder.Code, recorder.Header(), body)
	}
	recorder = httptest.NewRecorder()
	ctx, _ = gin.CreateTestContext(recorder)
	new(Handler).writeServiceError(ctx, "quotaRefreshFailed", errors.New("Bearer private-token"), 502, "同步账号额度失败")
	if recorder.Code != 502 || strings.Contains(recorder.Body.String(), "private-token") {
		t.Fatal(recorder.Body.String())
	}
}
