package account

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type RateLimitReconcileState string

const (
	RateLimitReconcileInconclusive RateLimitReconcileState = "inconclusive"
	RateLimitReconcileRefreshing   RateLimitReconcileState = "refreshing"
	RateLimitReconcileAvailable    RateLimitReconcileState = "available"
	RateLimitReconcileExhausted    RateLimitReconcileState = "exhausted"
)

type quotaRefreshResult struct {
	Credential accountdomain.Credential
	Windows    []accountdomain.QuotaWindow
	Modes      []string
	// Complete marks a snapshot listing every mode the Provider still limits.
	Complete bool
}

// quotaModeSnapshotAdapter is a quota adapter whose single-mode sync reads
// the provider's complete quota snapshot (ChatGPT conversation/init). A
// per-mode refresh then stores and reconciles every window it returned.
type quotaModeSnapshotAdapter interface {
	QuotaModeSyncReadsSnapshot()
}

func (s *Service) HasQuotaWindows(ctx context.Context, id uint64) (bool, error) {
	return s.accounts.HasQuotaWindows(ctx, id)
}

func (s *Service) DecrementQuota(ctx context.Context, id uint64, mode string, amount int) (bool, error) {
	if amount <= 0 {
		amount = 1
	}
	if repository, ok := s.accounts.(interface {
		DecrementQuotaWindowBy(context.Context, uint64, string, int, time.Time) (bool, error)
	}); ok {
		return repository.DecrementQuotaWindowBy(ctx, id, mode, amount, s.now())
	}
	updated := false
	for range amount {
		decremented, err := s.accounts.DecrementQuotaWindow(ctx, id, mode, s.now())
		if err != nil {
			return updated, err
		}
		if !decremented {
			break
		}
		updated = true
	}
	return updated, nil
}

func (s *Service) DecrementWebQuota(ctx context.Context, id uint64, mode string, amount int) (bool, error) {
	return s.DecrementQuota(ctx, id, mode, amount)
}

func (s *Service) ExhaustQuota(ctx context.Context, id uint64, mode string, resetAt *time.Time) error {
	if resetAt == nil {
		windows, err := s.accounts.GetQuotaWindows(ctx, []uint64{id})
		if err == nil {
			for _, window := range windows[id] {
				if window.Mode != mode {
					continue
				}
				resetAt = quotaRecoveryDueAt(window, s.now(), true)
				break
			}
		}
	}
	if err := s.accounts.ExhaustQuotaWindow(ctx, id, mode, resetAt, s.now()); err != nil {
		return err
	}
	if resetAt != nil && s.quotaQueue != nil {
		return s.quotaQueue.ScheduleQuotaRecovery(ctx, accountdomain.QuotaRecoveryEvent{AccountID: id, Mode: mode, DueAt: *resetAt})
	}
	return nil
}

func (s *Service) ExhaustWebQuota(ctx context.Context, id uint64, mode string, resetAt *time.Time) error {
	return s.ExhaustQuota(ctx, id, mode, resetAt)
}

func (s *Service) RefreshQuota(ctx context.Context, id uint64) ([]accountdomain.QuotaWindow, error) {
	result, err, _ := s.quotaSyncs.Do("all:"+strconv.FormatUint(id, 10), func() (any, error) {
		return s.refreshQuota(ctx, id)
	})
	if err != nil {
		return nil, err
	}
	refreshed, ok := result.(quotaRefreshResult)
	if !ok {
		return nil, fmt.Errorf("Provider 额度同步返回类型无效")
	}
	if err := s.reconcileQuotaRecoveryWindows(ctx, refreshed.Credential.Provider, id, refreshed.Windows); err != nil {
		return refreshed.Windows, err
	}
	// 身份补全是非关键操作：只在额度落库和恢复任务调度完成后执行，
	// 并沿用调用方取消语义，不能反向影响额度同步结果。
	if _, ok := s.providers.AccountIdentity(refreshed.Credential.Provider); ok && ctx.Err() == nil {
		// SyncAccountIdentity 会自行判断身份是否完整。Web 账号必须具备合法
		// Gateway UUID，不能因为旧记录里只有 email 就跳过迁移。
		if identityErr := s.syncAccountIdentityBestEffort(ctx, id); errors.Is(identityErr, provider.ErrUnauthorized) {
			return refreshed.Windows, identityErr
		}
	}
	return refreshed.Windows, nil
}

func (s *Service) refreshQuota(ctx context.Context, id uint64) (quotaRefreshResult, error) {
	value, err := s.accounts.Get(ctx, id)
	if err != nil {
		return quotaRefreshResult{}, mapRepositoryError(err)
	}
	adapter, ok := s.providers.Quota(value.Provider)
	if !ok {
		return quotaRefreshResult{}, fmt.Errorf("%s Quota Provider 未注册", value.Provider)
	}
	snapshot, err := adapter.SyncQuota(ctx, value)
	if err != nil {
		if errors.Is(err, provider.ErrUnauthorized) {
			err = errors.Join(err, s.markSSOCredentialRejected(ctx, value, fmt.Sprintf("%s SSO credential rejected", value.Provider)))
		}
		return quotaRefreshResult{}, err
	}
	quotaKind, _ := s.providers.QuotaKind(value.Provider)
	if quotaKind == provider.QuotaLocalWindow {
		existing, loadErr := s.accounts.GetQuotaWindows(ctx, []uint64{id})
		if loadErr != nil {
			return quotaRefreshResult{}, loadErr
		}
		snapshot.Windows = preserveActiveQuotaWindows(existing[id], snapshot.Windows, s.now())
	}
	if err := s.accounts.ReplaceQuotaWindows(ctx, id, snapshot.Tier, snapshot.SyncedAt, snapshot.Windows); err != nil {
		return quotaRefreshResult{}, err
	}
	return quotaRefreshResult{Credential: value, Windows: snapshot.Windows}, nil
}

func preserveActiveQuotaWindows(existing, incoming []accountdomain.QuotaWindow, now time.Time) []accountdomain.QuotaWindow {
	byMode := make(map[string]accountdomain.QuotaWindow, len(existing))
	for _, window := range existing {
		byMode[window.Mode] = window
	}
	result := append([]accountdomain.QuotaWindow(nil), incoming...)
	for index, window := range result {
		current, ok := byMode[window.Mode]
		if !ok || current.ResetAt == nil || !current.ResetAt.After(now) {
			continue
		}
		result[index] = current
	}
	return result
}

// ReconcileRateLimit 根据额度模式核实 429。Web 周池和 Console
// 均以上游快照为准；Console 的 resource-exhausted 还可能表示瞬时
// RPS/RPM 限流，不能在未查询 /usage 时直接将账号冻结 24 小时。
func (s *Service) ReconcileRateLimit(ctx context.Context, id uint64, mode string, retryAfter time.Duration) (RateLimitReconcileState, error) {
	if mode == "weekly" || isConsoleUsageQuotaMode(mode) {
		var window accountdomain.QuotaWindow
		var err error
		if isConsoleUsageQuotaMode(mode) {
			window, err = s.refreshConsoleQuotaModeLocked(ctx, id, mode)
		} else {
			window, err = s.RefreshQuotaMode(ctx, id, mode)
		}
		if err != nil {
			if isConsoleUsageQuotaMode(mode) {
				if errors.Is(err, errQuotaRefreshBusy) {
					return RateLimitReconcileRefreshing, nil
				}
				// Keep the last known snapshot on an inconclusive probe and let the
				// bounded refresh worker retry. The gateway will still apply its normal
				// account cooldown and rotate this request to another account.
				s.QueueQuotaRefresh(id, mode)
				s.logger.Warn("console_rate_limit_quota_probe_failed", "account_id", id, "mode", mode, "error", err)
			}
			return RateLimitReconcileInconclusive, err
		}
		if window.Remaining == 0 || window.UsagePercent >= 100 {
			return RateLimitReconcileExhausted, nil
		}
		return RateLimitReconcileAvailable, nil
	}
	windows, err := s.accounts.GetQuotaWindows(ctx, []uint64{id})
	if err != nil {
		return RateLimitReconcileInconclusive, err
	}
	if _, ok := quotaWindowByMode(windows[id], mode); !ok {
		// No window limits this mode (a Plus account has no message limit):
		// the 429 is a rate limit the caller cools down, not a spent quota.
		return RateLimitReconcileInconclusive, nil
	}
	var resetAt *time.Time
	if retryAfter > 0 {
		value := s.now().Add(retryAfter)
		resetAt = &value
	}
	if err := s.ExhaustQuota(ctx, id, mode, resetAt); err != nil {
		return RateLimitReconcileInconclusive, err
	}
	return RateLimitReconcileExhausted, nil
}

func (s *Service) ReconcileWebRateLimit(ctx context.Context, id uint64, mode string, retryAfter time.Duration) (bool, error) {
	state, err := s.ReconcileRateLimit(ctx, id, mode, retryAfter)
	return state == RateLimitReconcileExhausted, err
}

func (s *Service) RefreshQuotaMode(ctx context.Context, id uint64, mode string) (accountdomain.QuotaWindow, error) {
	mode = strings.TrimSpace(mode)
	key := quotaSyncKey(id, mode)
	result, err, _ := s.quotaSyncs.Do(key, func() (any, error) {
		if isWebImagineQuotaMode(mode) {
			return s.refreshQuotaGroup(ctx, id, accountdomain.QuotaGroupWebImagine)
		}
		return s.refreshQuotaMode(ctx, id, mode)
	})
	if err != nil {
		return accountdomain.QuotaWindow{}, err
	}
	refreshed, ok := result.(quotaRefreshResult)
	if !ok {
		return accountdomain.QuotaWindow{}, fmt.Errorf("Provider 模式额度同步返回类型无效")
	}
	if len(refreshed.Modes) > 0 {
		if err := s.reconcileQuotaGroupWindows(ctx, refreshed.Credential.Provider, id, refreshed.Modes, refreshed.Windows); err != nil {
			return accountdomain.QuotaWindow{}, err
		}
	}
	window, err := s.resolveRefreshedQuotaWindow(ctx, id, mode, refreshed)
	if err != nil {
		return accountdomain.QuotaWindow{}, err
	}
	if len(refreshed.Modes) == 0 && refreshed.Credential.Provider == accountdomain.ProviderConsole {
		// One Console request refreshes all three authoritative windows. Reconcile
		// every matching recovery event so externally consumed media quota cannot
		// remain unscheduled merely because a different kind triggered the refresh.
		if err := s.reconcileQuotaRecoveryWindows(ctx, refreshed.Credential.Provider, id, refreshed.Windows); err != nil {
			return window, err
		}
	} else if len(refreshed.Modes) == 0 {
		if err := s.reconcileQuotaRecoveryWindow(ctx, refreshed.Credential.Provider, id, window); err != nil {
			return window, err
		}
	} else if window.Mode == "weekly" {
		// The requested Imagine product was availability-only and resolved to
		// the paid shared pool. Reconcile the authoritative weekly window too;
		// the group reconciliation above only covers product-specific modes.
		if err := s.reconcileQuotaRecoveryWindow(ctx, refreshed.Credential.Provider, id, window); err != nil {
			return window, err
		}
	}
	return window, nil
}

// ProbeQuotaMode refreshes a claimed recovery event without scheduling a
// second event for the same account and mode. The recovery worker owns the
// current claim and is responsible for acknowledging or rescheduling it.
func (s *Service) ProbeQuotaMode(ctx context.Context, id uint64, mode string) (accountdomain.QuotaWindow, error) {
	mode = strings.TrimSpace(mode)
	key := quotaSyncKey(id, mode)
	result, err, _ := s.quotaSyncs.Do(key, func() (any, error) {
		if isWebImagineQuotaMode(mode) {
			return s.refreshQuotaGroup(ctx, id, accountdomain.QuotaGroupWebImagine)
		}
		return s.refreshQuotaMode(ctx, id, mode)
	})
	if err != nil {
		return accountdomain.QuotaWindow{}, err
	}
	refreshed, ok := result.(quotaRefreshResult)
	if !ok {
		return accountdomain.QuotaWindow{}, fmt.Errorf("Provider 模式额度探测返回类型无效")
	}
	return s.resolveRefreshedQuotaWindow(ctx, id, mode, refreshed)
}

func (s *Service) resolveRefreshedQuotaWindow(ctx context.Context, id uint64, mode string, refreshed quotaRefreshResult) (accountdomain.QuotaWindow, error) {
	if window, ok := quotaWindowByMode(refreshed.Windows, mode); ok {
		return window, nil
	}
	if refreshed.Complete {
		return accountdomain.QuotaWindow{}, fmt.Errorf("%w: %s", accountdomain.ErrQuotaNotLimited, mode)
	}
	credential := refreshed.Credential
	paidWebImagine := credential.Provider == accountdomain.ProviderWeb && isWebImagineQuotaMode(mode) &&
		(credential.WebTier == accountdomain.WebTierSuper || credential.WebTier == accountdomain.WebTierHeavy)
	if paidWebImagine {
		weekly, err := s.refreshQuotaMode(ctx, id, "weekly")
		if err != nil {
			return accountdomain.QuotaWindow{}, err
		}
		if window, ok := quotaWindowByMode(weekly.Windows, "weekly"); ok {
			return window, nil
		}
	}
	return accountdomain.QuotaWindow{}, fmt.Errorf("Provider usage 响应缺少 %s 额度", mode)
}

func (s *Service) refreshQuotaGroup(ctx context.Context, id uint64, group string) (quotaRefreshResult, error) {
	value, err := s.accounts.Get(ctx, id)
	if err != nil {
		return quotaRefreshResult{}, mapRepositoryError(err)
	}
	adapter, ok := s.providers.QuotaGroup(value.Provider)
	if !ok {
		return quotaRefreshResult{}, fmt.Errorf("%s quota group Provider 未注册", value.Provider)
	}
	snapshot, err := adapter.SyncQuotaGroup(ctx, value, group)
	if err != nil {
		if errors.Is(err, provider.ErrUnauthorized) {
			err = errors.Join(err, s.markSSOCredentialRejected(ctx, value, fmt.Sprintf("%s SSO credential rejected", value.Provider)))
		}
		return quotaRefreshResult{}, err
	}
	if snapshot.Group != group || len(snapshot.Modes) == 0 {
		return quotaRefreshResult{}, fmt.Errorf("Provider quota group %s 返回无效快照", group)
	}
	if snapshot.SyncedAt.IsZero() {
		snapshot.SyncedAt = s.now()
	}
	if err := s.accounts.ReplaceQuotaWindowGroup(ctx, id, snapshot.SyncedAt, snapshot.Modes, snapshot.Windows); err != nil {
		return quotaRefreshResult{}, err
	}
	return quotaRefreshResult{Credential: value, Windows: snapshot.Windows, Modes: snapshot.Modes}, nil
}

func (s *Service) refreshQuotaMode(ctx context.Context, id uint64, mode string) (quotaRefreshResult, error) {
	value, err := s.accounts.Get(ctx, id)
	if err != nil {
		return quotaRefreshResult{}, mapRepositoryError(err)
	}
	adapter, ok := s.providers.Quota(value.Provider)
	if !ok {
		return quotaRefreshResult{}, fmt.Errorf("%s Quota Provider 未注册", value.Provider)
	}
	var window accountdomain.QuotaWindow
	var windows []accountdomain.QuotaWindow
	var syncedAt time.Time
	var tier accountdomain.WebTier
	_, snapshotSync := adapter.(quotaModeSnapshotAdapter)
	if value.Provider == accountdomain.ProviderConsole || snapshotSync {
		// Console /usage always returns Chat, Image and Video together, ChatGPT
		// conversation/init every limited feature. Persist the response as one
		// authoritative snapshot so each route observes the same upstream usage
		// generation and traffic on one mode keeps the others current.
		var snapshot provider.QuotaSnapshot
		snapshot, err = adapter.SyncQuota(ctx, value)
		if err == nil {
			windows = snapshot.Windows
			syncedAt = snapshot.SyncedAt
			for _, candidate := range windows {
				if candidate.Mode == mode {
					window = candidate
					break
				}
			}
			if window.Mode == "" && !snapshotSync {
				err = fmt.Errorf("Console usage 响应缺少 %s 额度", mode)
			}
		}
	} else {
		window, err = adapter.SyncQuotaMode(ctx, value, mode)
		windows = []accountdomain.QuotaWindow{window}
		syncedAt = s.now()
	}
	if err != nil {
		if errors.Is(err, provider.ErrUnauthorized) {
			err = errors.Join(err, s.markSSOCredentialRejected(ctx, value, fmt.Sprintf("%s SSO credential rejected", value.Provider)))
		}
		return quotaRefreshResult{}, err
	}
	quotaKind, _ := s.providers.QuotaKind(value.Provider)
	if quotaKind == provider.QuotaRemoteWindow {
		// Web reconciliation updates one mode; Console already supplied and
		// persisted its complete /usage snapshot above.
		tier = value.WebTier
	}
	if syncedAt.IsZero() {
		syncedAt = s.now()
	}
	if value.Provider == accountdomain.ProviderConsole || snapshotSync {
		if err := s.accounts.ReplaceQuotaWindows(ctx, id, tier, syncedAt, windows); err != nil {
			return quotaRefreshResult{}, err
		}
	} else if err := s.accounts.SaveQuotaWindows(ctx, id, tier, syncedAt, windows); err != nil {
		return quotaRefreshResult{}, err
	}
	result := quotaRefreshResult{Credential: value, Windows: windows, Complete: snapshotSync}
	if snapshotSync {
		// The snapshot covers every declared mode: RefreshQuotaMode reconciles
		// them all and cancels recovery of a mode that is no longer limited.
		definition, _ := s.providers.Definition(value.Provider)
		result.Modes = definition.Profile.QuotaModes
	}
	return result, nil
}

func quotaSyncKey(accountID uint64, mode string) string {
	mode = strings.TrimSpace(mode)
	if isConsoleUsageQuotaMode(mode) {
		return "all:" + strconv.FormatUint(accountID, 10)
	}
	if isWebImagineQuotaMode(mode) || mode == accountdomain.QuotaGroupWebImagine {
		return accountdomain.QuotaGroupWebImagine + ":" + strconv.FormatUint(accountID, 10)
	}
	return mode + ":" + strconv.FormatUint(accountID, 10)
}

func quotaWindowByMode(windows []accountdomain.QuotaWindow, mode string) (accountdomain.QuotaWindow, bool) {
	for _, window := range windows {
		if window.Mode == mode {
			return window, true
		}
	}
	return accountdomain.QuotaWindow{}, false
}

func (s *Service) reconcileQuotaRecoveryWindows(ctx context.Context, providerValue accountdomain.Provider, accountID uint64, windows []accountdomain.QuotaWindow) error {
	for _, window := range windows {
		if err := s.reconcileQuotaRecoveryWindow(ctx, providerValue, accountID, window); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) reconcileQuotaGroupWindows(ctx context.Context, providerValue accountdomain.Provider, accountID uint64, modes []string, windows []accountdomain.QuotaWindow) error {
	byMode := make(map[string]accountdomain.QuotaWindow, len(windows))
	for _, window := range windows {
		byMode[window.Mode] = window
	}
	for _, mode := range modes {
		if window, ok := byMode[mode]; ok {
			if err := s.reconcileQuotaRecoveryWindow(ctx, providerValue, accountID, window); err != nil {
				return err
			}
			continue
		}
		if s.quotaQueue != nil {
			if err := s.quotaQueue.CancelQuotaRecovery(ctx, accountID, mode); err != nil {
				return fmt.Errorf("取消额度恢复事件: %w", err)
			}
		}
	}
	return nil
}

func (s *Service) reconcileQuotaRecoveryWindow(ctx context.Context, providerValue accountdomain.Provider, accountID uint64, window accountdomain.QuotaWindow) error {
	if s.quotaQueue == nil || !s.quotaWindowControlsRouting(providerValue, window.Mode) {
		return nil
	}
	if dueAt := quotaRecoveryDueAt(window, s.now(), window.Remaining == 0); dueAt != nil {
		if err := s.quotaQueue.ScheduleQuotaRecovery(ctx, accountdomain.QuotaRecoveryEvent{AccountID: accountID, Mode: window.Mode, DueAt: *dueAt}); err != nil {
			return fmt.Errorf("安排额度恢复事件: %w", err)
		}
		return nil
	}
	if err := s.quotaQueue.CancelQuotaRecovery(ctx, accountID, window.Mode); err != nil {
		return fmt.Errorf("取消额度恢复事件: %w", err)
	}
	return nil
}

// quotaRecoveryDueAt keeps upstream quota exhaustion recoverable even when
// the Provider reports no reset timestamp. Console uses a conservative
// predicted 24-hour probe window; generic remote windows retain the shorter
// fallback and transport failures use the recovery queue's bounded backoff.
func quotaRecoveryDueAt(window accountdomain.QuotaWindow, now time.Time, exhausted bool) *time.Time {
	if !exhausted {
		return nil
	}
	if window.ResetAt != nil && window.ResetAt.After(now) {
		value := *window.ResetAt
		return &value
	}
	if isConsoleUsageQuotaMode(window.Mode) {
		value := now.Add(consolePredictedQuotaProbeDelay)
		return &value
	}
	if window.Source == accountdomain.QuotaSourceUpstream {
		value := now.Add(unknownRemoteQuotaProbeDelay)
		return &value
	}
	return nil
}

func (s *Service) ListDueWebQuotaWindows(ctx context.Context, now time.Time, limit int) ([]accountdomain.QuotaWindow, error) {
	windows, err := s.ListDueQuotaWindows(ctx, now, limit)
	if err != nil {
		return nil, err
	}
	result := make([]accountdomain.QuotaWindow, 0, len(windows))
	for _, window := range windows {
		credential, getErr := s.accounts.Get(ctx, window.AccountID)
		if errors.Is(getErr, repository.ErrNotFound) {
			continue
		}
		if getErr != nil {
			return nil, getErr
		}
		if credential.Provider == accountdomain.ProviderWeb {
			result = append(result, window)
		}
	}
	return result, nil
}

func (s *Service) ListDueQuotaWindows(ctx context.Context, now time.Time, limit int) ([]accountdomain.QuotaWindow, error) {
	return s.accounts.ListDueQuotaWindows(ctx, now, limit)
}

// snapshotQuotaMode reports a mode of a provider whose quota sync reads every
// window in one call (QuotaModeSyncReadsSnapshot).
func (s *Service) snapshotQuotaMode(mode string) bool {
	if s.providers == nil {
		return false
	}
	for _, value := range s.providers.Providers() {
		definition, ok := s.providers.Definition(value)
		if !ok || !slices.Contains(definition.Profile.QuotaModes, mode) {
			continue
		}
		adapter, ok := s.providers.Quota(value)
		if !ok {
			return false
		}
		_, snapshot := adapter.(quotaModeSnapshotAdapter)
		return snapshot
	}
	return false
}

// isDeclaredQuotaMode reports whether any registered provider lists mode in its Definition.
func (s *Service) isDeclaredQuotaMode(mode string) bool {
	if s.providers == nil {
		return false
	}
	for _, value := range s.providers.Providers() {
		definition, ok := s.providers.Definition(value)
		if ok && slices.Contains(definition.Profile.QuotaModes, mode) {
			return true
		}
	}
	return false
}

func isWebChatQuotaMode(mode string) bool {
	switch mode {
	case "auto", "fast", "expert", "heavy":
		return true
	default:
		return false
	}
}

func isConsoleUsageQuotaMode(mode string) bool {
	switch mode {
	case "console", "console_image", "console_video":
		return true
	default:
		return false
	}
}

func isWebImagineQuotaMode(mode string) bool {
	return accountdomain.IsWebImagineQuotaMode(mode)
}

func (s *Service) quotaWindowControlsRouting(providerValue accountdomain.Provider, mode string) bool {
	if providerValue == accountdomain.ProviderConsole {
		return isConsoleUsageQuotaMode(mode)
	}
	if s.providers != nil {
		if definition, ok := s.providers.Definition(providerValue); ok && slices.Contains(definition.Profile.DisplayQuotaModes, mode) {
			return false
		}
	}
	return true
}

// SyncAllQuotasWithProgress 尽力同步一个 Provider 全部启用账号的额度：
// 窗口额度 Provider 同步额度窗口，Billing Provider 同步 Billing 快照。
func (s *Service) SyncAllQuotasWithProgress(ctx context.Context, providerValue accountdomain.Provider, progress BatchProgressObserver) (int, int, error) {
	quotaKind, ok := s.providers.QuotaKind(providerValue)
	if !ok {
		return 0, 0, fmt.Errorf("%w: Provider %s 未注册", ErrUnsupported, providerValue)
	}
	ids, err := s.accounts.ListEnabledAccountIDs(ctx, providerValue, false)
	if err != nil {
		return 0, 0, err
	}
	if quotaKind == provider.QuotaBilling {
		return s.refreshBillings(ctx, ids, progress)
	}
	return s.runAccountBatch(ctx, string(providerValue)+"_quota_sync", ids, s.syncPool, progress, func(workCtx context.Context, id uint64) error {
		_, err := s.RefreshQuota(workCtx, id)
		return err
	})
}

// SyncQuotaAccounts 同步指定账号集合的额度窗口，供追赶任务复用共享并发池。
func (s *Service) SyncQuotaAccounts(ctx context.Context, ids []uint64) (int, int, error) {
	return s.runAccountBatch(ctx, "quota_stale_catchup", ids, s.syncPool, nil, func(workCtx context.Context, id uint64) error {
		_, err := s.RefreshQuota(workCtx, id)
		return err
	})
}

// BatchResetQuotaState clears local Build quota recovery state without changing
// upstream billing snapshots or historical audit usage.
func (s *Service) BatchResetQuotaState(ctx context.Context, ids []uint64) (int, error) {
	values, err := normalizeIDs(ids, maxQuotaResetAccounts)
	if err != nil {
		return 0, err
	}
	for start := 0; start < len(values); start += quotaResetChunkSize {
		end := min(start+quotaResetChunkSize, len(values))
		count, countErr := s.accounts.CountProviderAccountsByIDs(ctx, accountdomain.ProviderBuild, values[start:end])
		if countErr != nil {
			return 0, countErr
		}
		if count != int64(end-start) {
			return 0, invalidInput("仅 Grok Build 账号支持手动重置额度状态")
		}
	}
	reset := 0
	for start := 0; start < len(values); start += quotaResetChunkSize {
		if err := ctx.Err(); err != nil {
			return reset, err
		}
		end := min(start+quotaResetChunkSize, len(values))
		if err := s.accounts.ResetQuotaState(ctx, accountdomain.ProviderBuild, values[start:end]); err != nil {
			return reset, err
		}
		reset += end - start
	}
	return reset, nil
}

// ResetAllBuildQuotaState clears local quota state for every enabled Build
// account without materializing the complete account ID set in memory.
func (s *Service) ResetAllBuildQuotaState(ctx context.Context) (int64, error) {
	return s.accounts.ResetProviderQuotaState(ctx, accountdomain.ProviderBuild, true)
}

// BatchRefreshQuota 使用有限并发同步选中 Web 或 Console 账号的额度窗口。
func (s *Service) BatchRefreshQuota(ctx context.Context, ids []uint64) (int, int, error) {
	values, err := normalizeBatchIDs(ids)
	if err != nil {
		return 0, 0, err
	}
	return s.runAccountBatch(ctx, "quota_sync", values, s.syncPool, nil, func(workCtx context.Context, id uint64) error {
		_, err := s.RefreshQuota(workCtx, id)
		return err
	})
}
