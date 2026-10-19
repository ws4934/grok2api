package gateway

import (
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/pkg/resultcache"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"golang.org/x/sync/singleflight"
)

const concurrencySnapshotTTL = 25 * time.Millisecond
const maxConcurrencySnapshots = 256

type SelectionUnavailableReason string

const (
	SelectionNoAccounts        SelectionUnavailableReason = "no_accounts"
	SelectionUnsupportedModel  SelectionUnavailableReason = "unsupported_model"
	SelectionCooling           SelectionUnavailableReason = "cooling"
	SelectionModelCooling      SelectionUnavailableReason = "model_cooling"
	SelectionQuotaExhausted    SelectionUnavailableReason = "quota_exhausted"
	SelectionSaturated         SelectionUnavailableReason = "saturated"
	SelectionPinnedUnavailable SelectionUnavailableReason = "pinned_unavailable"
)

// SelectionUnavailableError 保留选号失败的真实原因，避免所有情况都退化成模糊的 503。
type SelectionUnavailableError struct {
	Reason      SelectionUnavailableReason
	RetryAfter  time.Duration
	Scope       clientkeydomain.AccountScope
	AccountID   uint64
	AccountName string
}

func (e *SelectionUnavailableError) Error() string {
	if e == nil {
		return "没有可用上游账号"
	}
	prefix := ""
	if e.Scope.IsRestricted() {
		prefix = "Client Key 限定范围"
	}
	switch e.Reason {
	case SelectionUnsupportedModel:
		if prefix != "" {
			return prefix + "不支持该模型"
		}
		return "当前账号池不支持该模型"
	case SelectionCooling:
		if prefix != "" {
			return prefix + "中的可用账号正在冷却"
		}
		return "可用上游账号正在冷却"
	case SelectionModelCooling:
		if prefix != "" {
			return prefix + "中可用账号的目标模型正在冷却"
		}
		return "可用上游账号的目标模型正在冷却"
	case SelectionQuotaExhausted:
		if prefix != "" {
			return prefix + "中的可用账号额度等待恢复"
		}
		return "可用上游账号额度等待恢复"
	case SelectionSaturated:
		if prefix != "" {
			return prefix + "中的可用账号均达到并发上限"
		}
		return "可用上游账号均达到并发上限"
	case SelectionPinnedUnavailable:
		if prefix != "" {
			return prefix + "中绑定的上游账号当前不可用"
		}
		return "绑定的上游账号当前不可用"
	default:
		if prefix != "" {
			return prefix + "当前没有可用上游账号"
		}
		return "没有可用上游账号"
	}
}

// HTTPStatus returns the client-facing status for a routing refusal.
func (e *SelectionUnavailableError) HTTPStatus() int {
	if e != nil {
		switch e.Reason {
		case SelectionCooling, SelectionModelCooling, SelectionQuotaExhausted:
			return http.StatusTooManyRequests
		}
	}
	return http.StatusServiceUnavailable
}

// Code returns the stable diagnostic code for a routing refusal.
func (e *SelectionUnavailableError) Code() string {
	if e != nil {
		switch e.Reason {
		case SelectionCooling:
			return "upstream_cooling"
		case SelectionModelCooling:
			return "upstream_model_cooling"
		case SelectionQuotaExhausted:
			return "upstream_quota_exhausted"
		case SelectionSaturated:
			return "upstream_saturated"
		case SelectionUnsupportedModel:
			return "upstream_model_unavailable"
		case SelectionPinnedUnavailable:
			return "upstream_pinned_account_unavailable"
		case SelectionNoAccounts:
			if e.Scope.IsRestricted() {
				return "client_key_account_scope_unavailable"
			}
		}
	}
	return "upstream_unavailable"
}

// Selector 实现可替换的 balanced 账号选择策略。
type Selector struct {
	accounts               repository.AccountRepository
	concurrency            repository.ConcurrencyLimiter
	sticky                 repository.StickySessionRepository
	stickyTTL              time.Duration
	cooldownBase           time.Duration
	cooldownMax            time.Duration
	capacityWait           time.Duration
	preferFreeBuild        bool
	excludeBuildBotFlagged bool
	segmentedConfig        segmentedSelectorConfig
	segmentedState         segmentedSelectorState
	configMu               sync.RWMutex
	candidateMu            sync.Mutex
	selectionMu            sync.RWMutex
	healthMu               sync.RWMutex
	quotaMu                sync.RWMutex
	staleLogMu             sync.Mutex
	logger                 *slog.Logger
	leaseWakeMu            sync.Mutex
	leaseWake              chan struct{}
	lastSelectedAt         map[uint64]time.Time
	lastSuccessAt          map[uint64]time.Time
	healthOverrides        map[uint64]routingHealthOverride
	quotaConsumed          map[quotaConsumptionKey]int
	staleFallbackLoggedAt  map[string]time.Time
	candidates             map[candidateCacheKey]candidateSnapshot
	routingBases           map[routingBaseCacheKey]routingBaseSnapshot
	routingOverlays        map[routingOverlayCacheKey]routingOverlaySnapshot
	routingAccountProvider map[uint64]account.Provider
	baseGlobalVersion      uint64
	overlayGlobalVersion   uint64
	baseProviderVersion    map[account.Provider]uint64
	overlayProviderVersion map[account.Provider]uint64
	candidateLoads         singleflight.Group
	concurrencySnapshots   *resultcache.Cache[[32]byte, map[string]int]
	tierOrders             interface {
		TierOrder(account.Provider, string) []account.WebTier
	}
}

func NewSelector(accounts repository.AccountRepository, concurrency repository.ConcurrencyLimiter, sticky repository.StickySessionRepository, tierOrders interface {
	TierOrder(account.Provider, string) []account.WebTier
}, stickyTTL, cooldownBase, cooldownMax time.Duration, capacityWait ...time.Duration) *Selector {
	wait := time.Duration(0)
	if len(capacityWait) > 0 && capacityWait[0] > 0 {
		wait = capacityWait[0]
	}
	return &Selector{accounts: accounts, concurrency: concurrency, sticky: sticky, tierOrders: tierOrders, stickyTTL: stickyTTL, cooldownBase: cooldownBase, cooldownMax: cooldownMax, capacityWait: wait, leaseWake: make(chan struct{}), logger: slog.Default(), lastSelectedAt: make(map[uint64]time.Time), lastSuccessAt: make(map[uint64]time.Time), healthOverrides: make(map[uint64]routingHealthOverride), quotaConsumed: make(map[quotaConsumptionKey]int), staleFallbackLoggedAt: make(map[string]time.Time), candidates: make(map[candidateCacheKey]candidateSnapshot), routingBases: make(map[routingBaseCacheKey]routingBaseSnapshot), routingOverlays: make(map[routingOverlayCacheKey]routingOverlaySnapshot), routingAccountProvider: make(map[uint64]account.Provider), baseProviderVersion: make(map[account.Provider]uint64), overlayProviderVersion: make(map[account.Provider]uint64), concurrencySnapshots: resultcache.New[[32]byte, map[string]int](maxConcurrencySnapshots, concurrencySnapshotTTL)}
}

// SetLogger wires the application logger into routing degradation diagnostics.
// It is intended to be called during startup before the selector serves traffic.
func (s *Selector) SetLogger(logger *slog.Logger) {
	if logger != nil {
		s.logger = logger
	}
}

func (s *Selector) UpdateConfig(stickyTTL, cooldownBase, cooldownMax time.Duration, capacityWait ...time.Duration) {
	s.configMu.Lock()
	s.stickyTTL = stickyTTL
	s.cooldownBase = cooldownBase
	s.cooldownMax = cooldownMax
	if len(capacityWait) > 0 {
		s.capacityWait = max(time.Duration(0), capacityWait[0])
	}
	s.configMu.Unlock()
}

// UpdatePreferFreeBuild 热更新 Build Free 账号优先策略。
func (s *Selector) UpdatePreferFreeBuild(value bool) {
	s.configMu.Lock()
	s.preferFreeBuild = value
	s.configMu.Unlock()
}

// UpdateSegmentedSelector changes the large-pool bounded planner policy.
func (s *Selector) UpdateSegmentedSelector(enabled bool, minCandidates, windowSize int) {
	s.configMu.Lock()
	s.segmentedConfig = normalizeSegmentedSelectorConfig(segmentedSelectorConfig{
		enabled: enabled, minCandidates: minCandidates, windowSize: windowSize,
	})
	s.configMu.Unlock()
}

func (s *Selector) routingConfig() (time.Duration, time.Duration, time.Duration, time.Duration) {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.stickyTTL, s.cooldownBase, s.cooldownMax, s.capacityWait
}

// UpdateExcludeBuildBotFlaggedFromScheduling toggles Build bot-risk exclusion from
// scheduling and invalidates Build candidate caches when the value changes.
func (s *Selector) UpdateExcludeBuildBotFlaggedFromScheduling(value bool) {
	s.configMu.Lock()
	changed := s.excludeBuildBotFlagged != value
	s.excludeBuildBotFlagged = value
	s.configMu.Unlock()
	if changed {
		s.invalidateCachedProviderCandidates()
	}
}

func (s *Selector) preferFreeBuildEnabled() bool {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.preferFreeBuild
}

func (s *Selector) excludeBuildBotFlaggedEnabled() bool {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.excludeBuildBotFlagged
}

// tierExempt asks the provider registry whether the provider ignores tier
// scopes; without a registry every provider is tier-scoped.
func (s *Selector) tierExempt(provider account.Provider) bool {
	if registry, ok := s.tierOrders.(interface{ TierExempt(account.Provider) bool }); ok {
		return registry.TierExempt(provider)
	}
	return false
}

// accountTier asks the provider registry for the candidate's client-key tier;
// without a registry every account is unknown.
func (s *Selector) accountTier(provider account.Provider, candidate account.RoutingCandidate) clientkeydomain.AccountTier {
	if registry, ok := s.tierOrders.(interface {
		AccountTier(account.Provider, account.RoutingCandidate) clientkeydomain.AccountTier
	}); ok {
		return registry.AccountTier(provider, candidate)
	}
	return clientkeydomain.AccountTierUnknown
}

func (s *Selector) accountScopeAllowsCandidate(provider account.Provider, scope clientkeydomain.AccountScope, candidate account.RoutingCandidate) bool {
	if scope.Tiers == clientkeydomain.TierScopeAll || s.tierExempt(provider) {
		return true
	}
	switch s.accountTier(provider, candidate) {
	case clientkeydomain.AccountTierFree:
		return scope.Tiers&clientkeydomain.TierScopeFree != 0
	case clientkeydomain.AccountTierSuper:
		return scope.Tiers&clientkeydomain.TierScopeSuper != 0
	default:
		return scope.Tiers&clientkeydomain.TierScopeUnknown != 0
	}
}

// effectiveQuotaMode returns the quota window the candidate spends: its loaded
// window, else the provider's account-specific mode for the route's mode.
func (s *Selector) effectiveQuotaMode(candidate account.RoutingCandidate, fallback string) string {
	if candidate.QuotaWindow != nil && candidate.QuotaWindow.Mode != "" {
		return candidate.QuotaWindow.Mode
	}
	if registry, ok := s.tierOrders.(interface {
		CandidateQuotaMode(account.Provider, account.RoutingCandidate, string) string
	}); ok {
		return registry.CandidateQuotaMode(candidate.Credential.Provider, candidate, fallback)
	}
	return fallback
}

func candidateEgressLeaseCooling(candidate account.RoutingCandidate, credential account.Credential, now time.Time) bool {
	block := candidate.EgressLeaseBlock
	return block != nil && block.AccountID == credential.ID && block.NodeID != 0 &&
		(credential.EgressNodeID == 0 || credential.EgressNodeID == block.NodeID) && now.Before(block.CooldownUntil)
}

// candidateSupportsModel treats a recognized static-catalog entry (a tier
// order) as an effective capability for tiers that the adapter explicitly
// allows. This prevents a historical capability snapshot from blocking a newly
// enabled catalog feature, while unknown/manual routes and remote-catalog
// providers retain the persisted snapshot semantics.
func (s *Selector) candidateSupportsModel(provider account.Provider, upstreamModel, quotaMode string, candidate account.RoutingCandidate) bool {
	if s.staticCatalog(provider) {
		if order := s.resolveTierOrder(provider, upstreamModel, quotaMode); len(order) > 0 {
			return webTierInOrder(order, candidate.Credential.WebTier)
		}
	}
	return !candidate.ModelCapabilityKnown || candidate.SupportsModel
}

// staticCatalog asks the provider registry whether the provider's models come
// from its built-in catalog, whose tier orders entitle accounts to a model.
func (s *Selector) staticCatalog(provider account.Provider) bool {
	registry, ok := s.tierOrders.(interface{ StaticCatalog(account.Provider) bool })
	return ok && registry.StaticCatalog(provider)
}

func earlierFuture(current, candidate, now time.Time) time.Time {
	if candidate.IsZero() || !now.Before(candidate) {
		return current
	}
	if current.IsZero() || candidate.Before(current) {
		return candidate
	}
	return current
}

func retryDelay(now, retryAt time.Time) time.Duration {
	if retryAt.IsZero() || !now.Before(retryAt) {
		return 0
	}
	return retryAt.Sub(now)
}

func (s *Selector) resolveTierOrder(provider account.Provider, upstreamModel, quotaMode string) []account.WebTier {
	if s.tierOrders == nil {
		return nil
	}
	if resolver, ok := s.tierOrders.(interface {
		TierOrderForQuotaMode(account.Provider, string, string) []account.WebTier
	}); ok {
		return resolver.TierOrderForQuotaMode(provider, upstreamModel, quotaMode)
	}
	return s.tierOrders.TierOrder(provider, upstreamModel)
}

func tierOrderRank(order []account.WebTier, tier account.WebTier) int {
	tier = normalizedRoutingWebTier(tier)
	for index, value := range order {
		if value == tier {
			return index
		}
	}
	return len(order)
}

func normalizedRoutingWebTier(tier account.WebTier) account.WebTier {
	if tier == "" || tier == account.WebTierAuto {
		return account.WebTierBasic
	}
	return tier
}

func webTierInOrder(order []account.WebTier, tier account.WebTier) bool {
	tier = normalizedRoutingWebTier(tier)
	for _, allowed := range order {
		if allowed == tier {
			return true
		}
	}
	return false
}
