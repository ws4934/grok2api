package gateway

import (
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	neterrorpkg "github.com/chenyme/grok2api/backend/internal/pkg/neterror"
)

const unlimitedRoutingAttempts = -1

type routingAttemptPolicy struct {
	limit     int
	unlimited bool
}

func newRoutingAttemptPolicy(configured int) routingAttemptPolicy {
	if configured == unlimitedRoutingAttempts {
		return routingAttemptPolicy{unlimited: true}
	}
	if configured <= 0 {
		configured = 3
	}
	return routingAttemptPolicy{limit: configured}
}

func newRequestRoutingAttemptPolicy(configured int, pinned bool) routingAttemptPolicy {
	if pinned {
		return newRoutingAttemptPolicy(1)
	}
	return newRoutingAttemptPolicy(configured)
}

func (p routingAttemptPolicy) allows(attempt int) bool {
	return p.unlimited || attempt < p.limit
}

func (p routingAttemptPolicy) hasNext(attempt int) bool {
	return p.unlimited || attempt+1 < p.limit
}

// nonAccountFailureFingerprintLimit 仅限制非账号归因故障（网络/5xx 等）。
// 账号级失败持续换号，避免少量瞬时上游故障过早放弃仍可用的凭证池。
const nonAccountFailureFingerprintLimit = 16

// Stream idle failures are commonly provider-wide rather than account-wide.
// Allow one compensating account switch, then stop to prevent a silent
// upstream from multiplying a long idle deadline across the whole pool.
const streamIdleFailureFingerprintLimit = 2

var freeQuotaUsagePattern = regexp.MustCompile(`(?i)tokens\s*\(actual/limit\)\s*:\s*([0-9]+)\s*/\s*([0-9]+)`)

func isUpstreamStreamFailure(errorCode string) bool {
	switch errorCode {
	case "upstream_stream_incomplete", "upstream_stream_interrupted", "upstream_stream_idle_timeout", "upstream_response_empty", "upstream_output_loop":
		return true
	default:
		return false
	}
}

func streamFailureHealthPenalty(errorCode string, usage Usage, idleCooldown time.Duration) (int, time.Duration) {
	if (errorCode == "upstream_stream_idle_timeout" || errorCode == "upstream_response_empty") && !usage.OutputObserved && usage.OutputTokens == 0 && usage.ReasoningTokens == 0 {
		if idleCooldown <= 0 {
			idleCooldown = qualityIdleAccountCooldown
		}
		if errorCode == "upstream_response_empty" {
			return http.StatusBadGateway, idleCooldown
		}
		return http.StatusGatewayTimeout, idleCooldown
	}
	return 0, 0
}

// upstreamResponseErrorHealthPenalty classifies failures that happen after a
// successful response header but before a usable non-streaming body exists.
// A truly empty response receives the configured long cooldown. If some body
// bytes arrived before an idle timeout, retain only the ordinary short network
// cooldown by returning a zero status/retry pair.
func upstreamResponseErrorHealthPenalty(err error, idleCooldown time.Duration) (int, time.Duration, bool) {
	switch {
	case neterrorpkg.IsUpstreamResponseEmpty(err):
		status, cooldown := streamFailureHealthPenalty("upstream_response_empty", Usage{}, idleCooldown)
		return status, cooldown, true
	case neterrorpkg.IsUpstreamStreamIdleTimeout(err):
		if neterrorpkg.IdleTimeoutObservedData(err) {
			return 0, 0, true
		}
		status, cooldown := streamFailureHealthPenalty("upstream_stream_idle_timeout", Usage{}, idleCooldown)
		return status, cooldown, true
	default:
		return 0, 0, false
	}
}

func isRetryableTransportFailure(policy provider.FailurePolicy, err error) bool {
	return !policy.FatalHeaderTimeout || !neterrorpkg.IsResponseHeaderTimeout(err)
}

func readRetryableBody(body io.ReadCloser) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	defer body.Close()
	data, _, err := provider.ReadDiagnosticBody(body)
	return data, err
}

func parseFreeQuotaExhaustion(body []byte) (int64, int64, bool) {
	text := strings.ToLower(string(body))
	if !strings.Contains(text, "subscription:free-usage-exhausted") {
		return 0, 0, false
	}
	matches := freeQuotaUsagePattern.FindSubmatch(body)
	if len(matches) != 3 {
		return 0, 0, true
	}
	used, usedErr := strconv.ParseInt(string(matches[1]), 10, 64)
	limit, limitErr := strconv.ParseInt(string(matches[2]), 10, 64)
	if usedErr != nil || limitErr != nil {
		return 0, 0, true
	}
	return used, limit, true
}

// shouldStopForNonAccountFingerprint 仅对非账号归因故障累计指纹并在达到阈值后停止换号。
// 账号级失败（额度、鉴权、冷却等）继续轮询其它凭证。
// 未知 403、Team 模型限流只跳过当前号，不累计指纹、不提前结束整次请求。
func shouldStopForNonAccountFingerprint(fingerprints map[string]int, failure *UpstreamFailure) bool {
	if failure == nil || failure.AccountScoped || failure.Fingerprint == "" {
		return false
	}
	if failure.HTTPStatus == http.StatusForbidden {
		return false
	}
	if failure.Fingerprint == "429:team_model_rate_limit" {
		return false
	}
	fingerprints[failure.Fingerprint]++
	limit := nonAccountFailureFingerprintLimit
	if failure.Code == "upstream_stream_idle_timeout" || failure.Fingerprint == "upstream_stream_idle_timeout" || failure.Code == "upstream_stream_empty" || failure.Fingerprint == "upstream_stream_empty" {
		limit = streamIdleFailureFingerprintLimit
	}
	return fingerprints[failure.Fingerprint] >= limit
}

func isRetryable(status int) bool {
	return status == 402 || status == 403 || status == 429 || status >= 500
}

// isBuildTransientRetryStatus mirrors the Grok Build client's turn-level
// resubmit classification. Cloudflare 525/526 report an origin TLS
// configuration failure and are deliberately excluded from transient traffic.
func isBuildTransientRetryStatus(status int) bool {
	return status >= http.StatusInternalServerError && status <= 599 && status != 525 && status != 526
}

func isBuildTransientRetryError(err error) bool {
	return err != nil &&
		!neterrorpkg.IsUpstreamResponseEmpty(err) &&
		!errors.Is(err, errQualityEmptyStream) &&
		!neterrorpkg.IsUpstreamOutputLoop(err)
}

func isRetryableResponse(response *provider.Response, policy provider.FailurePolicy) bool {
	if response == nil {
		return false
	}
	// The official client treats Cloudflare origin-TLS failures as fatal,
	// even when the edge asks clients to retry. Account rotation cannot repair them.
	if policy.FatalOriginTLS && (response.StatusCode == 525 || response.StatusCode == 526) {
		return false
	}
	if response.StatusCode == http.StatusBadRequest && policy.ForceAccountFailover && response.ReasoningRecoveryFailed {
		return true
	}
	if !isRetryable(response.StatusCode) {
		return false
	}
	// Account-scoped payment failures must always rotate accounts.
	// Upstream X-Should-Retry:false is only honored for non-account errors (e.g. 5xx history).
	if forcesAccountFailover(response.StatusCode, policy) {
		return true
	}
	return !strings.EqualFold(strings.TrimSpace(response.Header.Get("X-Should-Retry")), "false")
}

// isTerminalRequestForbidden identifies request-level 403 responses that must
// be returned without account or egress side effects. Unknown 403 responses,
// including bare permission-denied, remain on the credential traversal path.
// Request policy classification applies only where the provider declares it so
// browser providers keep their clearance recovery. An auth-scheme rollout error
// (DPoP) is terminal where declared because no account or egress can satisfy it.
func isTerminalRequestForbidden(policy provider.FailurePolicy, failure *UpstreamFailure) bool {
	if failure == nil {
		return false
	}
	return failure.SafetyRejection ||
		(policy.TerminalRequestScopedForbidden && failure.RequestScopedForbidden) ||
		(policy.TerminalAuthSchemeForbidden && failure.RequestScopedForbidden && isDPoPProofRequired(failure.UpstreamCode))
}

// forcesAccountFailover keeps account-scoped billing, permission, and rate-limit
// failures on the account-rotation path so their state can be recorded before another
// account is selected. free-usage 429 and Team RPS 429 both need rotation even when
// upstream sets X-Should-Retry:false.
func forcesAccountFailover(status int, policy provider.FailurePolicy) bool {
	return policy.ForceAccountFailover &&
		(status == http.StatusPaymentRequired || status == http.StatusForbidden || status == http.StatusTooManyRequests)
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if parsed, err := http.ParseTime(value); err == nil && parsed.After(now) {
		return parsed.Sub(now)
	}
	return 0
}

func firstError(values ...error) error {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return errors.New("未知上游错误")
}
