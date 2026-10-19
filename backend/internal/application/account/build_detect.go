package account

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/pkg/batch"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// BuildDetectOutcome 描述单次 Grok Build 可用性探测结果。
type BuildDetectOutcome string

const (
	// BuildDetectOutcomeOK 表示探测成功，账号可用。
	BuildDetectOutcomeOK BuildDetectOutcome = "ok"
	// BuildDetectOutcomeInvalid 表示已确认失效并标 reauthRequired。
	BuildDetectOutcomeInvalid BuildDetectOutcome = "invalid"
	// BuildDetectOutcomeFailed 表示探测失败但未判定为永久失效（网络/5xx/临时额度等）。
	BuildDetectOutcomeFailed BuildDetectOutcome = "failed"
)

// BuildDetectItemResult 是单账号探测的结构化结果，供 SSE 增量推送。
type BuildDetectItemResult struct {
	AccountID  uint64
	Name       string
	Email      string
	Outcome    BuildDetectOutcome
	Reason     string
	HTTPStatus int
}

// BuildDetectItemObserver 在单个账号探测完成后推送明细；返回错误会取消批次。
type BuildDetectItemObserver func(item BuildDetectItemResult) error

// DetectBuildAccountsWithProgress 对指定或全部 Grok Build 账号发起探测请求；all 与 ids 必须且只能提供一个。
// 该方法同时上报批量进度与单账号明细。
// itemObserver 在每个账号完成后串行调用：选中检测会推送全部结果，全量检测仅推送已确认失效账号。
func (s *Service) DetectBuildAccountsWithProgress(ctx context.Context, ids []uint64, all bool, progress BatchProgressObserver, itemObserver BuildDetectItemObserver) (int, int, error) {
	if all == (len(ids) > 0) {
		return 0, 0, invalidInput("必须明确选择全部账号或提供非空账号 ID")
	}
	if s.providers == nil {
		return 0, 0, fmt.Errorf("Provider 注册表未初始化")
	}
	selectedMode := !all
	var err error
	if all {
		ids, err = s.accounts.ListEnabledAccountIDs(ctx, accountdomain.ProviderBuild, false)
		if err != nil {
			return 0, 0, err
		}
	} else {
		ids, err = normalizeBatchIDs(ids)
		if err != nil {
			return 0, 0, err
		}
	}
	if len(ids) == 0 {
		return 0, 0, nil
	}
	pool := s.detectPool
	if pool == nil {
		pool = s.syncPool
	}
	if progress != nil {
		if err := progress(0, len(ids)); err != nil {
			return 0, 0, err
		}
	}
	var observerMu sync.Mutex
	var progressErr error
	completed := 0
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	summary, err := batch.ForEachObserved(runCtx, ids, batch.Options{Workers: pool.Limit(), Pool: pool}, func(workCtx context.Context, id uint64) (BuildDetectItemResult, error) {
		item := s.detectBuildAccount(workCtx, id)
		if itemObserver != nil && (selectedMode || item.Outcome == BuildDetectOutcomeInvalid) {
			notifyErr := func() error {
				observerMu.Lock()
				defer observerMu.Unlock()
				return itemObserver(item)
			}()
			if notifyErr != nil {
				return item, notifyErr
			}
		}
		if item.Outcome == BuildDetectOutcomeOK {
			return item, nil
		}
		if item.Reason != "" {
			return item, fmt.Errorf("%s", item.Reason)
		}
		return item, fmt.Errorf("账号检测失败")
	}, func(index int, result batch.Result[BuildDetectItemResult]) {
		var panicErr *batch.PanicError
		if errors.As(result.Err, &panicErr) {
			s.logger.Error("account_bulk_task_panicked", "operation", "build_detect", "account_id", ids[index], "error", panicErr, "stack", string(panicErr.Stack))
		}
		observerMu.Lock()
		defer observerMu.Unlock()
		completed++
		if progress != nil {
			if notifyErr := progress(completed, len(ids)); notifyErr != nil && progressErr == nil {
				progressErr = notifyErr
				cancel()
			}
		}
	})
	s.logBatchSummary("build_detect", pool, summary, err)
	return summary.Succeeded, summary.Failed, errors.Join(err, progressErr)
}

// detectBuildAccount 使用现有 Build Responses 链路发送固定探测请求。
// 失效判定复用 provider.ClassifyCredentialRejection：凭据拒绝标 reauthRequired，
// spending-limit 写额度恢复状态，PermanentAccountDenial 仅阻断固定探测模型。
func (s *Service) detectBuildAccount(ctx context.Context, id uint64) BuildDetectItemResult {
	item := BuildDetectItemResult{AccountID: id, Outcome: BuildDetectOutcomeFailed}
	value, err := s.accounts.Get(ctx, id)
	if err != nil {
		item.Reason = mapRepositoryError(err).Error()
		return item
	}
	item.Name = value.Name
	item.Email = value.Email
	if value.Provider != accountdomain.ProviderBuild {
		item.Reason = "仅 Grok Build 账号支持可用性检测"
		return item
	}
	value, err = s.EnsureCredential(ctx, value, false)
	if err != nil {
		return s.finishBuildDetectCredentialError(ctx, value, err)
	}
	billing, err := s.loadDetectBilling(ctx, id)
	if err != nil {
		item.Reason = err.Error()
		return item
	}
	response, err := s.forwardBuildDetect(ctx, value, billing)
	if err != nil {
		return s.finishBuildDetectCredentialError(ctx, value, err)
	}
	if response.StatusCode == http.StatusUnauthorized {
		_ = response.Body.Close()
		return s.handleBuildDetectUnauthorized(ctx, value, billing)
	}
	return s.finishBuildDetectResponse(ctx, response, value, billing)
}

// handleBuildDetectUnauthorized 复用网关对 Build OAuth 401 的恢复与失效收敛路径。
func (s *Service) handleBuildDetectUnauthorized(ctx context.Context, value accountdomain.Credential, billing *accountdomain.Billing) BuildDetectItemResult {
	item := BuildDetectItemResult{AccountID: value.ID, Name: value.Name, Email: value.Email, Outcome: BuildDetectOutcomeFailed, HTTPStatus: http.StatusUnauthorized}
	if value.RefreshPermanent {
		reason := fmt.Sprintf("%s OAuth access token rejected after permanent refresh failure", value.Provider)
		if markErr := s.markBuildDetectReauth(ctx, value.ID, reason); markErr != nil {
			item.Reason = markErr.Error()
			return item
		}
		item.Outcome = BuildDetectOutcomeInvalid
		item.Reason = reason
		return item
	}
	refreshed, refreshErr := s.EnsureCredential(ctx, value, true)
	if refreshErr != nil {
		if errors.Is(refreshErr, ErrCredentialRefreshPermanent) {
			reason := fmt.Sprintf("%s OAuth access token rejected after permanent refresh failure", value.Provider)
			if markErr := s.markBuildDetectReauth(ctx, value.ID, reason); markErr != nil {
				item.Reason = errors.Join(refreshErr, markErr).Error()
				return item
			}
			item.Outcome = BuildDetectOutcomeInvalid
			item.Reason = reason
			return item
		}
		return s.finishBuildDetectCredentialError(ctx, value, refreshErr)
	}
	response, err := s.forwardBuildDetect(ctx, refreshed, billing)
	if err != nil {
		return s.finishBuildDetectCredentialError(ctx, refreshed, err)
	}
	if response.StatusCode == http.StatusUnauthorized {
		drainDetectBody(response.Body)
		_ = response.Body.Close()
		reason := "Grok Build OAuth credential rejected after refresh"
		if markErr := s.markBuildDetectReauth(ctx, refreshed.ID, reason); markErr != nil {
			item.Reason = markErr.Error()
			return item
		}
		item.AccountID = refreshed.ID
		item.Name = refreshed.Name
		item.Email = refreshed.Email
		item.Outcome = BuildDetectOutcomeInvalid
		item.Reason = reason
		return item
	}
	return s.finishBuildDetectResponse(ctx, response, refreshed, billing)
}

func (s *Service) finishBuildDetectCredentialError(ctx context.Context, value accountdomain.Credential, err error) BuildDetectItemResult {
	item := BuildDetectItemResult{
		AccountID: value.ID,
		Name:      value.Name,
		Email:     value.Email,
		Outcome:   BuildDetectOutcomeFailed,
		Reason:    err.Error(),
	}
	var refreshErr *provider.CredentialRefreshError
	if errors.Is(err, ErrCredentialRefreshPermanent) || errors.As(err, &refreshErr) && refreshErr.Permanent {
		reason := fmt.Sprintf("%s OAuth refresh credential permanently rejected", value.Provider)
		if markErr := s.markBuildDetectReauth(ctx, value.ID, reason); markErr != nil {
			item.Reason = errors.Join(err, markErr).Error()
			return item
		}
		item.Outcome = BuildDetectOutcomeInvalid
		item.Reason = reason
		return item
	}
	if rejection := provider.ClassifyCredentialRejection(0, nil, err); rejection.Rejected {
		reason := fmt.Sprintf("%s OAuth credential rejected", value.Provider)
		if markErr := s.markBuildDetectReauth(ctx, value.ID, reason); markErr != nil {
			item.Reason = errors.Join(err, markErr).Error()
			return item
		}
		item.Outcome = BuildDetectOutcomeInvalid
		item.Reason = reason
	}
	return item
}

func (s *Service) loadDetectBilling(ctx context.Context, id uint64) (*accountdomain.Billing, error) {
	snap, err := s.accounts.GetBilling(ctx, id)
	if err == nil {
		return &snap, nil
	}
	if errors.Is(err, repository.ErrNotFound) {
		return nil, nil
	}
	return nil, err
}

func (s *Service) forwardBuildDetect(ctx context.Context, value accountdomain.Credential, billing *accountdomain.Billing) (*provider.Response, error) {
	adapter, ok := s.providers.Responses(accountdomain.ProviderBuild)
	if !ok {
		return nil, fmt.Errorf("Provider %s 未注册 Responses 能力", accountdomain.ProviderBuild)
	}
	body := []byte(fmt.Sprintf(`{"model":%q,"input":%q}`, buildDetectModel, buildDetectPrompt))
	return adapter.ForwardResponse(ctx, provider.ResponseResourceRequest{
		Credential:    value,
		Billing:       billing,
		Method:        http.MethodPost,
		Path:          "/responses",
		Model:         buildDetectModel,
		Body:          body,
		NormalizeBody: true,
		Streaming:     false,
	})
}

// markBuildDetectReauth 与 markSSOCredentialRejected 一样不继承客户端取消，确保已确认失效的账号落库。
func (s *Service) markBuildDetectReauth(ctx context.Context, id uint64, reason string) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), credentialStateWriteTimeout)
	defer cancel()
	if err := s.MarkReauthRequired(writeCtx, id, reason); err != nil {
		s.logger.Error("account_reauth_required_write_failed", "account_id", id, "provider", accountdomain.ProviderBuild, "error", err)
		return err
	}
	return nil
}

func (s *Service) finishBuildDetectResponse(ctx context.Context, response *provider.Response, credential accountdomain.Credential, billing *accountdomain.Billing) BuildDetectItemResult {
	item := BuildDetectItemResult{
		AccountID:  credential.ID,
		Name:       credential.Name,
		Email:      credential.Email,
		Outcome:    BuildDetectOutcomeFailed,
		HTTPStatus: response.StatusCode,
	}
	if response.Body != nil {
		defer response.Body.Close()
	}
	body := readDetectBodyForClassification(response.Body)
	rejection := provider.ClassifyCredentialRejection(response.StatusCode, body, nil)
	if rejection.Rejected {
		reason := fmt.Sprintf("%s OAuth credential rejected (HTTP %d)", credential.Provider, response.StatusCode)
		if markErr := s.markBuildDetectReauth(ctx, credential.ID, reason); markErr != nil {
			item.Reason = markErr.Error()
			return item
		}
		item.Outcome = BuildDetectOutcomeInvalid
		item.Reason = reason
		return item
	}
	if rejection.SpendingLimitBlocked {
		reason := fmt.Sprintf("%s spending limit blocked", credential.Provider)
		if markErr := s.markBuildDetectQuotaExhausted(ctx, credential, billing); markErr != nil {
			item.Reason = errors.Join(errors.New(reason), markErr).Error()
			return item
		}
		item.Reason = reason
		return item
	}
	if rejection.ModelQuotaExhausted {
		reason := fmt.Sprintf("%s model quota exhausted for %s", credential.Provider, buildDetectModel)
		if markErr := s.markBuildDetectModelQuotaExhausted(ctx, credential, reason); markErr != nil {
			item.Reason = errors.Join(errors.New(reason), markErr).Error()
			return item
		}
		item.Reason = reason
		return item
	}
	if rejection.QuotaExhausted {
		reason := fmt.Sprintf("%s quota exhausted", credential.Provider)
		if markErr := s.markBuildDetectQuotaExhausted(ctx, credential, billing); markErr != nil {
			item.Reason = errors.Join(errors.New(reason), markErr).Error()
			return item
		}
		item.Reason = reason
		return item
	}
	if rejection.PermanentAccountDenial {
		reason := fmt.Sprintf("%s chat endpoint access denied for %s", credential.Provider, buildDetectModel)
		if markErr := s.markBuildDetectModelDenied(ctx, credential, reason); markErr != nil {
			item.Reason = errors.Join(errors.New(reason), markErr).Error()
			return item
		}
		item.Reason = reason
		return item
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		item.Reason = fmt.Sprintf("上游检测失败: HTTP %d", response.StatusCode)
		return item
	}
	item.Outcome = BuildDetectOutcomeOK
	item.Reason = ""
	return item
}

func (s *Service) markBuildDetectQuotaExhausted(ctx context.Context, credential accountdomain.Credential, billing *accountdomain.Billing) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), credentialStateWriteTimeout)
	defer cancel()
	now := s.now()
	recovery := accountdomain.QuotaRecovery{
		AccountID: credential.ID, Kind: accountdomain.QuotaRecoveryKindFree, Status: accountdomain.QuotaRecoveryStatusExhausted,
		ExhaustedAt: &now, LastConfirmedAt: &now, UpdatedAt: now,
	}
	nextProbeAt := now.Add(buildDetectQuotaRecoveryPause)
	if billing != nil && billing.IsPaid() {
		if periodEnd, ok := billing.PeriodEnd(); ok && periodEnd.After(now) {
			recovery.Kind = accountdomain.QuotaRecoveryKindPaid
			nextProbeAt = periodEnd
		}
	}
	recovery.NextProbeAt = &nextProbeAt
	if err := s.accounts.SaveQuotaRecovery(writeCtx, recovery); err != nil {
		s.logger.Error("account_quota_recovery_write_failed", "account_id", credential.ID, "provider", credential.Provider, "error", err)
		return err
	}
	if s.sticky != nil {
		if err := s.sticky.DeleteByAccount(writeCtx, credential.ID); err != nil {
			s.logger.Warn("account_sticky_delete_failed", "account_id", credential.ID, "provider", credential.Provider, "error", err)
		}
	}
	return nil
}

func (s *Service) markBuildDetectModelDenied(ctx context.Context, credential accountdomain.Credential, reason string) error {
	return s.markBuildDetectModelBlock(ctx, credential, "model_access_denied", buildDetectModelDeniedCooldown, reason)
}

func (s *Service) markBuildDetectModelQuotaExhausted(ctx context.Context, credential accountdomain.Credential, reason string) error {
	return s.markBuildDetectModelBlock(ctx, credential, "model_quota_depleted", buildDetectQuotaRecoveryPause, reason)
}

func (s *Service) markBuildDetectModelBlock(ctx context.Context, credential accountdomain.Credential, blockReason string, cooldown time.Duration, diagnostic string) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), credentialStateWriteTimeout)
	defer cancel()
	now := s.now()
	if err := s.accounts.UpsertModelQuotaBlock(writeCtx, accountdomain.ModelQuotaBlock{
		AccountID: credential.ID, UpstreamModel: buildDetectModel, Reason: blockReason,
		CooldownUntil: now.Add(cooldown), UpdatedAt: now,
	}); err != nil {
		s.logger.Error("account_model_block_write_failed", "account_id", credential.ID, "provider", credential.Provider, "model", buildDetectModel, "reason", diagnostic, "block_reason", blockReason, "error", err)
		return err
	}
	return nil
}

// readDetectBodyForClassification 读取响应正文用于凭据拒绝分类，最多 64 KiB。
// 返回的副本供 ClassifyCredentialRejection 解析；body 读完后会被消耗，调用方不应再使用。
func readDetectBodyForClassification(body io.ReadCloser) []byte {
	if body == nil {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(body, 64*1024))
	if err != nil {
		return nil
	}
	return data
}

func drainDetectBody(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 1<<20))
}
