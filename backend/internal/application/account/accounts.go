package account

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type View struct {
	Credential         accountdomain.Credential
	Billing            *accountdomain.Billing
	Quota              QuotaView
	QuotaWindows       []accountdomain.QuotaWindow
	BuildBotFlagged    bool
	BuildBotFlagSource int
	CredentialKind     string
	RefreshUnsupported bool
	// EnabledChanged is request-scoped update metadata. It is not persisted or
	// serialized directly; the HTTP layer uses it to avoid warning when a PATCH
	// merely repeats the account's existing enabled value.
	EnabledChanged bool
}

type UpdateInput struct {
	Name                   *string
	Enabled                *bool
	Priority               *int
	MaxConcurrent          *int
	MinimumRemaining       *float64
	CloudflareCookies      *string
	ClearCloudflareCookies bool
	// BuildSuperEntitled 仅 grok_build 可设置；非 Build 返回业务错误。
	BuildSuperEntitled *bool
	// BuildRouteMode 仅 grok_build 可设置；nil 表示不修改。
	BuildRouteMode *accountdomain.BuildRouteMode
}

type ListFilter struct {
	Provider  string
	QuotaType string
	Status    string
	Egress    string
	Renewal   string
	Risk      string
	// Agreement applies only to grok_web accounts.
	Agreement string
	// Association values are provider-specific: Web supports build, console, and combined filters;
	// Build and Console support only webLinked and webUnlinked.
	Association string
	Sort        repository.SortQuery
}

type Summary struct {
	Total      int64
	Available  int64
	Recovering int64
	Attention  int64
	Risk       int64
	Providers  map[string]ProviderSummary
	Recovery   RecoverySummary
	Issues     IssueSummary
}

type ProviderSummary struct {
	Total     int64
	Available int64
}

type RecoverySummary struct {
	Cooldown     int64
	WaitingReset int64
	Probing      int64
}

type IssueSummary struct {
	Disabled       int64
	ReauthRequired int64
}

func (s *Service) Summary(ctx context.Context) (Summary, error) {
	now := s.now()
	rows, err := s.accounts.Summarize(ctx, now)
	if err != nil {
		return Summary{}, err
	}
	providers := accountdomain.Providers()
	if s.providers != nil {
		providers = s.providers.Providers()
	}
	result := Summary{Providers: make(map[string]ProviderSummary, len(providers))}
	for _, providerValue := range providers {
		result.Providers[string(providerValue)] = ProviderSummary{}
	}
	for _, row := range rows {
		result.Total += row.Total
		result.Available += row.Available
		result.Recovery.Cooldown += row.Cooldown
		result.Recovery.WaitingReset += row.WaitingReset
		result.Recovery.Probing += row.Probing
		result.Issues.Disabled += row.Disabled
		result.Issues.ReauthRequired += row.ReauthRequired
		result.Providers[row.Provider] = ProviderSummary{Total: row.Total, Available: row.Available}
	}
	result.Recovering = result.Recovery.Cooldown + result.Recovery.WaitingReset + result.Recovery.Probing
	result.Attention = result.Issues.Disabled + result.Issues.ReauthRequired
	indexed, hasIndex := s.accounts.(buildBotFlagIndexRepository)
	var flaggedIDs []uint64
	if hasIndex {
		result.Risk, err = indexed.CountBuildBotFlagged(ctx)
	} else {
		flaggedIDs, err = s.buildBotFlaggedAccountIDs(ctx)
		result.Risk = int64(len(flaggedIDs))
	}
	if err != nil {
		return Summary{}, err
	}
	if s.excludeBuildBotFlaggedFromSchedulingEnabled() && result.Risk > 0 {
		var excluded int64
		if hasIndex {
			excluded, err = indexed.CountAvailableBuildBotFlagged(ctx, now)
		} else {
			excluded, err = s.accounts.CountAvailableAmong(ctx, accountdomain.ProviderBuild, flaggedIDs, now)
		}
		if err != nil {
			return Summary{}, err
		}
		if excluded > 0 {
			buildKey := string(accountdomain.ProviderBuild)
			build := result.Providers[buildKey]
			if excluded > build.Available {
				excluded = build.Available
			}
			build.Available -= excluded
			result.Providers[buildKey] = build
			if excluded > result.Available {
				excluded = result.Available
			}
			result.Available -= excluded
		}
	}
	return result, nil
}

func (s *Service) List(ctx context.Context, page, pageSize int, search string, filter ListFilter) ([]View, int64, error) {
	page, pageSize = normalizePage(page, pageSize)
	egressMode, egressNodeID, egressSourceID, egressValid := parseEgressFilter(filter.Egress)
	if (filter.Provider != "" && !accountdomain.Provider(filter.Provider).IsValid()) ||
		!oneOf(filter.QuotaType, "", "free", "paid", "unknown", "auto", "basic", "super", "heavy") ||
		!oneOf(filter.Status, "", "active", "disabled", "reauthRequired", "cooldown", "waitingReset", "probing") ||
		!egressValid ||
		!oneOf(filter.Renewal, "", "refreshable", "unrefreshable") ||
		!oneOf(filter.Risk, "", "flagged", "normal") ||
		(filter.Risk != "" && filter.Provider != string(accountdomain.ProviderBuild)) ||
		!oneOf(filter.Agreement, "", "nsfwEnabled", "nsfwDisabled", "termsAccepted", "termsNotAccepted", "allAccepted", "allNotAccepted") ||
		(filter.Agreement != "" && filter.Provider != string(accountdomain.ProviderWeb)) ||
		!validAssociationFilter(filter.Provider, filter.Association) ||
		!repository.IsValidSort(filter.Sort, "name", "type", "status", "createdAt") {
		return nil, 0, ErrInvalidFilter
	}
	var refreshable *bool
	if filter.Renewal != "" {
		value := filter.Renewal == "refreshable"
		refreshable = &value
	}
	repositoryFilter := repository.AccountListFilter{
		Provider: filter.Provider, QuotaType: filter.QuotaType, Status: filter.Status, Egress: egressMode,
		EgressNodeID: egressNodeID, EgressSourceID: egressSourceID,
		Refreshable: refreshable, Agreement: filter.Agreement, Association: filter.Association, Now: s.now(),
	}
	if filter.Risk != "" {
		if _, ok := s.accounts.(buildBotFlagIndexRepository); ok {
			repositoryFilter.Risk = filter.Risk
		} else {
			flaggedIDs, err := s.buildBotFlaggedAccountIDs(ctx)
			if err != nil {
				return nil, 0, err
			}
			if filter.Risk == "flagged" {
				repositoryFilter.AccountIDs = flaggedIDs
				repositoryFilter.RestrictIDs = true
			} else {
				repositoryFilter.ExcludeIDs = flaggedIDs
			}
		}
	}
	values, total, err := s.accounts.List(ctx, repository.AccountListQuery{
		Page:   repository.PageQuery{Offset: (page - 1) * pageSize, Limit: pageSize, Search: search, Sort: filter.Sort},
		Filter: repositoryFilter,
	})
	if err != nil {
		return nil, 0, err
	}
	accountIDs := make([]uint64, 0, len(values))
	for _, value := range values {
		accountIDs = append(accountIDs, value.ID)
	}
	observedTokens, err := s.audits.SumTokensByAccountsSince(ctx, accountIDs, time.Now().UTC().Add(-freeUsageWindow))
	if err != nil {
		return nil, 0, err
	}
	billings, err := s.accounts.GetBillings(ctx, accountIDs)
	if err != nil {
		return nil, 0, err
	}
	recoveries, err := s.accounts.GetQuotaRecoveries(ctx, accountIDs)
	if err != nil {
		return nil, 0, err
	}
	quotaWindows, err := s.accounts.GetQuotaWindows(ctx, accountIDs)
	if err != nil {
		return nil, 0, err
	}
	views := make([]View, 0, len(values))
	for _, value := range values {
		metadata := s.buildBotFlagMetadata(value)
		view := View{Credential: value, BuildBotFlagged: metadata.BuildBotFlagged, BuildBotFlagSource: metadata.BuildBotFlagSource, CredentialKind: metadata.CredentialKind, RefreshUnsupported: metadata.RefreshUnsupported}
		if billing, ok := billings[value.ID]; ok {
			view.Billing = &billing
		}
		var recovery *accountdomain.QuotaRecovery
		if recoveryValue, ok := recoveries[value.ID]; ok {
			recovery = &recoveryValue
		}
		view.Quota = newQuotaView(view.Billing, observedTokens[value.ID], recovery, value.ObservedModel, value.BuildSuperEntitled && value.Provider == accountdomain.ProviderBuild)
		view.QuotaWindows = quotaWindows[value.ID]
		views = append(views, view)
	}
	return views, total, nil
}

// parseEgressFilter splits the account egress filter into its bound/unbound mode
// and an optional narrowing target. Accepted values are "", "bound", "unbound",
// "node:<id>" and "source:<id>"; the last two are "bound" narrowed to one egress
// node or to every node owned by one subscription source.
func parseEgressFilter(value string) (mode string, nodeID uint64, sourceID uint64, ok bool) {
	if oneOf(value, "", "bound", "unbound") {
		return value, 0, 0, true
	}
	prefix, raw, found := strings.Cut(value, ":")
	if !found {
		return "", 0, 0, false
	}
	// Relational account and egress IDs are stored in signed BIGINT/INTEGER
	// columns. Reject values outside that range here so malformed filters cannot
	// reach database/sql as unsupported high-bit uint64 arguments and become 500s.
	id, err := strconv.ParseUint(raw, 10, 63)
	if err != nil || id == 0 {
		return "", 0, 0, false
	}
	switch prefix {
	case "node":
		return "bound", id, 0, true
	case "source":
		return "bound", 0, id, true
	default:
		return "", 0, 0, false
	}
}

// validAssociationFilter validates association filters against the selected provider.
// Web keeps its six Build/Console/combined values; Build and Console filter only by Web links.
func validAssociationFilter(providerValue, association string) bool {
	if association == "" {
		return true
	}
	switch providerValue {
	case string(accountdomain.ProviderWeb):
		return oneOf(association, "buildLinked", "buildUnlinked", "consoleLinked", "consoleUnlinked", "allLinked", "allUnlinked")
	case string(accountdomain.ProviderBuild), string(accountdomain.ProviderConsole):
		return oneOf(association, "webLinked", "webUnlinked")
	default:
		return false
	}
}

// BatchUpdate 对同一号池的一组账号应用相同路由参数。
func (s *Service) BatchUpdate(ctx context.Context, providerValue accountdomain.Provider, ids []uint64, input UpdateInput) (int64, error) {
	ids, err := normalizeIDs(ids, maxBatchUpdateAccounts)
	if err != nil {
		return 0, err
	}
	if !providerValue.IsValid() {
		return 0, invalidInput("账号来源无效")
	}
	slices.Sort(ids)
	if input.MaxConcurrent != nil && (*input.MaxConcurrent < 1 || *input.MaxConcurrent > accountdomain.MaxConcurrent) {
		return 0, invalidInput("maxConcurrent 必须在 1 到 256 之间")
	}
	if input.MinimumRemaining != nil && *input.MinimumRemaining < 0 {
		return 0, invalidInput("minimumRemaining 不能小于零")
	}
	if input.Name != nil {
		return 0, invalidInput("批量更新不支持修改账号名称")
	}
	updated, err := s.accounts.UpdateMany(ctx, providerValue, ids, repository.AccountUpdates{Enabled: input.Enabled, Priority: input.Priority, MaxConcurrent: input.MaxConcurrent, MinimumRemaining: input.MinimumRemaining})
	if err != nil {
		return 0, mapRepositoryError(err)
	}
	if input.Enabled != nil && !*input.Enabled && s.sticky != nil {
		if batchDeleter, ok := s.sticky.(repository.StickySessionBatchDeleter); ok {
			_ = batchDeleter.DeleteByAccounts(ctx, ids)
		} else {
			for _, id := range ids {
				_ = s.sticky.DeleteByAccount(ctx, id)
			}
		}
	}
	return updated, nil
}

func (s *Service) Get(ctx context.Context, id uint64) (View, error) {
	value, err := s.accounts.Get(ctx, id)
	if err != nil {
		return View{}, mapRepositoryError(err)
	}
	metadata := s.buildBotFlagMetadata(value)
	view := View{Credential: value, BuildBotFlagged: metadata.BuildBotFlagged, BuildBotFlagSource: metadata.BuildBotFlagSource, CredentialKind: metadata.CredentialKind, RefreshUnsupported: metadata.RefreshUnsupported}
	if billing, err := s.accounts.GetBilling(ctx, id); err == nil {
		view.Billing = &billing
	} else if !errors.Is(err, repository.ErrNotFound) {
		return View{}, err
	}
	observedTokens, err := s.audits.SumTokensByAccountsSince(ctx, []uint64{id}, time.Now().UTC().Add(-freeUsageWindow))
	if err != nil {
		return View{}, err
	}
	var recovery *accountdomain.QuotaRecovery
	if recoveryValue, err := s.accounts.GetQuotaRecovery(ctx, id); err == nil {
		recovery = &recoveryValue
	} else if !errors.Is(err, repository.ErrNotFound) {
		return View{}, err
	}
	view.Quota = newQuotaView(view.Billing, observedTokens[id], recovery, value.ObservedModel, value.BuildSuperEntitled && value.Provider == accountdomain.ProviderBuild)
	if windows, err := s.accounts.GetQuotaWindows(ctx, []uint64{id}); err == nil {
		view.QuotaWindows = windows[id]
	} else {
		return View{}, err
	}
	return view, nil
}

func (s *Service) credentialMetadata(value accountdomain.Credential) provider.CredentialMetadata {
	if s.providers == nil {
		return provider.CredentialMetadata{}
	}
	return s.providers.CredentialMetadata(value)
}

func (s *Service) buildBotFlagMetadata(value accountdomain.Credential) provider.CredentialMetadata {
	metadata := s.credentialMetadata(value)
	if metadata.BuildBotFlagInspected {
		return metadata
	}
	source := value.BuildBotFlagSource
	if source != 1 && source != 2 {
		source = 0
	}
	metadata.BuildBotFlagSource = source
	metadata.BuildBotFlagged = source != 0
	return metadata
}

func (s *Service) Update(ctx context.Context, id uint64, input UpdateInput) (View, error) {
	value, err := s.accounts.Get(ctx, id)
	if err != nil {
		return View{}, mapRepositoryError(err)
	}
	enabledChanged := input.Enabled != nil && value.Enabled != *input.Enabled
	if input.Name != nil {
		value.Name = strings.TrimSpace(*input.Name)
		if value.Name == "" {
			return View{}, invalidInput("账号名称不能为空")
		}
	}
	if input.Enabled != nil {
		value.Enabled = *input.Enabled
	}
	if input.Priority != nil {
		value.Priority = *input.Priority
	}
	if input.MaxConcurrent != nil {
		if *input.MaxConcurrent < 1 || *input.MaxConcurrent > accountdomain.MaxConcurrent {
			return View{}, invalidInput("maxConcurrent 必须在 1 到 256 之间")
		}
		value.MaxConcurrent = *input.MaxConcurrent
	}
	if input.MinimumRemaining != nil {
		if *input.MinimumRemaining < 0 {
			return View{}, invalidInput("minimumRemaining 不能小于零")
		}
		value.MinimumRemaining = *input.MinimumRemaining
	}
	if input.ClearCloudflareCookies {
		value.EncryptedCloudflareCookie = ""
	} else if input.CloudflareCookies != nil {
		if value.Provider == accountdomain.ProviderBuild {
			return View{}, invalidInput("Grok Build 账号不使用 Cloudflare Cookie")
		}
		if len(*input.CloudflareCookies) > 16<<10 {
			return View{}, invalidInput("Cloudflare Cookie 不能超过 16 KiB")
		}
		if strings.TrimSpace(*input.CloudflareCookies) != "" {
			cookies := egressapp.SanitizeCloudflareCookies(*input.CloudflareCookies)
			if cookies == "" {
				return View{}, invalidInput("Cloudflare Cookie 中没有有效字段")
			}
			encrypted, encryptErr := s.cipher.Encrypt(cookies)
			if encryptErr != nil {
				return View{}, encryptErr
			}
			value.EncryptedCloudflareCookie = encrypted
		}
	}
	if input.BuildSuperEntitled != nil {
		if value.Provider != accountdomain.ProviderBuild {
			return View{}, invalidInput("仅 Grok Build 账号支持设置 Build Super entitlement")
		}
		value.BuildSuperEntitled = *input.BuildSuperEntitled
	}
	if input.BuildRouteMode != nil {
		if value.Provider != accountdomain.ProviderBuild {
			return View{}, invalidInput("仅 Grok Build 账号支持设置上游地址")
		}
		if !input.BuildRouteMode.IsValid() {
			return View{}, invalidInput("Build 上游地址必须是 auto、build 或 xai")
		}
		value.BuildRouteMode = *input.BuildRouteMode
	}
	updated, err := s.accounts.Update(ctx, value)
	if err != nil {
		return View{}, mapRepositoryError(err)
	}
	if !updated.Enabled && s.sticky != nil {
		_ = s.sticky.DeleteByAccount(ctx, updated.ID)
	} else if updated.Enabled && s.providers != nil && s.providers.SupportsCredentialRefresh(updated.Provider) {
		s.WakeCredentialRefresh()
	}
	view, err := s.Get(ctx, updated.ID)
	if err != nil {
		return View{}, err
	}
	view.EnabledChanged = enabledChanged
	return view, nil
}

// ClearCooldown resets request-path health so a cooled account can be
// scheduled again. UpdateHealth publishes InvalidationAccountHealthChanged,
// which overwrites the selector memory overlay (runtimeStore=memory).
func (s *Service) ClearCooldown(ctx context.Context, id uint64) (View, error) {
	value, err := s.accounts.Get(ctx, id)
	if err != nil {
		return View{}, mapRepositoryError(err)
	}
	// missing_thinking is a durable quality strike, not a transient cooldown
	// error. Clearing the timer must not turn the next miss into another first
	// strike and bypass the second-miss disable policy.
	healthMarker := accountdomain.NormalizeHealthMarker(value.LastError)
	if err := s.accounts.UpdateHealth(ctx, value.ID, value.Provider, 0, nil, healthMarker, false); err != nil {
		return View{}, mapRepositoryError(err)
	}
	return s.Get(ctx, id)
}

// MarkBuildAPIFallback 幂等写入 Build 账号 XAI 推理回退标记；失败不吞掉，调用方可重试。
func (s *Service) MarkBuildAPIFallback(ctx context.Context, id uint64, enabled bool) error {
	return mapRepositoryError(s.accounts.MarkBuildAPIFallback(ctx, id, enabled))
}

func (s *Service) MarkReauthRequired(ctx context.Context, id uint64, reason string) error {
	if err := s.accounts.MarkReauthRequired(ctx, id, reason); err != nil {
		return mapRepositoryError(err)
	}
	if s.sticky != nil {
		_ = s.sticky.DeleteByAccount(ctx, id)
	}
	return nil
}

// markSSOCredentialRejected 在上游明确返回 401 后可靠持久化失效状态。
// 状态写入不继承客户端取消，避免已经确认失效的账号因请求断开继续留在号池。
func (s *Service) markSSOCredentialRejected(ctx context.Context, value accountdomain.Credential, reason string) error {
	if value.AuthType != accountdomain.AuthTypeSSO {
		return nil
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), credentialStateWriteTimeout)
	defer cancel()
	if err := s.MarkReauthRequired(writeCtx, value.ID, reason); err != nil {
		s.logger.Error("account_reauth_required_write_failed", "account_id", value.ID, "provider", value.Provider, "error", err)
		return err
	}
	return nil
}
