package provider

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// CredentialRefreshError distinguishes permanent OAuth errors requiring reauthorization from temporary errors that can retry with backoff.
type CredentialRefreshError struct {
	Status  int
	Code    string
	Message string
	// Response is a bounded, redacted representation of the upstream OAuth
	// response. It is diagnostic only and must never contain credentials.
	Response   string
	Permanent  bool
	RetryAfter time.Duration
	Cause      error
}

func (e *CredentialRefreshError) Error() string {
	if e == nil {
		return "credential refresh failed"
	}
	if e.Code != "" {
		if e.Message != "" {
			return "credential refresh failed: " + e.Code + ": " + e.Message
		}
		return "credential refresh failed: " + e.Code
	}
	if e.Message != "" {
		return "credential refresh failed: " + e.Message
	}
	if e.Cause != nil {
		return "credential refresh failed: " + e.Cause.Error()
	}
	return "credential refresh failed"
}

func (e *CredentialRefreshError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// IsPermanentCredentialRefreshErrorCode reports credential-specific terminal
// failures. HTTP status alone is intentionally insufficient: OAuth gateways
// also use 400/401 for temporary policy, client, and infrastructure errors.
func IsPermanentCredentialRefreshErrorCode(code string) bool {
	switch normalizeCredentialRefreshErrorCode(code) {
	case "invalid_grant",
		"invalid_refresh_token",
		"refresh_token_invalid",
		"refresh_token_expired",
		"refresh_token_revoked",
		"refresh_token_reused",
		"refresh_token_reuse",
		"token_reused",
		"token_reuse_detected",
		"expired_token",
		"revoked_token",
		"token_revoked",
		"missing_refresh_token",
		"refresh_token_invalidated",
		"unsupported_client_profile":
		return true
	default:
		return false
	}
}

// IsCredentialRefreshConfigurationErrorCode reports OAuth failures caused by
// this gateway's client/request configuration rather than by one account's
// refresh token. These errors should be retried conservatively and surfaced to
// operators, but must not mark an individual account reauthRequired.
func IsCredentialRefreshConfigurationErrorCode(code string) bool {
	switch normalizeCredentialRefreshErrorCode(code) {
	case "invalid_client", "unauthorized_client", "invalid_request", "invalid_scope", "unsupported_grant_type":
		return true
	default:
		return false
	}
}

// IsUnclassifiedCredentialAuthRejection reports a 400/401 response that is
// neither a known terminal refresh-token error, a known client configuration
// error, nor an explicitly retryable OAuth condition. Repeated occurrences can
// eventually require operator reauthorization without claiming the refresh
// token was definitively revoked.
func IsUnclassifiedCredentialAuthRejection(status int, code string) bool {
	if status != http.StatusBadRequest && status != http.StatusUnauthorized {
		return false
	}
	if IsPermanentCredentialRefreshErrorCode(code) || IsCredentialRefreshConfigurationErrorCode(code) {
		return false
	}
	switch normalizeCredentialRefreshErrorCode(code) {
	case "authorization_pending", "slow_down", "temporarily_unavailable", "server_error",
		"rate_limited", "rate_limit_exceeded", "too_many_requests", "oauth_timeout",
		"oauth_transport_error", "oauth_unavailable":
		return false
	default:
		return true
	}
}

func normalizeCredentialRefreshErrorCode(code string) string {
	normalized := strings.ToLower(strings.TrimSpace(code))
	return strings.ReplaceAll(normalized, "-", "_")
}

// CredentialSeed represents an OAuth credential not yet persisted after login or import.
type CredentialSeed struct {
	Provider                account.Provider
	AuthType                account.AuthType
	WebTier                 account.WebTier
	Name                    string
	Email                   string
	UserID                  string
	TeamID                  string
	SourceKey               string
	OIDCClientID            string
	AccessToken             string
	RefreshToken            string
	CloudflareCookies       string
	ExpiresAt               time.Time
	WebNSFWEnabledAt        *time.Time
	WebTermsAcceptedAt      *time.Time
	WebTermsAcceptedVersion int
	WebBirthDateSetAt       *time.Time
	// Plan is the provider's subscription label (for example "plus"); empty when unknown.
	Plan string
	// State holds provider-owned credential material beyond the tokens (for
	// example id_token or a browser profile). It is stored encrypted as JSON.
	State map[string]string
}

// RefreshedCredential represents rotated credentials returned by an OAuth refresh.
type RefreshedCredential struct {
	EncryptedAccessToken  string
	EncryptedRefreshToken string
	// EncryptedState replaces the stored provider state when non-empty.
	EncryptedState string
	ExpiresAt      time.Time
	// RefreshTokenRotated reports that the OAuth response explicitly returned
	// a different refresh token. It is diagnostic metadata only; token values
	// must never be logged.
	RefreshTokenRotated bool
}

type CredentialRefreshAdapter interface {
	Adapter
	RefreshCredential(ctx context.Context, credential account.Credential) (RefreshedCredential, error)
}

// AuthorizationKind distinguishes polled device flows from browser redirects.
type AuthorizationKind string

const (
	AuthorizationDevice   AuthorizationKind = "device"
	AuthorizationRedirect AuthorizationKind = "redirect"
)

// AuthorizationChallenge is what an administrator needs to finish an
// interactive login. State is opaque adapter data (device code, PKCE
// verifier); it is kept server side and never returned to clients.
type AuthorizationChallenge struct {
	Kind                    AuthorizationKind
	State                   string
	VerificationURI         string
	VerificationURIComplete string
	UserCode                string
	AuthorizeURL            string
	Interval                time.Duration
	ExpiresIn               time.Duration
}

// InteractiveAuthAdapter adds accounts through an administrator-driven login.
// Device flows complete with an empty input and return ErrAuthorizationPending
// or ErrSlowDown until approved; redirect flows take the full callback URL.
type InteractiveAuthAdapter interface {
	Adapter
	StartAuthorization(ctx context.Context) (AuthorizationChallenge, error)
	CompleteAuthorization(ctx context.Context, challenge AuthorizationChallenge, input string) (CredentialSeed, error)
}

type CredentialCodecAdapter interface {
	Adapter
	ParseImportedCredentials(data []byte) ([]CredentialSeed, error)
	MarshalCredentials(values []CredentialSeed) ([]byte, error)
}

// CredentialImportPreparer exchanges incomplete imported credentials before
// persistence. Implementations must preserve provider token rotation.
type CredentialImportPreparer interface {
	Adapter
	PrepareImportedCredential(ctx context.Context, seed CredentialSeed) (CredentialSeed, error)
}

// CredentialMetadata contains non-sensitive display data safely derived from a stored credential.
// Raw tokens and complete JWT claims must never be exposed through this structure.
type CredentialMetadata struct {
	// BuildBotFlagInspected is true only when the Build token was successfully
	// decrypted and decoded. False means the risk source is unknown, not clean.
	BuildBotFlagInspected bool
	// BuildBotFlagged is true when BuildBotFlagSource is 1 or 2.
	BuildBotFlagged bool
	// BuildBotFlagSource is the numeric bot_flag_source/bfs claim (1 or 2), or 0 when unset.
	BuildBotFlagSource int
	// CredentialKind names how the credential was issued, e.g. the OAuth client;
	// empty when the provider does not tell.
	CredentialKind string
	// RefreshUnsupported: the stored refresh token cannot be renewed.
	RefreshUnsupported bool
}

type CredentialMetadataAdapter interface {
	Adapter
	CredentialMetadata(credential account.Credential) CredentialMetadata
}

// AccountIdentity contains non-sensitive account identity metadata confirmed by upstream.
// Email is for display only; cross-Provider automatic linking uses stable UserID only.
type AccountIdentity struct {
	Email  string
	UserID string
	TeamID string
	// Plan is the subscription label reported by upstream; empty leaves the stored plan unchanged.
	Plan string
}

type AccountIdentityAdapter interface {
	Adapter
	SyncAccountIdentity(ctx context.Context, credential account.Credential) (AccountIdentity, error)
}

type BuildCredentialConverter interface {
	Adapter
	ConvertToBuild(ctx context.Context, credential account.Credential) (CredentialSeed, error)
}

// WebAccountSettingsAdapter defines upstream profile-setting capabilities for Grok Web SSO accounts.
// This capability belongs only to the Web Provider; Build and Console must not emulate it through generic account logic.
type WebAccountSettingsAdapter interface {
	Adapter
	AcceptTerms(ctx context.Context, credential account.Credential) error
	SetBirthDate(ctx context.Context, credential account.Credential, birthDate time.Time) error
	EnableNSFW(ctx context.Context, credential account.Credential) error
}

func (r *Registry) CredentialRefresh(value account.Provider) (CredentialRefreshAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(CredentialRefreshAdapter)
	return result, ok
}

func (r *Registry) InteractiveAuth(value account.Provider) (InteractiveAuthAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(InteractiveAuthAdapter)
	return result, ok
}

func (r *Registry) CredentialCodec(value account.Provider) (CredentialCodecAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(CredentialCodecAdapter)
	return result, ok
}

// CredentialMetadata returns derived credential metadata safe for admin display.
func (r *Registry) CredentialMetadata(credential account.Credential) CredentialMetadata {
	if r == nil {
		return CredentialMetadata{}
	}
	adapter, ok := r.adapters[credential.Provider]
	if !ok {
		return CredentialMetadata{}
	}
	inspector, ok := adapter.(CredentialMetadataAdapter)
	if !ok {
		return CredentialMetadata{}
	}
	return inspector.CredentialMetadata(credential)
}

func (r *Registry) AccountIdentity(value account.Provider) (AccountIdentityAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(AccountIdentityAdapter)
	return result, ok
}

func (r *Registry) BuildConverter(value account.Provider) (BuildCredentialConverter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(BuildCredentialConverter)
	return result, ok
}

// WebAccountSettings returns the Grok Web-specific account profile settings capability.
func (r *Registry) WebAccountSettings() (WebAccountSettingsAdapter, bool) {
	adapter, ok := r.Get(account.ProviderWeb)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(WebAccountSettingsAdapter)
	return result, ok
}
