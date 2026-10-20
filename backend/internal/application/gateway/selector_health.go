package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

const successPersistInterval = 30 * time.Second

// Health overrides bridge precise request-path mutations until the immutable
// provider snapshot naturally refreshes. Keep them through the snapshot's
// normal and stale lifetimes so a transient database error cannot resurrect a
// cooled account from an older snapshot.
const routingHealthOverrideTTL = candidateCacheTTL + candidateCacheStaleTTL

const modelAccessDeniedCooldown = 5 * time.Minute

// softNetworkCooldown 网络/超时/5xx 仅短暂隔离本号，避免指数冷却掏空热池。
const softNetworkCooldown = 5 * time.Second

type routingHealthOverride struct {
	provider      account.Provider
	failureCount  int
	cooldownUntil *time.Time
	lastError     string
	updatedAt     time.Time
	revision      uint64
	expiresAt     time.Time
}

func (s *Selector) MarkSuccess(ctx context.Context, credential account.Credential) {
	s.markSuccess(ctx, credential, true)
}

func isMissingThinkingStrike(lastError string) bool {
	return lastError == lastErrorMissingThinking || lastError == lastErrorMissingThinkingDisabled
}

type missingThinkingPenaltyResult string

const (
	missingThinkingPenaltyUnchanged missingThinkingPenaltyResult = "unchanged"
	missingThinkingPenaltyCooled    missingThinkingPenaltyResult = "cooled"
	missingThinkingPenaltyDisabled  missingThinkingPenaltyResult = "disabled"
)

func (s *Selector) markSuccess(ctx context.Context, credential account.Credential, quotaProbe bool) {
	now := time.Now().UTC()
	keepThinkingStrike := isMissingThinkingStrike(credential.LastError)
	healthChanged := credential.FailureCount > 0 || credential.CooldownUntil != nil || credential.LastError != ""
	touchLastUsed := healthChanged
	s.successMu.Lock()
	if last := s.lastSuccessAt[credential.ID]; last.IsZero() || now.Sub(last) >= successPersistInterval {
		touchLastUsed = true
	}
	if touchLastUsed {
		s.lastSuccessAt[credential.ID] = now
	}
	s.successMu.Unlock()
	if healthChanged {
		lastError := ""
		if keepThinkingStrike {
			lastError = lastErrorMissingThinking
		}
		if err := s.accounts.UpdateHealth(ctx, credential.ID, credential.Provider, 0, nil, lastError, true); err == nil {
			s.ApplyInvalidation(repository.InvalidationEvent{
				Kind: repository.InvalidationAccountHealthChanged, Provider: credential.Provider, AccountID: credential.ID,
				HealthMarker: account.NormalizeHealthMarker(lastError),
			})
		}
	} else if touchLastUsed {
		_ = s.accounts.TouchLastUsed(ctx, credential.ID, now)
	}
	if quotaProbe {
		_ = s.accounts.ClearQuotaRecovery(ctx, credential.ID)
	}
}

// MarkModelAccessDenied isolates a permission failure to the rejected model.
// Build OAuth accounts may still have valid video access when a chat endpoint
// returns 403, so a model denial must not invalidate the whole credential.
func (s *Selector) MarkModelAccessDenied(ctx context.Context, credential account.Credential, upstreamModel string, retryAfter time.Duration) error {
	upstreamModel = strings.TrimSpace(upstreamModel)
	if upstreamModel == "" {
		return nil
	}
	if retryAfter <= 0 {
		retryAfter = modelAccessDeniedCooldown
	}
	now := time.Now().UTC()
	if err := s.accounts.UpsertModelQuotaBlock(ctx, account.ModelQuotaBlock{
		AccountID: credential.ID, UpstreamModel: upstreamModel, Reason: "model_access_denied",
		CooldownUntil: now.Add(retryAfter), UpdatedAt: now,
	}); err != nil {
		return err
	}
	return nil
}

// markMissingThinking disables immediately when configured; otherwise it
// cools the first no-thinking hit and disables a later hit after the cooldown.
func (s *Selector) markMissingThinking(ctx context.Context, credential account.Credential, cooldown time.Duration, autoDisable bool) (missingThinkingPenaltyResult, error) {
	if cooldown <= 0 {
		cooldown = defaultMissingThinkingCooldown
	}
	now := time.Now().UTC()
	inCooldown := credential.CooldownUntil != nil && now.Before(*credential.CooldownUntil)
	if autoDisable || (isMissingThinkingStrike(credential.LastError) && !inCooldown) {
		if err := s.disableAccount(ctx, credential); err != nil {
			return missingThinkingPenaltyUnchanged, err
		}
		healthErr := s.accounts.UpdateHealth(ctx, credential.ID, credential.Provider, credential.FailureCount, nil, lastErrorMissingThinkingDisabled, false)
		return missingThinkingPenaltyDisabled, healthErr
	}
	if inCooldown {
		return missingThinkingPenaltyUnchanged, nil
	}
	until := now.Add(cooldown)
	if err := s.accounts.UpdateHealth(ctx, credential.ID, credential.Provider, credential.FailureCount, &until, lastErrorMissingThinking, false); err != nil {
		return missingThinkingPenaltyUnchanged, err
	}
	s.ApplyInvalidation(repository.InvalidationEvent{
		Kind: repository.InvalidationAccountHealthChanged, Provider: credential.Provider, AccountID: credential.ID,
		FailureCount: credential.FailureCount, CooldownUntil: &until, HealthMarker: account.LastErrorMissingThinking,
	})
	return missingThinkingPenaltyCooled, nil
}

func (s *Selector) disableAccount(ctx context.Context, credential account.Credential) error {
	disabled := false
	if _, err := s.accounts.UpdateMany(ctx, credential.Provider, []uint64{credential.ID}, repository.AccountUpdates{Enabled: &disabled}); err != nil {
		return err
	}
	s.ApplyInvalidation(repository.InvalidationEvent{
		Kind: repository.InvalidationAccountStateChanged, Provider: credential.Provider, AccountID: credential.ID,
	})
	if s.sticky != nil {
		_ = s.sticky.DeleteByAccount(ctx, credential.ID)
	}
	return nil
}

// MarkRefreshDeferred rests an account whose credential renewal is waiting
// out a failed attempt, so requests stop landing on a token the upstream rejects.
func (s *Selector) MarkRefreshDeferred(ctx context.Context, credential account.Credential, err error) {
	var deferred *accountapp.CredentialRefreshDeferredError
	if errors.As(err, &deferred) {
		s.MarkFailure(ctx, credential, http.StatusUnauthorized, time.Until(deferred.Until))
	}
}

func (s *Selector) MarkFailure(ctx context.Context, credential account.Credential, status int, retryAfter time.Duration) {
	retryAfter = s.boundUpstreamRetryAfter(retryAfter)
	_ = s.markFailure(ctx, credential, credential.FailureCount, credential.FailureCount+1, status, retryAfter, status == 0)
}

// MarkFailureAfterSuccess records a stream failure from a fresh health baseline.
// The upstream already returned a successful response header, so failures that
// preceded this request must not be carried into the new cooldown calculation.
func (s *Selector) MarkFailureAfterSuccess(ctx context.Context, credential account.Credential, status int, retryAfter time.Duration) error {
	return s.markFailure(ctx, credential, 0, 1, status, retryAfter, status == 0)
}

// markSoftFailure preserves the real upstream status for diagnostics while
// applying the bounded, non-accumulating health penalty used for transient
// network and provider-wide failures.
func (s *Selector) markSoftFailure(ctx context.Context, credential account.Credential, status int, retryAfter time.Duration) error {
	retryAfter = s.boundUpstreamRetryAfter(retryAfter)
	return s.markFailure(ctx, credential, credential.FailureCount, credential.FailureCount+1, status, retryAfter, true)
}

func (s *Selector) boundUpstreamRetryAfter(retryAfter time.Duration) time.Duration {
	if retryAfter <= 0 {
		return retryAfter
	}
	_, _, cooldownMax, _ := s.routingConfig()
	if cooldownMax > 0 && retryAfter > cooldownMax {
		return cooldownMax
	}
	return retryAfter
}

func (s *Selector) markFailure(ctx context.Context, credential account.Credential, baselineFailureCount, nextFailureCount, status int, retryAfter time.Duration, soft bool) error {
	_, cooldownBase, cooldownMax, _ := s.routingConfig()
	// Soft failures only isolate this account briefly and never accumulate the
	// durable failure count. Hard account-scoped 4xx responses retain the
	// exponential policy below.
	effectiveFailureCount := nextFailureCount
	cooldown := cooldownBase
	if soft {
		effectiveFailureCount = baselineFailureCount
		cooldown = softNetworkCooldown
		if retryAfter > cooldown {
			cooldown = retryAfter
		}
	} else {
		for i := 1; i < effectiveFailureCount && cooldown < cooldownMax; i++ {
			cooldown *= 2
		}
		if cooldown > cooldownMax {
			cooldown = cooldownMax
		}
		if retryAfter > cooldown {
			cooldown = retryAfter
		}
	}
	until := time.Now().UTC().Add(cooldown)
	lastError := fmt.Sprintf("upstream status %d", status)
	healthErr := s.accounts.UpdateHealth(ctx, credential.ID, credential.Provider, effectiveFailureCount, &until, lastError, false)
	if healthErr == nil {
		s.ApplyInvalidation(repository.InvalidationEvent{
			Kind: repository.InvalidationAccountHealthChanged, Provider: credential.Provider, AccountID: credential.ID,
			FailureCount: effectiveFailureCount, CooldownUntil: &until,
		})
	}
	if status == 401 || status == 402 || status == 403 || status == 429 {
		_ = s.sticky.DeleteByAccount(ctx, credential.ID)
	}
	return healthErr
}

func (s *Selector) applyHealthInvalidation(event repository.InvalidationEvent) {
	updatedAt := event.PublishedAt.UTC()
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	expiresAt := updatedAt.Add(routingHealthOverrideTTL)
	if !time.Now().UTC().Before(expiresAt) {
		return
	}
	var cooldownUntil *time.Time
	if event.CooldownUntil != nil {
		value := event.CooldownUntil.UTC()
		cooldownUntil = &value
	}
	overrideLastError := account.NormalizeHealthMarker(event.HealthMarker)
	if overrideLastError == "" && (event.FailureCount > 0 || cooldownUntil != nil) {
		overrideLastError = "upstream failure"
	}
	value := routingHealthOverride{
		provider: event.Provider, failureCount: max(0, event.FailureCount), cooldownUntil: cooldownUntil,
		lastError: overrideLastError, updatedAt: updatedAt, revision: event.Revision, expiresAt: expiresAt,
	}
	s.healthMu.Lock()
	current, exists := s.healthOverrides[event.AccountID]
	if exists {
		if current.revision > 0 && value.revision > 0 && value.revision < current.revision {
			s.healthMu.Unlock()
			return
		}
		if (current.revision == 0 || value.revision == 0) && value.updatedAt.Before(current.updatedAt) {
			s.healthMu.Unlock()
			return
		}
	}
	s.healthOverrides[event.AccountID] = value
	s.healthMu.Unlock()
}

func (s *Selector) applyRoutingHealth(value account.Credential, now time.Time) account.Credential {
	s.healthMu.RLock()
	override, exists := s.healthOverrides[value.ID]
	s.healthMu.RUnlock()
	if !exists || override.provider != value.Provider {
		return value
	}
	if !now.Before(override.expiresAt) {
		s.healthMu.Lock()
		if current, ok := s.healthOverrides[value.ID]; ok && !now.Before(current.expiresAt) {
			delete(s.healthOverrides, value.ID)
		}
		s.healthMu.Unlock()
		return value
	}
	value.FailureCount = override.failureCount
	value.CooldownUntil = override.cooldownUntil
	value.LastError = override.lastError
	return value
}

func (s *Selector) routingHealthSnapshot(provider account.Provider, now time.Time) map[uint64]routingHealthOverride {
	var result map[uint64]routingHealthOverride
	expired := make([]uint64, 0)
	s.healthMu.RLock()
	for accountID, value := range s.healthOverrides {
		if !now.Before(value.expiresAt) {
			expired = append(expired, accountID)
			continue
		}
		if value.provider == provider {
			if result == nil {
				result = make(map[uint64]routingHealthOverride)
			}
			result[accountID] = value
		}
	}
	s.healthMu.RUnlock()
	if len(expired) > 0 {
		s.healthMu.Lock()
		for _, accountID := range expired {
			if value, ok := s.healthOverrides[accountID]; ok && !now.Before(value.expiresAt) {
				delete(s.healthOverrides, accountID)
			}
		}
		s.healthMu.Unlock()
	}
	return result
}

// healthCooldownUntil is applyHealthSnapshot's CooldownUntil without copying the credential.
func healthCooldownUntil(value *account.Credential, overrides map[uint64]routingHealthOverride) *time.Time {
	if override, ok := overrides[value.ID]; ok && override.provider == value.Provider {
		return override.cooldownUntil
	}
	return value.CooldownUntil
}

func applyHealthSnapshot(value account.Credential, overrides map[uint64]routingHealthOverride) account.Credential {
	if override, ok := overrides[value.ID]; ok && override.provider == value.Provider {
		value.FailureCount = override.failureCount
		value.CooldownUntil = override.cooldownUntil
		value.LastError = override.lastError
	}
	return value
}

func (s *Selector) clearHealthOverrides(provider account.Provider, accountID uint64) {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	if accountID != 0 {
		delete(s.healthOverrides, accountID)
		return
	}
	if provider == "" {
		clear(s.healthOverrides)
		return
	}
	for id, value := range s.healthOverrides {
		if value.provider == provider {
			delete(s.healthOverrides, id)
		}
	}
}
