package provider

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrAuthorizationPending = errors.New("authorization pending")
	ErrSlowDown             = errors.New("authorization polling too fast")
	ErrAuthorizationDenied  = errors.New("authorization denied")
	ErrCredentialLimit      = errors.New("credential count exceeds limit")
	ErrUnauthorized         = errors.New("upstream credential unauthorized")
	ErrBirthDateAlreadySet  = errors.New("upstream birth date is already set")
	// ErrVideoParameterInvalid and ErrVideoOperationUnsupported are wrapped by
	// VideoRequestValidator rejections.
	ErrVideoParameterInvalid     = errors.New("视频请求参数无效")
	ErrVideoOperationUnsupported = errors.New("视频编辑/延长仅支持路由到 Console grok-imagine-video")
)

// HTTPStatusError preserves the upstream status when a streaming or asynchronous Provider cannot return a Response.
type HTTPStatusError interface {
	error
	HTTPStatusCode() int
}

// RetryAfterError preserves a safe upstream retry delay when an adapter cannot
// return a Response, for example when a WebSocket handshake is rejected.
type RetryAfterError interface {
	error
	RetryAfterDuration() time.Duration
}

// RequestScopedError marks an upstream rejection that retrying with another
// account or egress cannot resolve.
type RequestScopedError interface {
	error
	RequestScopedFailure() bool
}

// PublicMessageError exposes a deliberately sanitized message that may cross
// the public API boundary. Provider errors must opt in; arbitrary Error()
// strings can contain upstream response bodies, tokens, cookies, or request
// diagnostics and therefore are never returned to clients by default.
type PublicMessageError interface {
	error
	PublicErrorMessage() string
}

// ErrorHTTPStatus extracts the upstream HTTP status from a Provider error chain.
func ErrorHTTPStatus(err error) (int, bool) {
	var statusError HTTPStatusError
	if !errors.As(err, &statusError) {
		return 0, false
	}
	status := statusError.HTTPStatusCode()
	return status, status > 0
}

// ErrorHTTPStatusOrZero extracts an upstream status or returns 0.
func ErrorHTTPStatusOrZero(err error) int {
	status, ok := ErrorHTTPStatus(err)
	if !ok {
		return 0
	}
	return status
}

// ErrorRetryAfter extracts a positive retry delay from an error chain.
func ErrorRetryAfter(err error) time.Duration {
	var retryError RetryAfterError
	if !errors.As(err, &retryError) {
		return 0
	}
	return max(0, retryError.RetryAfterDuration())
}

// IsRequestScopedError reports whether the Provider has positively classified
// the failure as request-scoped.
func IsRequestScopedError(err error) bool {
	var requestError RequestScopedError
	return errors.As(err, &requestError) && requestError.RequestScopedFailure()
}

// ErrorPublicMessage extracts a message that the Provider has explicitly
// classified as safe for clients.
func ErrorPublicMessage(err error) (string, bool) {
	var publicError PublicMessageError
	if !errors.As(err, &publicError) {
		return "", false
	}
	message := strings.TrimSpace(publicError.PublicErrorMessage())
	return message, message != ""
}

// MediaPostProcessingStage identifies a local processing stage that failed after media generation.
type MediaPostProcessingStage string

const (
	MediaPostProcessingDownload MediaPostProcessingStage = "download"
	MediaPostProcessingStorage  MediaPostProcessingStage = "storage"
)

// MediaPostProcessingError indicates that upstream media was created but download or storage failed.
// These errors must not trigger generation on another account or reduce the generating account's health.
type MediaPostProcessingError struct {
	Stage MediaPostProcessingStage
	Cause error
}

func (e *MediaPostProcessingError) Error() string {
	if e == nil || e.Cause == nil {
		return "media post-processing failed"
	}
	return fmt.Sprintf("media post-processing %s failed: %v", e.Stage, e.Cause)
}

func (e *MediaPostProcessingError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// NewMediaPostProcessingError marks a download or storage error as non-retryable across accounts.
func NewMediaPostProcessingError(stage MediaPostProcessingStage, cause error) error {
	if cause == nil {
		return nil
	}
	return &MediaPostProcessingError{Stage: stage, Cause: cause}
}

// IsMediaPostProcessingError reports whether an error occurred during local processing after media generation.
func IsMediaPostProcessingError(err error) bool {
	var target *MediaPostProcessingError
	return errors.As(err, &target)
}
