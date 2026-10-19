package account

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"golang.org/x/sync/singleflight"
)

var ErrCredentialRefreshPermanent = errors.New("OAuth refresh token 已永久失效")

const permanentRefreshExpiredReason = "OAuth refresh token 已永久失效且 access token 已过期"

// EnsureCredential 在即将过期时刷新 token，同一账号并发请求只执行一次刷新。
func (s *Service) EnsureCredential(ctx context.Context, value accountdomain.Credential, force bool) (accountdomain.Credential, error) {
	return s.ensureCredential(ctx, value, ensureCredentialOptions{force: force})
}

type ensureCredentialOptions struct {
	force              bool
	bypassCooldown     bool
	respectSchedule    bool
	retryPermanentOnce bool
	// waitFailedRenewal: after a failed renewal, wait for its scheduled retry.
	waitFailedRenewal bool
}

// RenewRejectedCredential renews a credential the upstream just rejected
// (401). After a failed renewal it waits for the scheduled retry instead of
// spending another OAuth call on every rejected request.
func (s *Service) RenewRejectedCredential(ctx context.Context, value accountdomain.Credential) (accountdomain.Credential, error) {
	return s.ensureCredential(ctx, value, ensureCredentialOptions{force: true, waitFailedRenewal: true})
}

func (s *Service) ensureCredential(ctx context.Context, value accountdomain.Credential, options ensureCredentialOptions) (accountdomain.Credential, error) {
	if s.providers == nil || !s.providers.SupportsCredentialRefresh(value.Provider) {
		if options.force {
			return accountdomain.Credential{}, ErrUnsupported
		}
		return value, nil
	}
	now := s.now()
	if credential, err, handled := s.resolvePermanentRefreshFailure(ctx, value, now, options.force, options.retryPermanentOnce); handled {
		return credential, err
	}
	if !options.force && value.ExpiresAt.IsZero() && value.EncryptedAccessToken != "" {
		return value, nil
	}
	if !options.force && value.EncryptedAccessToken != "" && !value.ExpiresAt.IsZero() && now.Add(credentialRefreshAdvance).Before(value.ExpiresAt) {
		return value, nil
	}
	refreshKey := strconv.FormatUint(value.ID, 10)
	if options.respectSchedule {
		refreshKey += ":scheduled"
	}
	if options.retryPermanentOnce {
		refreshKey += ":manual-retry"
	}
	// The shared refresh runs on its own goroutine so a caller can stop waiting
	// while a started token exchange still completes and is persisted.
	shared := s.refreshes.DoChan(refreshKey, func() (any, error) {
		latest, err := s.accounts.Get(ctx, value.ID)
		if err != nil {
			return nil, err
		}
		currentTime := s.now()
		if credential, err, handled := s.resolvePermanentRefreshFailure(ctx, latest, currentTime, options.force, options.retryPermanentOnce); handled {
			if err != nil {
				return nil, err
			}
			return credential, nil
		}
		if options.respectSchedule && latest.RefreshDueAt != nil && latest.RefreshDueAt.After(currentTime) {
			return latest, nil
		}
		if options.force && latest.EncryptedAccessToken != "" && latest.EncryptedAccessToken != value.EncryptedAccessToken {
			return latest, nil
		}
		if !options.force && latest.EncryptedAccessToken != "" && !latest.ExpiresAt.IsZero() && currentTime.Add(credentialRefreshAdvance).Before(latest.ExpiresAt) {
			return latest, nil
		}
		if options.force && !options.bypassCooldown && s.credentialRefreshCoolingDown(latest, currentTime) {
			return latest, nil
		}
		if options.waitFailedRenewal {
			if err := refreshDeferred(latest, currentTime); err != nil {
				return nil, err
			}
		}
		release, err := s.acquireRefreshLock(ctx, latest.ID)
		if err != nil {
			return nil, err
		}
		if release != nil {
			defer release()
			latest, err = s.accounts.Get(ctx, value.ID)
			if err != nil {
				return nil, err
			}
			currentTime = s.now()
			if credential, err, handled := s.resolvePermanentRefreshFailure(ctx, latest, currentTime, options.force, options.retryPermanentOnce); handled {
				if err != nil {
					return nil, err
				}
				return credential, nil
			}
			if options.respectSchedule && latest.RefreshDueAt != nil && latest.RefreshDueAt.After(currentTime) {
				return latest, nil
			}
			if options.force && !options.bypassCooldown && s.credentialRefreshCoolingDown(latest, currentTime) {
				return latest, nil
			}
			if options.waitFailedRenewal {
				if err := refreshDeferred(latest, currentTime); err != nil {
					return nil, err
				}
			}
			if latest.EncryptedAccessToken != "" && latest.EncryptedAccessToken != value.EncryptedAccessToken {
				return latest, nil
			}
			if !options.force && latest.EncryptedAccessToken != "" && !latest.ExpiresAt.IsZero() && currentTime.Add(credentialRefreshAdvance).Before(latest.ExpiresAt) {
				return latest, nil
			}
		}
		adapter, ok := s.providers.CredentialRefresh(latest.Provider)
		if !ok {
			return nil, fmt.Errorf("Provider %s 未注册", latest.Provider)
		}
		// The upstream may consume the old refresh token as soon as the exchange
		// starts. Detach it from the caller so the rotated token is never dropped
		// by a client disconnect or a caller deadline.
		refreshCtx, cancelRefresh := context.WithTimeout(context.WithoutCancel(ctx), credentialRefreshTimeout)
		refreshed, err := adapter.RefreshCredential(refreshCtx, latest)
		cancelRefresh()
		if err != nil {
			persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), credentialRefreshStateTTL)
			s.recordCredentialRefreshFailure(persistCtx, latest, err, !options.retryPermanentOnce, release != nil)
			cancel()
			return nil, err
		}
		riskCredential := latest
		riskCredential.EncryptedAccessToken = refreshed.EncryptedAccessToken
		botFlagSource := latest.BuildBotFlagSource
		if metadata := s.credentialMetadata(riskCredential); metadata.BuildBotFlagInspected {
			botFlagSource = metadata.BuildBotFlagSource
		}
		persistCtx, cancelPersist := context.WithTimeout(context.WithoutCancel(ctx), credentialStateWriteTimeout)
		updated, err := s.accounts.UpdateTokens(persistCtx, latest.ID, refreshed.EncryptedAccessToken, refreshed.EncryptedRefreshToken, refreshed.EncryptedState, refreshed.ExpiresAt, botFlagSource)
		cancelPersist()
		if err != nil {
			s.logger.Error("credential_refresh_token_write_failed",
				"account_id", latest.ID,
				"provider", latest.Provider,
				"refresh_token_rotated", refreshed.RefreshTokenRotated,
				"egress_node_id", latest.EgressNodeID,
				"build_api_fallback_marked", latest.BuildAPIFallback,
				"distributed_lock", release != nil,
				"error", err,
			)
			return nil, err
		}
		s.invalidateBuildBotFlagCache()
		s.markRefreshSuccess(latest.ID, currentTime)
		s.WakeCredentialRefresh()
		return updated, nil
	})
	var result singleflight.Result
	select {
	case <-ctx.Done():
		return accountdomain.Credential{}, ctx.Err()
	case result = <-shared:
	}
	if result.Err != nil {
		return accountdomain.Credential{}, result.Err
	}
	credential, ok := result.Val.(accountdomain.Credential)
	if !ok {
		return accountdomain.Credential{}, fmt.Errorf("账号凭据刷新返回类型无效")
	}
	return credential, nil
}

// acquireRefreshLock 在 Redis 模式下等待其他实例完成刷新，锁租约过期后可自动接管。
func (s *Service) acquireRefreshLock(ctx context.Context, accountID uint64) (func(), error) {
	if s.refreshLock == nil {
		return nil, nil
	}
	key := "credential-refresh:" + strconv.FormatUint(accountID, 10)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		release, acquired, err := s.refreshLock.Acquire(ctx, key, 2*time.Minute)
		if err != nil {
			return nil, err
		}
		if acquired {
			return release, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Service) RefreshToken(ctx context.Context, id uint64) (View, error) {
	value, err := s.accounts.Get(ctx, id)
	if err != nil {
		return View{}, mapRepositoryError(err)
	}
	if _, err := s.ensureCredential(ctx, value, ensureCredentialOptions{force: true, bypassCooldown: true, retryPermanentOnce: true}); err != nil {
		return View{}, err
	}
	return s.Get(ctx, id)
}

func (s *Service) refreshCoolingDown(accountID uint64, now time.Time) bool {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	last := s.lastRefreshAt[accountID]
	return !last.IsZero() && now.Sub(last) < forcedRefreshMinInterval
}

// ErrCredentialRefreshDeferred: a renewal after an upstream 401 waits out the
// backoff of a failed one; each 401 would otherwise spend another OAuth call.
var ErrCredentialRefreshDeferred = errors.New("凭据续期失败，等待下次重试")

// CredentialRefreshDeferredError carries when the next renewal attempt is due.
type CredentialRefreshDeferredError struct{ Until time.Time }

func (e *CredentialRefreshDeferredError) Error() string {
	return fmt.Sprintf("%v（%s）", ErrCredentialRefreshDeferred, e.Until.UTC().Format(time.RFC3339))
}

func (e *CredentialRefreshDeferredError) Unwrap() error { return ErrCredentialRefreshDeferred }

func refreshDeferred(credential accountdomain.Credential, now time.Time) error {
	if credential.RefreshFailureCount == 0 || credential.RefreshDueAt == nil || !credential.RefreshDueAt.After(now) {
		return nil
	}
	return &CredentialRefreshDeferredError{Until: *credential.RefreshDueAt}
}

func (s *Service) credentialRefreshCoolingDown(credential accountdomain.Credential, now time.Time) bool {
	if credential.LastRefreshAt != nil {
		age := now.Sub(*credential.LastRefreshAt)
		if age >= 0 && age < forcedRefreshMinInterval {
			return true
		}
	}
	return s.refreshCoolingDown(credential.ID, now)
}

func (s *Service) markRefreshSuccess(accountID uint64, now time.Time) {
	s.refreshMu.Lock()
	s.lastRefreshAt[accountID] = now
	s.refreshMu.Unlock()
}

func (s *Service) clearRefreshState(accountID uint64) {
	s.refreshMu.Lock()
	delete(s.lastRefreshAt, accountID)
	s.refreshMu.Unlock()
}

func (s *Service) recordCredentialRefreshFailure(ctx context.Context, credential accountdomain.Credential, refreshErr error, preservePermanent, distributedLock bool) {
	if errors.Is(refreshErr, context.Canceled) || errors.Is(refreshErr, context.DeadlineExceeded) && errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	failureCount := credential.RefreshFailureCount + 1
	errorCode := "oauth_transport_error"
	errorMessage := "OAuth request failed"
	errorStatus := 0
	errorResponse := ""
	permanent := false
	retryAfter := time.Duration(0)
	var typed *provider.CredentialRefreshError
	if errors.As(refreshErr, &typed) {
		errorCode = strings.TrimSpace(typed.Code)
		if errorCode == "" {
			errorCode = "oauth_refresh_error"
		}
		errorStatus = typed.Status
		permanent = typed.Permanent
		retryAfter = typed.RetryAfter
		if message := normalizeCredentialRefreshErrorMessage(typed.Message); message != "" {
			errorMessage = message
		}
		errorResponse = normalizeCredentialRefreshErrorResponse(typed.Response)
	} else if errors.Is(refreshErr, context.DeadlineExceeded) {
		errorCode = "oauth_timeout"
		errorMessage = "OAuth request timed out"
	}
	// Defend against adapters or historical rows that classified every OAuth
	// 400/401 as terminal. Only explicit credential-specific terminal codes may
	// stop future refresh attempts.
	if permanent && !provider.IsPermanentCredentialRefreshErrorCode(errorCode) {
		permanent = false
	}
	// 真正的 OAuth 永久失败（invalid_grant 等）只能由成功换 token 清除。
	// 非终态错误不得被旧的 status-only permanent 分类粘住。
	if preservePermanent && credential.RefreshPermanent && !isRecoverableRefreshErrorCode(credential.LastRefreshErrorCode) && !isRecoverableRefreshErrorCode(errorCode) {
		permanent = true
	}
	now := s.now()
	unclassifiedAuthFailure := provider.IsUnclassifiedCredentialAuthRejection(errorStatus, errorCode)
	configurationError := provider.IsCredentialRefreshConfigurationErrorCode(errorCode)
	unclassifiedAuthFailureCount := 0
	if unclassifiedAuthFailure {
		unclassifiedAuthFailureCount = 1
		if credential.LastRefreshErrorStatus == errorStatus && strings.EqualFold(strings.TrimSpace(credential.LastRefreshErrorCode), strings.TrimSpace(errorCode)) {
			unclassifiedAuthFailureCount = credential.RefreshUnclassifiedAuthCount + 1
		}
	}
	retryAt := now.Add(credentialRefreshBackoff(credential.ID, failureCount, retryAfter))
	if configurationError && retryAt.Before(now.Add(credentialConfigurationRetry)) {
		retryAt = now.Add(credentialConfigurationRetry)
	}
	accessTokenAlive := credential.EncryptedAccessToken != "" && !credential.ExpiresAt.IsZero() && credential.ExpiresAt.After(now)
	requiresReauth := unclassifiedAuthFailure && !accessTokenAlive && unclassifiedAuthFailureCount >= credentialUnclassifiedAuthLimit
	if permanent && accessTokenAlive {
		// refresh token 已永久失效时，提前重试没有意义；到 access token 到期时再完成失效收敛。
		retryAt = credential.ExpiresAt
	} else if permanent {
		retryAt = now
	}
	if err := s.accounts.UpdateCredentialRefreshFailure(ctx, credential.ID, repository.CredentialRefreshFailure{
		Count: failureCount, UnclassifiedAuthFailureCount: unclassifiedAuthFailureCount,
		RetryAt: retryAt, Status: errorStatus, Code: errorCode,
		Message: errorMessage, Response: errorResponse, Permanent: permanent,
	}); err != nil {
		s.logger.Warn("credential_refresh_state_write_failed", "account_id", credential.ID, "error", err)
	}
	s.logger.Warn("credential_refresh_failed",
		"account_id", credential.ID,
		"provider", credential.Provider,
		"http_status", errorStatus,
		"error_code", errorCode,
		"error_message", errorMessage,
		"permanent", permanent,
		"failure_count", failureCount,
		"unclassified_auth_failure", unclassifiedAuthFailure,
		"unclassified_auth_failure_count", unclassifiedAuthFailureCount,
		"configuration_error", configurationError,
		"requires_reauth", requiresReauth,
		"retry_at", retryAt,
		"access_token_alive", accessTokenAlive,
		"refresh_token_rotated", false,
		"egress_node_id", credential.EgressNodeID,
		"build_api_fallback_marked", credential.BuildAPIFallback,
		"distributed_lock", distributedLock,
	)
	if permanent && accessTokenAlive {
		s.logger.Warn("credential_refresh_permanent_but_token_alive", "account_id", credential.ID, "error_code", errorCode, "expires_at", credential.ExpiresAt, "retry_at", retryAt)
		s.WakeCredentialRefresh()
		return
	}
	if permanent {
		if err := s.MarkReauthRequired(ctx, credential.ID, "OAuth refresh failed: "+errorCode); err != nil {
			s.logger.Warn("credential_refresh_reauth_mark_failed", "account_id", credential.ID, "error", err)
		}
		return
	}
	if requiresReauth {
		if err := s.MarkReauthRequired(ctx, credential.ID, "OAuth refresh repeatedly rejected without a classifiable error"); err != nil {
			s.logger.Warn("credential_refresh_unclassified_reauth_mark_failed", "account_id", credential.ID, "error", err)
			return
		}
		s.logger.Warn("credential_refresh_unclassified_reauth_required",
			"account_id", credential.ID,
			"http_status", errorStatus,
			"error_code", errorCode,
			"failure_count", unclassifiedAuthFailureCount,
		)
		return
	}
	s.logger.Warn("credential_refresh_deferred", "account_id", credential.ID, "failure_count", failureCount, "retry_at", retryAt, "error_code", errorCode)
	s.WakeCredentialRefresh()
}

func normalizeCredentialRefreshErrorMessage(value string) string {
	value = strings.Map(func(char rune) rune {
		switch char {
		case '\r', '\n', '\t':
			return ' '
		}
		if char < 0x20 || char == 0x7f {
			return -1
		}
		return char
	}, strings.TrimSpace(value))
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > 512 {
		value = string(runes[:511]) + "…"
	}
	return value
}

func normalizeCredentialRefreshErrorResponse(value string) string {
	value = strings.Map(func(char rune) rune {
		if char < 0x20 || char == 0x7f {
			return ' '
		}
		return char
	}, strings.TrimSpace(value))
	runes := []rune(value)
	if len(runes) > 4096 {
		value = string(runes[:4095]) + "…"
	}
	return value
}

// resolvePermanentRefreshFailure 阻止自动链路再次请求已确认失效的 refresh token，
// 并在 access token 到期后收敛账号状态。管理员显式刷新可通过
// retryPermanentOnce 绕过一次；credential_decrypt_failed 等可恢复本地错误不受阻断。
func (s *Service) resolvePermanentRefreshFailure(ctx context.Context, credential accountdomain.Credential, now time.Time, force, retryPermanentOnce bool) (accountdomain.Credential, error, bool) {
	if !credential.RefreshPermanent {
		return accountdomain.Credential{}, nil, false
	}
	if isRecoverableRefreshErrorCode(credential.LastRefreshErrorCode) {
		// 允许 force 或到期调度再次尝试解密/刷新；成功后会 clear permanent 标记。
		return accountdomain.Credential{}, nil, false
	}
	if retryPermanentOnce {
		return accountdomain.Credential{}, nil, false
	}
	accessTokenAlive := credential.EncryptedAccessToken != "" && !credential.ExpiresAt.IsZero() && credential.ExpiresAt.After(now)
	if accessTokenAlive && !force {
		return credential, nil, true
	}
	if !accessTokenAlive {
		if err := s.MarkReauthRequired(ctx, credential.ID, permanentRefreshExpiredReason); err != nil {
			return accountdomain.Credential{}, err, true
		}
	}
	if credential.LastRefreshErrorCode == "" {
		return accountdomain.Credential{}, ErrCredentialRefreshPermanent, true
	}
	return accountdomain.Credential{}, fmt.Errorf("%w: %s", ErrCredentialRefreshPermanent, credential.LastRefreshErrorCode), true
}

// isRecoverableRefreshErrorCode 标识“永久标记可被后续成功刷新清除”的本地/临时错误。
func isRecoverableRefreshErrorCode(code string) bool {
	return !provider.IsPermanentCredentialRefreshErrorCode(code)
}

func credentialRefreshBackoff(accountID uint64, failureCount int, retryAfter time.Duration) time.Duration {
	delays := [...]time.Duration{30 * time.Second, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute}
	index := max(0, min(failureCount-1, len(delays)-1))
	delay := delays[index]
	if retryAfter > delay {
		delay = min(retryAfter, 30*time.Minute)
	}
	return delay + time.Duration((accountID*37)%16)*time.Second
}

// RefreshAllTokens 续期所有声明支持刷新的 Provider 凭据，不可续期账号会被跳过。
func (s *Service) RefreshAllTokens(ctx context.Context) (int, int, int, error) {
	return s.RefreshAllTokensWithProgress(ctx, nil)
}

func (s *Service) RefreshAllTokensWithProgress(ctx context.Context, progress BatchProgressObserver) (int, int, int, error) {
	if s.providers == nil {
		return 0, 0, 0, fmt.Errorf("Provider 注册表未初始化")
	}
	allIDs := make([]uint64, 0)
	ids := make([]uint64, 0)
	for _, providerValue := range s.refreshableProviders() {
		providerIDs, err := s.accounts.ListEnabledCredentialRefreshAccountIDs(ctx, providerValue, false)
		if err != nil {
			return 0, 0, 0, err
		}
		refreshableIDs, err := s.accounts.ListEnabledCredentialRefreshAccountIDs(ctx, providerValue, true)
		if err != nil {
			return 0, 0, 0, err
		}
		allIDs = append(allIDs, providerIDs...)
		ids = append(ids, refreshableIDs...)
	}
	skipped := max(0, len(allIDs)-len(ids))
	succeeded, failed, err := s.refreshTokens(ctx, ids, progress)
	return succeeded, failed, skipped, err
}

func (s *Service) refreshTokens(ctx context.Context, ids []uint64, progress BatchProgressObserver) (int, int, error) {
	return s.runAccountBatch(ctx, "credential_refresh", ids, s.refreshPool, progress, func(workCtx context.Context, id uint64) error {
		value, err := s.accounts.Get(workCtx, id)
		if err == nil {
			_, err = s.ensureCredential(workCtx, value, ensureCredentialOptions{force: true, bypassCooldown: true, retryPermanentOnce: true})
		}
		return err
	})
}

// BatchRefreshTokens 续期指定账号的凭据；失效账号会强制向上游重试一次，
// 停用、Provider 不支持或缺少刷新凭据的账号会被跳过。
func (s *Service) BatchRefreshTokens(ctx context.Context, ids []uint64) (int, int, int, error) {
	values, err := normalizeBatchIDs(ids)
	if err != nil {
		return 0, 0, 0, err
	}
	if s.providers == nil {
		return 0, 0, 0, fmt.Errorf("Provider 注册表未初始化")
	}
	refreshableIDs := make([]uint64, 0, len(values))
	for _, id := range values {
		value, getErr := s.accounts.Get(ctx, id)
		if getErr != nil {
			return 0, 0, 0, getErr
		}
		if !s.providers.SupportsCredentialRefresh(value.Provider) || !value.Enabled || value.EncryptedRefreshToken == "" {
			continue
		}
		refreshableIDs = append(refreshableIDs, id)
	}
	skipped := len(values) - len(refreshableIDs)
	succeeded, failed, err := s.refreshTokens(ctx, refreshableIDs, nil)
	return succeeded, failed, skipped, err
}

// refreshableProviders lists the registered providers whose Definition declares credential refresh.
func (s *Service) refreshableProviders() []accountdomain.Provider {
	if s.providers == nil {
		return nil
	}
	var values []accountdomain.Provider
	for _, providerValue := range s.providers.Providers() {
		if s.providers.SupportsCredentialRefresh(providerValue) {
			values = append(values, providerValue)
		}
	}
	return values
}
