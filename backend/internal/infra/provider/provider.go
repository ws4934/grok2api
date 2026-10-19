package provider

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// ResponseResourceRequest describes a common upstream request to a Responses resource endpoint.
type ResponseResourceRequest struct {
	Credential account.Credential
	// ForcedEgressNodeID is set only by administrator quality probes. It lets a
	// healthy credential test a quarantined node without changing its binding.
	ForcedEgressNodeID uint64
	// Billing is used only to determine XAI eligibility in Build auto mode; nil means the account tier is unknown.
	Billing        *account.Billing
	Method         string
	Path           string
	Body           []byte
	Model          string
	PromptCacheKey string
	// ReasoningReplayKey comes only from explicit client session identity; soft cache identity must not replay ciphertext.
	ReasoningReplayKey string
	// AllowClientToolCacheRoute allows the Build native cache route to supplement existing client tools.
	// This is a protocol compatibility signal, not a client authentication result.
	AllowClientToolCacheRoute bool
	// GrokTurnIndex is the explicit Grok Shell client turn; it is validated before Build egress and never fabricated by the server.
	GrokTurnIndex string
	// TransientRetry is the one-based turn resubmission attempt. Zero marks the
	// initial submission and is omitted from the upstream request.
	TransientRetry int
	IdempotencyID  string
	Streaming      bool
	NormalizeBody  bool
	Operation      string
	// NormalizedMetadata receives non-sensitive metadata from the exact payload
	// normalization used for the physical upstream request. The caller owns the
	// value; adapters update it synchronously before network I/O.
	NormalizedMetadata *NormalizedRequestMetadata
}

// NormalizedRequestMetadata contains safe request attributes that may be kept
// in audit records. It must never contain request content or credentials.
type NormalizedRequestMetadata struct {
	ReasoningEffort string
}

// Response represents an upstream response that has not yet been written downstream.
type Response struct {
	StatusCode  int
	Status      string
	Header      http.Header
	Body        io.ReadCloser
	QuotaUnits  int
	UpstreamURL string
	Diagnostic  *DiagnosticResponse
	// ReasoningRecoveryFailed is an internal retry hint emitted only after the Build
	// adapter exhausts same-account recovery for an opaque reasoning 400. Gateway
	// policy must not infer this state from an upstream-controlled response header.
	ReasoningRecoveryFailed bool
	// RecoveredPrimaryFailure records a primary-plane failure hidden by a successful Provider fallback.
	RecoveredPrimaryFailure *DiagnosticResponse
	// RecoveredAttempts 保存被 adapter 内部恢复流程隐藏、但仍需进入请求审计的上游调用。
	RecoveredAttempts []RecoveredAttempt
	RateLimit         *RateLimitMetadata
	// ModelCatalogChanged indicates that the model catalog ETag in an inference response differs from
	// the ETag from the account's most recent successful /models sync.
	ModelCatalogChanged bool
}

const (
	RateLimitScopeRPS = "rps"
	RateLimitScopeRPM = "rpm"
)

// RateLimitMetadata contains transient rate-limit metadata that is safe to propagate from upstream.
type RateLimitMetadata struct {
	Scope      string
	TeamID     string
	Model      string
	Actual     int
	Limit      int
	RetryAfter time.Duration
}

const MaxDiagnosticBodyBytes = 64 << 10

// DiagnosticResponse retains a size-limited failure response before Provider conversion.
type DiagnosticResponse struct {
	StatusCode    int
	Status        string
	Header        http.Header
	Body          []byte
	BodyTruncated bool
}

// UpstreamDiagnosticError is implemented by provider errors caused by an
// upstream HTTP response, so request diagnostics can show that response.
// The URL must not carry credentials or signed query parameters.
type UpstreamDiagnosticError interface {
	UpstreamDiagnostic() (upstreamURL string, response DiagnosticResponse)
}

// RecoveredAttempt 表示一次被后续恢复结果替代的真实上游调用。
type RecoveredAttempt struct {
	Stage       string
	Result      string
	UpstreamURL string
	StartedAt   time.Time
	DurationMS  int64
	Diagnostic  DiagnosticResponse
	Failure     error
}

// ReadDiagnosticBody reads up to the diagnostic body limit and reports whether upstream content was truncated.
func ReadDiagnosticBody(body io.Reader) ([]byte, bool, error) {
	if body == nil {
		return nil, false, nil
	}
	data, err := io.ReadAll(io.LimitReader(body, MaxDiagnosticBodyBytes+1))
	if len(data) <= MaxDiagnosticBodyBytes {
		return data, false, err
	}
	return data[:MaxDiagnosticBodyBytes], true, err
}

// Adapter defines only Provider identity; concrete capabilities are registered through small interfaces as needed.
type Adapter interface {
	Provider() account.Provider
}

type ResponseAdapter interface {
	Adapter
	ForwardResponse(ctx context.Context, request ResponseResourceRequest) (*Response, error)
}

type ModelCatalogAdapter interface {
	Adapter
	ListModels(ctx context.Context, credential account.Credential) ([]string, error)
}

// AccountModelCapabilityNormalizer is optional and normalizes account model capabilities from Billing and credential entitlement.
// Without it, model sync writes the upstream catalog unchanged; nil billing means Unknown with no snapshot.
// credential is used for Build Super entitlement; default Providers may ignore it.
type AccountModelCapabilityNormalizer interface {
	Adapter
	NormalizeAccountModelCapabilities(models []string, billing *account.Billing, credential account.Credential) []string
}

type BillingAdapter interface {
	Adapter
	GetBilling(ctx context.Context, credential account.Credential) (account.Billing, error)
}

// ModelAlias resolves a hidden compatibility model name to one public route and can fix reasoning effort.
type ModelAlias struct {
	Alias           string
	PublicModel     string
	Provider        account.Provider
	UpstreamModel   string
	ReasoningEffort string
}

type ModelAliasAdapter interface {
	Adapter
	ModelAliases() []ModelAlias
}
