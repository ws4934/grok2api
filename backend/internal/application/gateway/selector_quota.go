package gateway

import (
	"context"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

const defaultFreeQuotaRecoveryPause = 24 * time.Hour

type quotaRecoveryHints struct {
	Billing *account.Billing
}

type quotaConsumptionKey struct {
	provider  account.Provider
	accountID uint64
	mode      string
}

func (s *Selector) MarkFreeQuotaExhausted(ctx context.Context, credential account.Credential, used, limit int64) {
	now := time.Now().UTC()
	nextProbeAt := now.Add(defaultFreeQuotaRecoveryPause)
	_ = s.markFreeQuotaExhaustedAt(ctx, credential, used, limit, now, nextProbeAt)
}

func (s *Selector) markFreeQuotaExhaustedAt(ctx context.Context, credential account.Credential, used, limit int64, now, nextProbeAt time.Time) error {
	if err := s.accounts.SaveQuotaRecovery(ctx, account.QuotaRecovery{
		AccountID: credential.ID, Kind: account.QuotaRecoveryKindFree, Status: account.QuotaRecoveryStatusExhausted,
		ConfirmedUsed: used, ConfirmedLimit: limit, ExhaustedAt: &now,
		NextProbeAt: &nextProbeAt, LastConfirmedAt: &now, UpdatedAt: now,
	}); err != nil {
		return err
	}
	_ = s.sticky.DeleteByAccount(ctx, credential.ID)
	s.invalidateCandidates(credential.Provider)
	return nil
}

func (s *Selector) MarkModelQuotaExhausted(ctx context.Context, credential account.Credential, billing *account.Billing, upstreamModel string, retryAfter time.Duration) {
	upstreamModel = strings.TrimSpace(upstreamModel)
	if upstreamModel == "" {
		s.MarkFreeQuotaExhausted(ctx, credential, 0, 0)
		return
	}
	knownFreeBuild := (account.RoutingCandidate{Credential: credential, Billing: billing}).IsKnownFreeBuild()
	if knownFreeBuild || retryAfter <= 0 {
		retryAfter = defaultFreeQuotaRecoveryPause
	}
	until := time.Now().UTC().Add(retryAfter)
	_ = s.accounts.UpsertModelQuotaBlock(ctx, account.ModelQuotaBlock{
		AccountID: credential.ID, UpstreamModel: upstreamModel, Reason: "model_quota_depleted", CooldownUntil: until, UpdatedAt: time.Now().UTC(),
	})
	// The model block makes affected bindings ineligible and they are rebound on
	// the next request. Preserve unrelated model/session affinity for this account.
	s.invalidateCandidates(credential.Provider)
}

// MarkPaymentQuotaExhausted removes a spending-limited account from routing.
// Paid accounts follow their upstream billing period; Free or unknown accounts
// use the fixed local recovery window.
func (s *Selector) MarkPaymentQuotaExhausted(ctx context.Context, credential account.Credential, hints quotaRecoveryHints) error {
	now := time.Now().UTC()
	if hints.Billing != nil && hints.Billing.IsPaid() {
		if periodEnd, ok := hints.Billing.PeriodEnd(); ok && periodEnd.After(now) {
			if err := s.accounts.SaveQuotaRecovery(ctx, account.QuotaRecovery{
				AccountID: credential.ID, Kind: account.QuotaRecoveryKindPaid, Status: account.QuotaRecoveryStatusExhausted,
				ExhaustedAt: &now, NextProbeAt: &periodEnd, LastConfirmedAt: &now, UpdatedAt: now,
			}); err != nil {
				return err
			}
			_ = s.sticky.DeleteByAccount(ctx, credential.ID)
			s.invalidateCandidates(credential.Provider)
			return nil
		}
	}
	return s.markFreeQuotaExhaustedAt(ctx, credential, 0, 0, now, now.Add(defaultFreeQuotaRecoveryPause))
}

// MarkQuotaStateChanged 在 Billing 探测改变持久化额度状态后更新对应账号的候选快照。
// 未提供账号 ID 时保留全量失效语义，供无法确定变更范围的调用方使用。
func (s *Selector) MarkQuotaStateChanged(provider account.Provider, accountIDs ...uint64) {
	if len(accountIDs) == 0 {
		s.invalidateCandidates(provider)
		return
	}
	for _, accountID := range accountIDs {
		s.clearQuotaConsumptionAccount(provider, accountID)
		s.evictCandidate(provider, accountID)
	}
}

// ConsumeQuota records a small local delta instead of Copy-on-Write cloning
// every cached candidate/base slice. Selection snapshots remain immutable for
// concurrent requests, while the next request observes the consumed amount.
func (s *Selector) ConsumeQuota(provider account.Provider, accountID uint64, mode string, amount int) {
	if accountID == 0 || mode == "" || mode == "weekly" || amount <= 0 {
		return
	}
	s.quotaMu.Lock()
	if s.quotaConsumed == nil {
		s.quotaConsumed = make(map[quotaConsumptionKey]int)
	}
	key := quotaConsumptionKey{provider: provider, accountID: accountID, mode: mode}
	s.quotaConsumed[key] += amount
	s.quotaMu.Unlock()
}

// quotaConsumption reads a provider's local quota deltas in place instead of
// copying the whole map per request. Hold quotaMu for reading while using it.
type quotaConsumption struct {
	values   map[quotaConsumptionKey]int
	provider account.Provider
}

func (s *Selector) quotaConsumptionSnapshot(provider account.Provider) quotaConsumption {
	return quotaConsumption{values: s.quotaConsumed, provider: provider}
}

// quotaSnapshotProvider reports a provider whose last complete quota snapshot
// lists every limited mode: a synced account without a window for the
// requested mode is unlimited (e.g. ChatGPT Plus has no message limit). It
// ranks as known to have quota, not as an unknown fallback; concurrency limits
// and in-flight balancing still apply.
func (s *Selector) quotaSnapshotProvider(provider account.Provider) bool {
	snapshots, ok := s.tierOrders.(interface {
		QuotaSnapshotProvider(account.Provider) bool
	})
	return ok && snapshots.QuotaSnapshotProvider(provider)
}

// quotaSyncedWithoutWindow is the per-account half of quotaSnapshotProvider.
func quotaSyncedWithoutWindow(candidate *account.RoutingCandidate) bool {
	return candidate.QuotaWindow == nil && candidate.Credential.QuotaSyncedAt != nil
}

func quotaWindowExhausted(candidate account.RoutingCandidate, consumed quotaConsumption) bool {
	if candidate.QuotaWindow == nil {
		return false
	}
	remaining := candidate.QuotaWindow.Remaining - consumed.values[quotaConsumptionKey{provider: consumed.provider, accountID: candidate.Credential.ID, mode: candidate.QuotaWindow.Mode}]
	return remaining <= 0
}

func (s *Selector) clearQuotaConsumption(provider account.Provider) {
	s.quotaMu.Lock()
	if provider == "" {
		clear(s.quotaConsumed)
	} else {
		for key := range s.quotaConsumed {
			if key.provider == provider {
				delete(s.quotaConsumed, key)
			}
		}
	}
	s.quotaMu.Unlock()
}

func (s *Selector) clearQuotaConsumptionAccount(provider account.Provider, accountID uint64) {
	s.quotaMu.Lock()
	for key := range s.quotaConsumed {
		if key.provider == provider && key.accountID == accountID {
			delete(s.quotaConsumed, key)
		}
	}
	s.quotaMu.Unlock()
}
