package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// resolvePublicModelRoutes supports both unprefixed downstream model names and explicitly sourced compatibility names.
// Registered Provider aliases are stable compatibility contracts. allowModelAliases gates only dynamically generated
// reasoning-effort aliases so existing clients keep working after the per-key discovery switch is introduced.
func (s *Service) resolvePublicModelRoutes(ctx context.Context, publicModel string, allowModelAliases bool) ([]modeldomain.Route, string, error) {
	routes, err := s.models.GetByPublicIDCandidates(ctx, publicModel)
	if err == nil {
		return routes, "", nil
	}
	if s.providers != nil {
		if alias, ok := s.providers.ResolveModelAlias(publicModel); ok {
			if alias.Provider != "" && alias.UpstreamModel != "" {
				route, routeErr := s.models.GetByProviderUpstream(ctx, alias.Provider, alias.UpstreamModel)
				if routeErr != nil {
					return nil, "", routeErr
				}
				return []modeldomain.Route{route}, alias.ReasoningEffort, nil
			}
			routes, resolveErr := s.models.GetByPublicIDCandidates(ctx, alias.PublicModel)
			return routes, alias.ReasoningEffort, resolveErr
		}
	}
	// Dynamic effort-suffix aliases (e.g. grok-4.5-low) for any Provider that
	// exposes the base model. Fixed-reasoning Providers may compatibility-accept
	// an alias while their wire normalizer drops the unsupported effort.
	if base, effort, ok := modeldomain.ParseReasoningModelAlias(publicModel); ok {
		if !allowModelAliases {
			return nil, "", err
		}
		routes, resolveErr := s.models.GetByPublicIDCandidates(ctx, base)
		if resolveErr != nil {
			return nil, "", resolveErr
		}
		eligible := make([]modeldomain.Route, 0, len(routes))
		for _, route := range routes {
			if modeldomain.SupportsReasoningEffortForProvider(route.Provider, route.PublicID, effort) ||
				modeldomain.IsFixedReasoningForProvider(route.Provider, route.PublicID) {
				eligible = append(eligible, route)
			}
		}
		if len(eligible) == 0 {
			return nil, "", repository.ErrNotFound
		}
		return eligible, effort, nil
	}
	return nil, "", err
}

// eligibleConversationRoutes filters route targets without choosing one. Keeping
// this separate from ordering lets one public name form a schedulable target pool.
func (s *Service) eligibleConversationRoutes(routes []modeldomain.Route, key clientkey.Key, operation audit.Operation, path string, requireStoredResponse bool, ownership *inferencedomain.ResponseOwnership) ([]modeldomain.Route, modeldomain.Route, error) {
	if len(routes) == 0 || s.providers == nil {
		return nil, modeldomain.Route{}, ErrModelNotFound
	}
	fallback := routes[0]
	eligible := make([]modeldomain.Route, 0, len(routes))
	accountScope := key.AccountScope()
	matchedOwnership := ownership == nil
	scopeMatched := false
	allowed := false
	conversationSupported := false
	storedResponseUnsupported := false
	for _, route := range routes {
		if ownership != nil {
			if ownership.ModelRouteID != 0 {
				if route.ID != ownership.ModelRouteID {
					continue
				}
			} else if route.Provider != ownership.Provider {
				// Backward compatibility for ownership rows created before route IDs
				// were persisted: retain the original Provider-scoped pin.
				continue
			}
		}
		matchedOwnership = true
		fallback = route
		if !accountScope.AllowsProvider(route.Provider) {
			continue
		}
		scopeMatched = true
		if !s.clientKeys.CanUseModel(key, route.ID) {
			continue
		}
		allowed = true
		if !s.providers.SupportsConversation(route.Provider, string(operation)) {
			continue
		}
		conversationSupported = true
		if path == "/responses/compact" && !s.providers.SupportsResponseCompaction(route.Provider) {
			continue
		}
		if requireStoredResponse && !s.providers.SupportsStoredResponses(route.Provider) {
			storedResponseUnsupported = true
			continue
		}
		eligible = append(eligible, route)
	}
	if len(eligible) > 0 {
		return eligible, fallback, nil
	}
	if !matchedOwnership {
		return nil, fallback, ErrResponseAccountUnavailable
	}
	if !scopeMatched {
		return nil, fallback, &SelectionUnavailableError{Reason: SelectionNoAccounts, Scope: accountScope}
	}
	if !allowed {
		return nil, fallback, clientkeyapp.ErrModelNotAllowed
	}
	if storedResponseUnsupported {
		return nil, fallback, ErrResponseStateUnsupported
	}
	if conversationSupported && path == "/responses/compact" {
		return nil, fallback, ErrConversationUnsupported
	}
	return nil, fallback, ErrConversationUnsupported
}

// selectConversationRoute retains the legacy single-target helper for callers
// that do not need target-pool ordering.
func (s *Service) selectConversationRoute(routes []modeldomain.Route, key clientkey.Key, operation audit.Operation, path string, requireStoredResponse bool, ownership *inferencedomain.ResponseOwnership) (modeldomain.Route, error) {
	eligible, fallback, err := s.eligibleConversationRoutes(routes, key, operation, path, requireStoredResponse, ownership)
	if err != nil {
		return fallback, err
	}
	return eligible[0], nil
}

// orderConversationRouteTargets randomizes targets within the same Provider by
// rendezvous score. Provider priority remains stable, while a session seed keeps
// Codex/Claude continuations on the same target without global mutable state.
func orderConversationRouteTargets(routes []modeldomain.Route, seed string) []modeldomain.Route {
	ordered := append([]modeldomain.Route(nil), routes...)
	sort.SliceStable(ordered, func(left, right int) bool {
		leftPriority := ordered[left].Provider.Order()
		rightPriority := ordered[right].Provider.Order()
		if leftPriority != rightPriority {
			return leftPriority < rightPriority
		}
		leftScore := routeTargetScore(seed, ordered[left].ID)
		rightScore := routeTargetScore(seed, ordered[right].ID)
		if leftScore != rightScore {
			return leftScore > rightScore
		}
		return ordered[left].ID < ordered[right].ID
	})
	return ordered
}

func routeTargetScore(seed string, routeID uint64) uint64 {
	digest := sha256.Sum256([]byte(seed + ":" + strconv.FormatUint(routeID, 10)))
	return binary.BigEndian.Uint64(digest[:8])
}

func routeTargetSeed(input Input) string {
	// Match the Build account-affinity precedence so Codex and Claude Code keep
	// both the route target and account stable across one logical session.
	anchor := strings.TrimSpace(input.PromptCacheSeed)
	if anchor == "" {
		anchor = strings.TrimSpace(input.PromptCacheKey)
	}
	if anchor == "" {
		system, firstUser, _ := extractMessageAnchors(input.Body)
		system = truncateAnchor(system, 100)
		firstUser = truncateAnchor(firstUser, 200)
		if firstUser != "" {
			anchor = "soft:" + system + ":" + firstUser
		}
	}
	if anchor == "" {
		anchor = strings.TrimSpace(input.RequestID)
	}
	return strconv.FormatUint(input.ClientKey.ID, 10) + ":" + anchor
}

// selectMediaRoute selects a same-name route that satisfies media capability, key permissions, and Provider support.
func (s *Service) selectMediaRoute(routes []modeldomain.Route, key clientkey.Key, capability modeldomain.Capability, providerSupported func(accountdomain.Provider) bool) (modeldomain.Route, error) {
	eligible, fallback, err := s.eligibleMediaRoutes(routes, key, capability, providerSupported)
	if err != nil {
		return fallback, err
	}
	return eligible[0], nil
}

func (s *Service) eligibleMediaRoutes(routes []modeldomain.Route, key clientkey.Key, capability modeldomain.Capability, providerSupported func(accountdomain.Provider) bool) ([]modeldomain.Route, modeldomain.Route, error) {
	if len(routes) == 0 {
		return nil, modeldomain.Route{}, ErrModelNotFound
	}
	fallback := routes[0]
	eligible := make([]modeldomain.Route, 0, len(routes))
	accountScope := key.AccountScope()
	capabilityMatched := false
	scopeMatched := false
	allowed := false
	for _, route := range routes {
		if route.Capability != capability {
			continue
		}
		fallback = route
		capabilityMatched = true
		if !accountScope.AllowsProvider(route.Provider) {
			continue
		}
		scopeMatched = true
		if !s.clientKeys.CanUseModel(key, route.ID) {
			continue
		}
		allowed = true
		if providerSupported(route.Provider) {
			eligible = append(eligible, route)
		}
	}
	if len(eligible) > 0 {
		return eligible, fallback, nil
	}
	if !capabilityMatched {
		return nil, fallback, ErrModelNotFound
	}
	if !scopeMatched {
		return nil, fallback, &SelectionUnavailableError{Reason: SelectionNoAccounts, Scope: accountScope}
	}
	if !allowed {
		return nil, fallback, clientkeyapp.ErrModelNotAllowed
	}
	return nil, fallback, ErrNoAvailableAccount
}

// selectSchedulableMediaRoute resolves a concrete same-name media target and
// its immutable account plan together. A cooling or exhausted first target
// therefore cannot hide a healthy target from another Provider.
func (s *Service) selectSchedulableMediaRoute(ctx context.Context, routes []modeldomain.Route, key clientkey.Key, capability modeldomain.Capability, consumesQuota bool, providerSupported func(accountdomain.Provider) bool) (modeldomain.Route, *selectionSession, error) {
	return s.selectSchedulableMediaRouteWithQuotaMode(ctx, routes, key, capability, consumesQuota, providerSupported, nil)
}

func (s *Service) selectSchedulableMediaRouteWithQuotaMode(ctx context.Context, routes []modeldomain.Route, key clientkey.Key, capability modeldomain.Capability, consumesQuota bool, providerSupported func(accountdomain.Provider) bool, resolveQuotaMode func(modeldomain.Route) string) (modeldomain.Route, *selectionSession, error) {
	eligible, fallback, err := s.eligibleMediaRoutes(routes, key, capability, providerSupported)
	if err != nil {
		return fallback, nil, err
	}
	return s.selectSchedulableEligibleMediaRouteWithQuotaMode(ctx, eligible, key, consumesQuota, resolveQuotaMode)
}

// selectSchedulableEligibleMediaRouteWithQuotaMode selects an account plan
// from routes that already passed capability, client-key, and Provider support
// checks. Callers may apply request-specific route constraints between the
// eligibility and scheduling phases without evaluating disallowed routes.
func (s *Service) selectSchedulableEligibleMediaRouteWithQuotaMode(ctx context.Context, eligible []modeldomain.Route, key clientkey.Key, consumesQuota bool, resolveQuotaMode func(modeldomain.Route) string) (modeldomain.Route, *selectionSession, error) {
	if len(eligible) == 0 {
		return modeldomain.Route{}, nil, ErrNoAvailableAccount
	}
	var firstSelectionErr error
	for _, route := range eligible {
		quotaMode := ""
		if consumesQuota {
			if resolveQuotaMode != nil {
				quotaMode = resolveQuotaMode(route)
			} else {
				quotaMode = s.providers.QuotaMode(route.Provider, route.UpstreamModel)
			}
		}
		session, selectionErr := s.selector.beginSelectionSessionForKey(
			ctx,
			route.Provider,
			route.ID,
			route.UpstreamModel,
			quotaMode,
			"",
			nil,
			false,
			key.AccountScope(),
		)
		if selectionErr == nil {
			return route, session, nil
		}
		if firstSelectionErr == nil {
			firstSelectionErr = selectionErr
		}
	}
	if firstSelectionErr == nil {
		firstSelectionErr = ErrNoAvailableAccount
	}
	return eligible[0], nil, firstSelectionErr
}

func rewriteAliasedModel(body []byte, publicModel, reasoningEffort string, operation audit.Operation) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("解析兼容模型请求: %w", err)
	}
	payload["model"] = publicModel
	if reasoningEffort != "" {
		switch operation {
		case audit.OperationChat:
			payload["reasoning_effort"] = reasoningEffort
		case audit.OperationMessages:
			config, _ := payload["output_config"].(map[string]any)
			if reasoningEffort == modeldomain.ReasoningEffortNone {
				if config != nil {
					delete(config, "effort")
				}
				if len(config) == 0 {
					delete(payload, "output_config")
				} else {
					payload["output_config"] = config
				}
				payload["thinking"] = map[string]any{"type": "disabled"}
				break
			}
			if config == nil {
				config = make(map[string]any)
			}
			config["effort"] = reasoningEffort
			payload["output_config"] = config
			payload["thinking"] = map[string]any{"type": "adaptive"}
		default:
			reasoning, _ := payload["reasoning"].(map[string]any)
			if reasoning == nil {
				reasoning = make(map[string]any)
			}
			reasoning["effort"] = reasoningEffort
			payload["reasoning"] = reasoning
		}
	}
	return json.Marshal(payload)
}
