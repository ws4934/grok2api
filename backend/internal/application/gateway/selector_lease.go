package gateway

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type accountLease struct {
	Credential          account.Credential
	Billing             *account.Billing
	QuotaProbe          bool
	QuotaProbeKind      account.QuotaRecoveryKind
	QuotaMode           string
	routingCandidate    *account.RoutingCandidate
	selectorObservation *selectorLeaseObservation
	release             func()
}

var errRoutingCredentialStale = errors.New("routing credential is no longer available")

// credentialMaterialFailureSkipLimit 限制一次选号连续跳过的同类瞬态故障。
// 少量失败仍按单号隔离；超过上限时保留原始存储错误，避免大账号池放大系统性故障。
const credentialMaterialFailureSkipLimit = 8

type credentialMaterialFailureTracker struct {
	class string
	count int
}

func (tracker *credentialMaterialFailureTracker) record(class string) (int, bool) {
	if tracker == nil {
		return 1, true
	}
	if tracker.class != class {
		tracker.class = class
		tracker.count = 0
	}
	tracker.count++
	return tracker.count, tracker.count <= credentialMaterialFailureSkipLimit
}

func (tracker *credentialMaterialFailureTracker) reset() {
	if tracker == nil {
		return
	}
	tracker.class = ""
	tracker.count = 0
}

// skipUnusableCredentialMaterial 只把账号缺失或明确的瞬态存储故障转成可跳过的过期 claim。
// 请求取消、永久错误和无法分类的错误保留根因返回，不能伪装成整池无可用账号。
func (s *Selector) skipUnusableCredentialMaterial(ctx context.Context, value account.Credential, loadErr error, tracker *credentialMaterialFailureTracker) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(loadErr, repository.ErrNotFound) {
		tracker.reset()
		s.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: value.Provider, AccountID: value.ID})
		return errRoutingCredentialStale
	}
	failureClass, transient := transientRoutingStoreFailureClass(loadErr)
	if !transient {
		return fmt.Errorf("加载账号 %d 的凭据材料: %w", value.ID, loadErr)
	}
	consecutiveFailures, allowed := tracker.record(failureClass)
	if !allowed {
		return fmt.Errorf("连续加载账号凭据材料失败（%s，%d 次）: %w", failureClass, consecutiveFailures, loadErr)
	}
	if s.logger != nil {
		s.logger.Warn(
			"routing_credential_material_skipped",
			"account_id", value.ID,
			"provider", value.Provider,
			"failure_class", failureClass,
			"consecutive_failures", consecutiveFailures,
			"error", loadErr,
		)
	}
	return errRoutingCredentialStale
}

func (l *accountLease) Release() {
	if l == nil {
		return
	}
	if l.selectorObservation != nil {
		l.selectorObservation.completeRelease()
	}
	if l.release != nil {
		l.release()
		l.release = nil
	}
}

func (l *accountLease) markSelectorUpstreamStarted() {
	if l != nil && l.selectorObservation != nil {
		l.selectorObservation.upstreamStarted.Store(true)
	}
}

func (l *accountLease) completeSelectorObservation(success bool) {
	if l != nil && l.selectorObservation != nil {
		l.selectorObservation.complete(success)
	}
}

func (s *Selector) claimAccountSlotTracked(ctx context.Context, value account.Credential, materialFailures *credentialMaterialFailureTracker) (*accountLease, error) {
	now := time.Now().UTC()
	value = s.applyRoutingHealth(value, now)
	if value.CooldownUntil != nil && now.Before(*value.CooldownUntil) {
		return nil, nil
	}
	limit := value.MaxConcurrent
	if limit <= 0 {
		limit = account.DefaultMaxConcurrent
	}
	release, acquired, err := s.concurrency.Acquire(ctx, accountConcurrencyKey(value.ID), limit)
	if err != nil {
		return nil, fmt.Errorf("获取账号并发租约: %w", err)
	}
	if !acquired {
		return nil, nil
	}
	releaseSlot := func() {
		release()
		s.announceLeaseReturn()
	}
	if s.accounts != nil {
		material, loadErr := s.accounts.GetCredentialMaterial(ctx, value.ID, value.Provider)
		if loadErr != nil {
			releaseSlot()
			return nil, s.skipUnusableCredentialMaterial(ctx, value, loadErr, materialFailures)
		}
		materialFailures.reset()
		hydrated, matched := material.ApplyTo(value)
		if !matched {
			releaseSlot()
			s.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: value.Provider, AccountID: value.ID})
			return nil, errRoutingCredentialStale
		}
		value = hydrated
	}
	s.selectionMu.Lock()
	s.lastSelectedAt[value.ID] = time.Now().UTC()
	s.selectionMu.Unlock()
	return &accountLease{Credential: value, release: func() {
		releaseSlot()
	}}, nil
}

func (s *Selector) acquirePinnedCapacity(ctx context.Context, value account.Credential, materialFailures *credentialMaterialFailureTracker) (*accountLease, error) {
	_, _, _, capacityWait := s.routingConfig()
	deadline := time.Now().Add(capacityWait)
	for {
		lease, err := s.claimAccountSlotTracked(ctx, value, materialFailures)
		if err != nil || lease != nil {
			return lease, err
		}
		if capacityWait <= 0 {
			return nil, &SelectionUnavailableError{Reason: SelectionSaturated, RetryAfter: time.Second}
		}
		retry, err := s.awaitLeaseRetry(ctx, deadline)
		if err != nil {
			return nil, err
		}
		if !retry {
			return nil, &SelectionUnavailableError{Reason: SelectionSaturated, RetryAfter: time.Second}
		}
	}
}

func (s *Selector) leaseReturnNotice() <-chan struct{} {
	s.leaseWakeMu.Lock()
	defer s.leaseWakeMu.Unlock()
	if s.leaseWake == nil {
		s.leaseWake = make(chan struct{})
	}
	return s.leaseWake
}

func (s *Selector) announceLeaseReturn() {
	s.leaseWakeMu.Lock()
	if s.leaseWake != nil {
		close(s.leaseWake)
	}
	s.leaseWake = make(chan struct{})
	s.leaseWakeMu.Unlock()
}

// awaitLeaseRetry 在本实例归还租约时立即重试；短轮询用于感知其他实例释放的共享并发名额。
func (s *Selector) awaitLeaseRetry(ctx context.Context, deadline time.Time) (bool, error) {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false, nil
	}
	notice := s.leaseReturnNotice()
	timer := time.NewTimer(min(remaining, 100*time.Millisecond))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-notice:
		return true, nil
	case <-timer.C:
		return time.Now().Before(deadline), nil
	}
}
