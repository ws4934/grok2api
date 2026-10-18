package egress

import (
	"strings"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// SetExtensionClearance enables on-demand Cloudflare clearance for an extension
// scope against target (an empty target disables it). The scope reuses the
// global FlareSolverr endpoint, timeout and refresh interval, and only solves
// after a lease reported a challenge, whatever the Grok clearance mode is.
func (m *Manager) SetExtensionClearance(scope domain.Scope, target string) {
	m.clearanceMu.Lock()
	defer m.clearanceMu.Unlock()
	if m.extensionClearance == nil {
		m.extensionClearance = make(map[domain.Scope]string)
	}
	target = strings.TrimRight(strings.TrimSpace(target), "/")
	if target == "" {
		delete(m.extensionClearance, scope)
		return
	}
	m.extensionClearance[scope] = target
}

// ClearanceEnabled reports whether leases for scope carry managed clearance.
func (m *Manager) ClearanceEnabled(scope domain.Scope) bool {
	m.clearanceMu.Lock()
	defer m.clearanceMu.Unlock()
	_, enabled := m.clearanceForLocked(scope)
	return enabled
}

// clearanceForLocked returns the clearance configuration that applies to scope
// and whether it is managed. Call with clearanceMu held.
func (m *Manager) clearanceForLocked(scope domain.Scope) (ClearanceConfig, bool) {
	cfg := m.clearanceConfig
	if isGrokWebScope(scope) {
		return cfg, cfg.Mode == "flaresolverr" || cfg.Mode == "on_demand"
	}
	target := m.extensionClearance[scope]
	if target == "" || strings.TrimSpace(cfg.FlareSolverrURL) == "" {
		return cfg, false
	}
	cfg.Mode, cfg.TargetURL = "on_demand", target
	return cfg, true
}

// scopedClearanceKey keeps a direct extension clearance apart from the Grok
// direct clearance; node keys are already unique because nodes have one scope.
func scopedClearanceKey(scope domain.Scope, nodeID uint64, proxyURL string, sticky bool) string {
	key := clearanceCacheKey(nodeID, proxyURL, sticky)
	if nodeID == 0 && !isGrokWebScope(scope) {
		return key + ":" + string(scope)
	}
	return key
}
