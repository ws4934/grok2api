package egress

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	application "github.com/chenyme/grok2api/backend/internal/application/egress"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	neterrorpkg "github.com/chenyme/grok2api/backend/internal/pkg/neterror"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func (m *Manager) inflightCounter(nodeID uint64) *atomic.Int64 {
	// Counters remain address-stable for the manager lifetime so a concurrent
	// release can never decrement a replacement counter after an ABA deletion.
	if value, ok := m.inflight.Load(nodeID); ok {
		return value.(*atomic.Int64)
	}
	candidate := &atomic.Int64{}
	actual, _ := m.inflight.LoadOrStore(nodeID, candidate)
	return actual.(*atomic.Int64)
}

func (m *Manager) incrementInflight(nodeID uint64) {
	m.inflightCounter(nodeID).Add(1)
}

func (m *Manager) decrementInflight(nodeID uint64) {
	if value, ok := m.inflight.Load(nodeID); ok {
		value.(*atomic.Int64).Add(-1)
	}
}

func (m *Manager) listNodes(ctx context.Context, scope domain.Scope, now time.Time) ([]domain.Node, error) {
	for {
		m.nodeMu.RLock()
		if snapshot, ok := m.nodes[scope]; ok && now.Before(snapshot.expiresAt) {
			// Node snapshots are replaced with a copied slice and treated as immutable
			// by callers. Returning the shared read-only slice avoids a per-request copy.
			values := snapshot.values
			m.nodeMu.RUnlock()
			return values, nil
		}
		m.nodeMu.RUnlock()
		loaded, err, _ := m.nodeLoads.Do(string(scope), func() (any, error) {
			checkTime := time.Now().UTC()
			m.nodeMu.RLock()
			if snapshot, ok := m.nodes[scope]; ok && checkTime.Before(snapshot.expiresAt) {
				values := snapshot.values
				m.nodeMu.RUnlock()
				return values, nil
			}
			version := m.nodeVersions[scope]
			m.nodeMu.RUnlock()
			values, err := m.repository.ListEgressNodes(ctx, scope, repository.SortQuery{})
			if err != nil {
				return nil, err
			}
			m.nodeMu.Lock()
			if m.nodeVersions[scope] != version {
				m.nodeMu.Unlock()
				return nil, errNodeSnapshotInvalidated
			}
			m.replaceNodeSnapshotLocked(scope, values, checkTime.Add(nodeSnapshotTTL))
			values = m.nodes[scope].values
			m.nodeMu.Unlock()
			return values, nil
		})
		if err != nil {
			if errors.Is(err, errNodeSnapshotInvalidated) && ctx.Err() == nil {
				now = time.Now().UTC()
				continue
			}
			return nil, err
		}
		return loaded.([]domain.Node), nil
	}
}

func (m *Manager) invalidateNodes(scope domain.Scope) {
	m.nodeMu.Lock()
	m.nodeVersions[scope]++
	snapshot, ok := m.nodes[scope]
	if ok {
		delete(m.nodes, scope)
	}
	for _, node := range snapshot.values {
		delete(m.healthyNodes, node.ID)
	}
	m.nodeMu.Unlock()
}

func (m *Manager) replaceNodeSnapshotLocked(scope domain.Scope, values []domain.Node, expiresAt time.Time) {
	for _, node := range m.nodes[scope].values {
		delete(m.healthyNodes, node.ID)
	}
	for _, node := range values {
		if nodeIsHealthy(node) {
			m.healthyNodes[node.ID] = expiresAt
		} else {
			delete(m.healthyNodes, node.ID)
		}
	}
	m.nodes[scope] = cachedNodeSnapshot{values: append([]domain.Node(nil), values...), expiresAt: expiresAt}
}

func fallbackScopes(scope domain.Scope) []domain.Scope {
	if scope == domain.ScopeWebAsset {
		return []domain.Scope{domain.ScopeWebAsset, domain.ScopeWeb}
	}
	if scope == domain.ScopeConsoleAsset {
		return []domain.Scope{domain.ScopeConsoleAsset, domain.ScopeConsole, domain.ScopeWeb}
	}
	if scope == domain.ScopeConsole {
		// Console uses the same browser/clearance surface as Grok Web. A
		// dedicated Console node is preferred, but a Web node is a safe and
		// expected fallback for deployments that configure one shared pool.
		return []domain.Scope{domain.ScopeConsole, domain.ScopeWeb}
	}
	return []domain.Scope{scope}
}

func (m *Manager) selectNode(nodes []domain.Node, affinity string) domain.Node {
	if affinity != "" {
		digest := sha256.Sum256([]byte(affinity))
		selected := nodes[int(binary.BigEndian.Uint64(digest[:8])%uint64(len(nodes)))]
		if selected.Health >= 0.8 || len(nodes) == 1 {
			return selected
		}
		for _, node := range nodes {
			if node.Health > selected.Health {
				selected = node
			}
		}
		return selected
	}
	best := nodes[0]
	bestCurrent := m.inflightCount(best.ID)
	for _, node := range nodes[1:] {
		current := m.inflightCount(node.ID)
		if current < bestCurrent || (current == bestCurrent && node.Health > best.Health) {
			best = node
			bestCurrent = current
		}
	}
	return best
}

func (m *Manager) inflightCount(nodeID uint64) int64 {
	if value, ok := m.inflight.Load(nodeID); ok {
		return value.(*atomic.Int64).Load()
	}
	return 0
}

func (m *Manager) Feedback(ctx context.Context, nodeID uint64, status int, transportErr error) {
	m.FeedbackForScope(ctx, domain.ScopeWeb, nodeID, status, transportErr)
}

func (m *Manager) FeedbackForScope(ctx context.Context, scope domain.Scope, nodeID uint64, status int, transportErr error) {
	if status == clientClosedRequestStatus || errors.Is(transportErr, context.Canceled) {
		return
	}
	// Console media hosts are public and do not use clearance credentials. A
	// 403 there commonly describes the object URL (expired, rejected, or
	// missing), not the proxy's ability to reach the origin, so it must not cool
	// or rotate an otherwise healthy primary Console node.
	if scope == domain.ScopeConsoleAsset && transportErr == nil && status == http.StatusForbidden {
		return
	}
	if neterrorpkg.IsUpstreamStreamIdleTimeout(transportErr) {
		return
	}
	if scope == domain.ScopeBuild && neterrorpkg.IsResponseHeaderTimeout(transportErr) {
		return
	}
	if nodeID == 0 {
		if transportErr != nil || (scope != domain.ScopeBuild && status == http.StatusForbidden) {
			m.clearanceMu.Lock()
			if isGrokWebScope(scope) && status == http.StatusForbidden && m.clearanceConfig.Mode == "flaresolverr" {
				state := m.clearances["direct"]
				state.invalid = true
				state.used = true
				m.clearances["direct"] = state
			}
			m.clearanceMu.Unlock()
			m.clientMu.Lock()
			stale := m.invalidateClientForScopeLocked(0, scope)
			m.clientMu.Unlock()
			closeRequestClients(stale)
		}
		return
	}
	succeeded := transportErr == nil && status >= 200 && status < 400
	if succeeded && m.cachedNodeIsHealthy(nodeID) {
		return
	}
	value, err := m.repository.GetEgressNode(ctx, nodeID)
	if err != nil {
		return
	}
	if succeeded && nodeIsHealthy(value) {
		m.nodeMu.Lock()
		m.healthyNodes[nodeID] = time.Now().UTC().Add(nodeSnapshotTTL)
		m.nodeMu.Unlock()
		return
	}
	now := time.Now().UTC()
	var stale []requestClient
	switch {
	case succeeded:
		value.Health = min(1, value.Health+0.1)
		value.FailureCount = 0
		value.CooldownUntil = nil
		value.LastError = ""
	case status == http.StatusUnauthorized || status == http.StatusTooManyRequests:
		return
	case scope == domain.ScopeBuild && status == http.StatusForbidden:
		// Build 403 may indicate account permissions, quota, token, or egress policy. The gateway classifies the body;
		// status alone must not misclassify standard CLI egress as Web anti-bot behavior.
		return
	case scope == domain.ScopeBuild && status == http.StatusBadRequest:
		// Device OAuth polls with 400 + authorization_pending before user confirmation.
		// This is a normal protocol state and must not cool the egress node.
		return
	case status == http.StatusForbidden:
		if m.isProxyPoolNode(value) {
			// A request-level 403 does not prove that a shared proxy pool is unhealthy.
			return
		}
		value.FailureCount++
		value.Health = max(0.05, value.Health*0.7)
		value.CooldownUntil = nil
		value.LastError = "anti-bot rejection"
		m.clearanceMu.Lock()
		if isGrokWebScope(scope) && m.clearanceConfig.Mode == "flaresolverr" {
			key := clearanceCacheKey(nodeID, "", false)
			state := m.clearances[key]
			state.invalid = true
			state.used = true
			m.clearances[key] = state
		}
		m.clearanceMu.Unlock()
		m.clientMu.Lock()
		stale = m.invalidateClientLocked(nodeID)
		m.clientMu.Unlock()
	case transportErr != nil:
		if m.isProxyPoolNode(value) {
			return
		}
		value.FailureCount++
		value.Health = max(0.05, value.Health*0.7)
		cooldown := min(10*time.Minute, 30*time.Second*time.Duration(1<<min(value.FailureCount-1, 4)))
		until := now.Add(cooldown)
		value.CooldownUntil = &until
		value.LastError = domain.LastErrorTransport
		m.clientMu.Lock()
		stale = m.invalidateClientLocked(nodeID)
		m.clientMu.Unlock()
	default:
		// An HTTP status describes the upstream response, not the health of the
		// configured proxy endpoint. Account routing handles upstream failures.
		return
	}
	closeRequestClients(stale)
	if stateRepository, ok := m.repository.(egressStateRepository); ok {
		if err := stateRepository.UpdateEgressNodeHealth(ctx, value.ID, value.Health, value.FailureCount, value.CooldownUntil, value.LastError); err == nil {
			m.invalidateNodes(value.Scope)
			if transportErr != nil {
				m.scheduleFailureProbe(value)
			}
		}
		return
	}
	if _, err := m.repository.UpdateEgressNode(ctx, value); err == nil {
		m.invalidateNodes(value.Scope)
		if transportErr != nil {
			m.scheduleFailureProbe(value)
		}
	}
}

func (m *Manager) cachedNodeIsHealthy(nodeID uint64) bool {
	m.nodeMu.Lock()
	validUntil, ok := m.healthyNodes[nodeID]
	healthy := ok && time.Now().UTC().Before(validUntil)
	if ok && !healthy {
		delete(m.healthyNodes, nodeID)
	}
	m.nodeMu.Unlock()
	return healthy
}

func nodeIsHealthy(value domain.Node) bool {
	return value.Health >= 1 && value.FailureCount == 0 && value.CooldownUntil == nil && value.LastError == ""
}

func (m *Manager) isStickyProxyNode(value domain.Node) bool {
	if m == nil || m.cipher == nil || strings.TrimSpace(value.EncryptedProxyURL) == "" {
		return false
	}
	proxyURL, err := m.cipher.Decrypt(value.EncryptedProxyURL)
	return err == nil && strings.Contains(proxyURL, application.ProxyAccountPlaceholder)
}

func (m *Manager) isProxyPoolNode(value domain.Node) bool {
	return value.ProxyPool || m.isStickyProxyNode(value)
}
