package account

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func (s *Service) RefreshBilling(ctx context.Context, id uint64) (accountdomain.Billing, error) {
	result, err, _ := s.billingSyncs.Do(strconv.FormatUint(id, 10), func() (any, error) {
		return s.refreshBilling(ctx, id)
	})
	if err != nil {
		return accountdomain.Billing{}, err
	}
	billing, ok := result.(accountdomain.Billing)
	if !ok {
		return accountdomain.Billing{}, fmt.Errorf("额度同步返回类型无效")
	}
	return billing, nil
}

func (s *Service) refreshBilling(ctx context.Context, id uint64) (accountdomain.Billing, error) {
	value, billing, err := s.fetchAndSaveBilling(ctx, id)
	if err != nil {
		return accountdomain.Billing{}, err
	}
	if err := s.reconcilePaidQuotaRecovery(ctx, value, billing, false); err != nil {
		return accountdomain.Billing{}, err
	}
	return billing, nil
}

func (s *Service) fetchAndSaveBilling(ctx context.Context, id uint64) (accountdomain.Credential, accountdomain.Billing, error) {
	value, err := s.accounts.Get(ctx, id)
	if err != nil {
		return accountdomain.Credential{}, accountdomain.Billing{}, mapRepositoryError(err)
	}
	value, err = s.EnsureCredential(ctx, value, false)
	if err != nil {
		return accountdomain.Credential{}, accountdomain.Billing{}, err
	}
	adapter, ok := s.providers.Billing(value.Provider)
	if !ok {
		return accountdomain.Credential{}, accountdomain.Billing{}, fmt.Errorf("Provider %s 未注册", value.Provider)
	}
	billing, err := adapter.GetBilling(ctx, value)
	if err != nil {
		return accountdomain.Credential{}, accountdomain.Billing{}, err
	}
	billing.AccountID = id
	if err := s.accounts.SaveBilling(ctx, billing); err != nil {
		return accountdomain.Credential{}, accountdomain.Billing{}, err
	}
	return value, billing, nil
}

// ProbePaidQuota 在真实账期到期后执行一次 Billing 探测，不消耗模型额度。
func (s *Service) ProbePaidQuota(ctx context.Context, value accountdomain.Credential) (bool, error) {
	latest, billing, err := s.fetchAndSaveBilling(ctx, value.ID)
	if err != nil {
		now := time.Now().UTC()
		next := now.Add(paidProbeRetryInterval)
		_ = s.accounts.SaveQuotaRecovery(ctx, accountdomain.QuotaRecovery{AccountID: value.ID, Kind: accountdomain.QuotaRecoveryKindPaid, Status: accountdomain.QuotaRecoveryStatusExhausted, NextProbeAt: &next, UpdatedAt: now})
		return false, err
	}
	if err := s.reconcilePaidQuotaRecovery(ctx, latest, billing, true); err != nil {
		return false, err
	}
	return !billing.IsExhausted(latest.MinimumRemaining), nil
}

func (s *Service) reconcilePaidQuotaRecovery(ctx context.Context, credential accountdomain.Credential, billing accountdomain.Billing, afterProbe bool) error {
	if !billing.IsPaid() || !billing.IsExhausted(credential.MinimumRemaining) {
		recovery, err := s.accounts.GetQuotaRecovery(ctx, credential.ID)
		if errors.Is(err, repository.ErrNotFound) || (err == nil && recovery.Kind != accountdomain.QuotaRecoveryKindPaid) {
			return nil
		}
		if err != nil {
			return err
		}
		return s.accounts.ClearQuotaRecovery(ctx, credential.ID)
	}
	periodEnd, ok := billing.PeriodEnd()
	if !ok {
		return nil
	}
	now := time.Now().UTC()
	next := periodEnd
	if !next.After(now) && afterProbe {
		next = now.Add(paidProbeRetryInterval)
	}
	exhaustedAt := now
	return s.accounts.SaveQuotaRecovery(ctx, accountdomain.QuotaRecovery{
		AccountID: credential.ID, Kind: accountdomain.QuotaRecoveryKindPaid, Status: accountdomain.QuotaRecoveryStatusExhausted,
		ExhaustedAt: &exhaustedAt, NextProbeAt: &next, LastConfirmedAt: &now, UpdatedAt: now,
	})
}

// HasBillingSnapshot 判断账号是否已经完成过一次额度同步，不触发任何上游请求。
func (s *Service) HasBillingSnapshot(ctx context.Context, id uint64) (bool, error) {
	_, err := s.accounts.GetBilling(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// SyncAllBilling 尽力刷新全部启用账号，单个账号失败不阻断其他账号。
func (s *Service) SyncAllBilling(ctx context.Context) (int, int, error) {
	return s.SyncAllBillingWithProgress(ctx, nil)
}

func (s *Service) SyncAllBillingWithProgress(ctx context.Context, progress BatchProgressObserver) (int, int, error) {
	if s.providers == nil {
		return 0, 0, fmt.Errorf("Provider 注册表未初始化")
	}
	ids := make([]uint64, 0)
	for _, providerValue := range s.providers.Providers() {
		quotaKind, ok := s.providers.QuotaKind(providerValue)
		if !ok || quotaKind != provider.QuotaBilling {
			continue
		}
		providerIDs, err := s.accounts.ListEnabledAccountIDs(ctx, providerValue, false)
		if err != nil {
			return 0, 0, err
		}
		ids = append(ids, providerIDs...)
	}
	return s.refreshBillings(ctx, ids, progress)
}

// BatchRefreshBilling 使用有限并发刷新选中账号，避免大量账号同步时串行阻塞或无界创建 goroutine。
func (s *Service) BatchRefreshBilling(ctx context.Context, ids []uint64) (int, int, error) {
	values, err := normalizeBatchIDs(ids)
	if err != nil {
		return 0, 0, err
	}
	return s.refreshBillings(ctx, values, nil)
}

func (s *Service) refreshBillings(ctx context.Context, ids []uint64, progress BatchProgressObserver) (int, int, error) {
	return s.runAccountBatch(ctx, "billing_sync", ids, s.syncPool, progress, func(workCtx context.Context, id uint64) error {
		_, err := s.RefreshBilling(workCtx, id)
		return err
	})
}
