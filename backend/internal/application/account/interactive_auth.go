package account

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

var (
	ErrAuthPending  = errors.New("账号授权尚未完成")
	ErrAuthSlowDown = errors.New("账号授权轮询过快")
	ErrAuthExpired  = errors.New("账号授权已拒绝或过期")
)

// redirectAuthSessionTTL bounds browser-redirect logins; device logins keep the upstream expiry.
const redirectAuthSessionTTL = 10 * time.Minute

// AuthStartResult is the client-visible part of an interactive login; the challenge state stays server side.
type AuthStartResult struct {
	SessionID               string
	Kind                    string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	AuthorizeURL            string
	Interval                time.Duration
	ExpiresAt               time.Time
}

// StartAuthorization starts the provider's interactive login and stores the
// challenge, with its state encrypted, in a session bound to the administrator.
func (s *Service) StartAuthorization(ctx context.Context, providerValue accountdomain.Provider, adminID uint64) (AuthStartResult, error) {
	adapter, ok := s.providers.InteractiveAuth(providerValue)
	if !ok {
		return AuthStartResult{}, fmt.Errorf("%w: Provider %s 不支持交互授权", ErrUnsupported, providerValue)
	}
	challenge, err := adapter.StartAuthorization(ctx)
	if err != nil {
		return AuthStartResult{}, err
	}
	if challenge.Kind != provider.AuthorizationDevice && challenge.Kind != provider.AuthorizationRedirect {
		return AuthStartResult{}, fmt.Errorf("Provider %s 返回了无效授权类型 %q", providerValue, challenge.Kind)
	}
	state, err := s.cipher.Encrypt(challenge.State)
	if err != nil {
		return AuthStartResult{}, err
	}
	sessionID, err := security.NewOpaqueToken(18)
	if err != nil {
		return AuthStartResult{}, err
	}
	ttl := challenge.ExpiresIn
	if ttl <= 0 || (challenge.Kind == provider.AuthorizationRedirect && ttl > redirectAuthSessionTTL) {
		ttl = redirectAuthSessionTTL
	}
	now := s.now()
	session := accountdomain.AuthSession{
		ID: sessionID, Provider: providerValue, AdminID: adminID, Kind: string(challenge.Kind), EncryptedState: state,
		UserCode: challenge.UserCode, VerificationURI: challenge.VerificationURI, VerificationURIComplete: challenge.VerificationURIComplete,
		AuthorizeURL: challenge.AuthorizeURL, Interval: challenge.Interval, NextPollAt: now.Add(challenge.Interval), ExpiresAt: now.Add(ttl),
	}
	if err := s.deviceSessions.Create(ctx, session); err != nil {
		return AuthStartResult{}, err
	}
	return AuthStartResult{
		SessionID: sessionID, Kind: session.Kind, UserCode: session.UserCode, VerificationURI: session.VerificationURI,
		VerificationURIComplete: session.VerificationURIComplete, AuthorizeURL: session.AuthorizeURL,
		Interval: session.Interval, ExpiresAt: session.ExpiresAt,
	}, nil
}

// CompleteAuthorization polls a device login once (empty input) or finishes a
// redirect login with the pasted callback URL, then persists the account.
func (s *Service) CompleteAuthorization(ctx context.Context, providerValue accountdomain.Provider, sessionID string, adminID uint64, input string) (View, error) {
	now := s.now()
	session, err := s.deviceSessions.Get(ctx, sessionID, now)
	if errors.Is(err, repository.ErrNotFound) {
		return View{}, ErrAuthExpired
	}
	if err != nil {
		return View{}, err
	}
	// A session started by another administrator or for another provider is reported as missing.
	if session.Provider != providerValue || session.AdminID != adminID {
		return View{}, ErrAuthExpired
	}
	if session.Kind == string(provider.AuthorizationRedirect) && strings.TrimSpace(input) == "" {
		return View{}, invalidInput("跳转授权需要回调地址")
	}
	if now.Before(session.NextPollAt) {
		return View{}, ErrAuthSlowDown
	}
	adapter, ok := s.providers.InteractiveAuth(providerValue)
	if !ok {
		return View{}, fmt.Errorf("%w: Provider %s 不支持交互授权", ErrUnsupported, providerValue)
	}
	state, err := s.cipher.Decrypt(session.EncryptedState)
	if err != nil {
		return View{}, err
	}
	seed, err := adapter.CompleteAuthorization(ctx, provider.AuthorizationChallenge{
		Kind: provider.AuthorizationKind(session.Kind), State: state, UserCode: session.UserCode,
		VerificationURI: session.VerificationURI, VerificationURIComplete: session.VerificationURIComplete,
		AuthorizeURL: session.AuthorizeURL, Interval: session.Interval, ExpiresIn: session.ExpiresAt.Sub(now),
	}, input)
	session.NextPollAt = now.Add(session.Interval)
	switch {
	case errors.Is(err, provider.ErrAuthorizationPending):
		_ = s.deviceSessions.Update(ctx, session)
		return View{}, ErrAuthPending
	case errors.Is(err, provider.ErrSlowDown):
		session.Interval += 5 * time.Second
		session.NextPollAt = now.Add(session.Interval)
		_ = s.deviceSessions.Update(ctx, session)
		return View{}, ErrAuthSlowDown
	case errors.Is(err, provider.ErrAuthorizationDenied):
		_ = s.deviceSessions.Delete(ctx, sessionID)
		return View{}, ErrAuthExpired
	case err != nil:
		_ = s.deviceSessions.Update(ctx, session)
		return View{}, err
	}
	if seed.Provider == "" {
		seed.Provider = providerValue
	}
	if seed.Provider != providerValue {
		return View{}, fmt.Errorf("Provider %s 授权返回了其他 Provider 的凭据", providerValue)
	}
	// The upstream already consumed the grant; the write must not depend on the client staying connected.
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), credentialStateWriteTimeout)
	value, _, err := s.persistSeed(persistCtx, seed)
	if err == nil {
		s.reconcileProviderLinksBestEffort(persistCtx, value.ID)
		_ = s.deviceSessions.Delete(persistCtx, sessionID)
	}
	cancel()
	if err != nil {
		return View{}, err
	}
	return s.Get(ctx, value.ID)
}
