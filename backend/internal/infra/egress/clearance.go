package egress

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	application "github.com/chenyme/grok2api/backend/internal/application/egress"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/pkg/flight"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// SetClearanceLock enables cross-instance coordination for shared, fixed egress
// nodes. Account-bound Resin clearances remain process-local because they must
// never be persisted into the node-wide cookie fields.
func (m *Manager) SetClearanceLock(value repository.DistributedLock) {
	m.clearanceMu.Lock()
	m.clearanceLock = value
	m.clearanceMu.Unlock()
}

func (m *Manager) UpdateClearanceConfig(value ClearanceConfig) {
	value.Mode = strings.TrimSpace(value.Mode)
	value.FlareSolverrURL = strings.TrimSpace(value.FlareSolverrURL)
	value.TargetURL = strings.TrimRight(strings.TrimSpace(value.TargetURL), "/")
	m.clearanceMu.Lock()
	previous := m.clearanceConfig
	m.clearanceConfig = value
	configurationChanged := previous.Mode != value.Mode || previous.FlareSolverrURL != value.FlareSolverrURL || previous.TargetURL != value.TargetURL
	if configurationChanged {
		m.clearanceVersion++
		m.clientMu.Lock()
		m.invalidateAllClientVersionsLocked()
		m.clientMu.Unlock()
	}
	m.clearanceMu.Unlock()
}

func clearanceCacheKey(nodeID uint64, proxyURL string, sticky bool) string {
	if nodeID == 0 {
		return "direct"
	}
	base := "node:" + strconv.FormatUint(nodeID, 10)
	if !sticky {
		return base
	}
	digest := sha256.Sum256([]byte(proxyURL))
	return base + ":account:" + fmt.Sprintf("%x", digest[:16])
}

func (m *Manager) ensureClearance(ctx context.Context, node domain.Node, proxyURL, existingCookies, existingUserAgent, key string, persist bool) (string, string, error) {
	m.clearanceMu.Lock()
	cfg, _ := m.clearanceForLocked(node.Scope)
	version := m.clearanceVersion
	interval := clearanceRefreshInterval(cfg)
	now := time.Now().UTC()
	fingerprint := clearanceFingerprint(cfg, proxyURL)
	bindingFingerprint := clearanceBindingFingerprint(cfg, proxyURL)
	m.cleanupClearanceCacheLocked(now, interval)
	state, known := m.clearances[key]
	if strings.HasPrefix(key, "direct") {
		if !known {
			m.ensureClearanceCacheCapacityLocked()
		}
		state.used = true
		m.clearances[key] = state
	}
	if (!known || state.userAgent == "") && persist && (existingCookies != "" || node.ClearanceRefreshedAt != nil) {
		if !known {
			m.ensureClearanceCacheCapacityLocked()
		}
		state = clearanceState{
			cookies: existingCookies, userAgent: existingUserAgent, used: true, version: version,
			fingerprint: node.ClearanceFingerprint, bindingFingerprint: node.ClearanceBindingFingerprint,
			lastUsedAt: now,
		}
		if node.ClearanceRefreshedAt != nil {
			state.refreshedAt = *node.ClearanceRefreshedAt
		}
		known = true
		m.clearances[key] = state
	}
	// A successful solve may legitimately return no Cloudflare cookies when the
	// selected egress does not trigger a challenge. The solver User-Agent marks
	// that cookie-less result as complete so requests do not block on re-solving.
	fresh := known && !state.invalid && state.userAgent != "" && state.version == version &&
		state.fingerprint == fingerprint && (state.bindingFingerprint == "" || state.bindingFingerprint == bindingFingerprint) &&
		!state.refreshedAt.IsZero() && now.Sub(state.refreshedAt) < interval
	if fresh {
		state.lastUsedAt = now
		m.clearances[key] = state
		cookies, userAgent := state.cookies, state.userAgent
		m.clearanceMu.Unlock()
		return cookies, userAgent, nil
	}
	fallbackAllowed := known && !state.invalid && state.userAgent != "" &&
		(state.bindingFingerprint == "" || state.bindingFingerprint == bindingFingerprint)
	fallback := clearanceSolution{Cookies: state.cookies, UserAgent: state.userAgent}
	forceRefresh := known && state.invalid
	refreshAfter := time.Time{}
	if forceRefresh {
		refreshAfter = state.refreshedAt
	}
	if fallbackAllowed {
		state.lastUsedAt = now
		m.clearances[key] = state
	}
	if cfg.Mode == "on_demand" && !forceRefresh {
		m.clearanceMu.Unlock()
		if fallbackAllowed {
			return fallback.Cookies, fallback.UserAgent, nil
		}
		return existingCookies, existingUserAgent, nil
	}
	if cfg.Mode != "flaresolverr" && cfg.Mode != "on_demand" {
		m.clearanceMu.Unlock()
		return existingCookies, existingUserAgent, nil
	}
	m.clearanceMu.Unlock()

	solution, err := m.refreshShared(ctx, node, proxyURL, key, persist, forceRefresh, !fallbackAllowed, refreshAfter)
	if err != nil {
		if fallbackAllowed {
			return fallback.Cookies, fallback.UserAgent, nil
		}
		return "", "", err
	}
	return solution.Cookies, solution.UserAgent, nil
}

// refreshShared runs one refreshNode per key for all concurrent callers,
// bounded by the clearance lock lease the refresh may hold.
func (m *Manager) refreshShared(ctx context.Context, node domain.Node, proxyURL, key string, persist, force, waitForPeer bool, refreshAfter time.Time) (clearanceSolution, error) {
	m.clearanceMu.Lock()
	cfg, _ := m.clearanceForLocked(node.Scope)
	m.clearanceMu.Unlock()
	return flight.Do(ctx, &m.clearanceLoads, key, clearanceSolveTimeout(cfg)+clearanceLockGrace, func(ctx context.Context) (clearanceSolution, error) {
		return m.refreshNode(ctx, node, proxyURL, key, persist, force, waitForPeer, refreshAfter)
	})
}

func clearanceSolveTimeout(cfg ClearanceConfig) time.Duration {
	if cfg.Timeout <= 0 {
		return time.Minute
	}
	return cfg.Timeout
}

func (m *Manager) refreshNode(ctx context.Context, node domain.Node, proxyURL, key string, persist, force, waitForPeer bool, refreshAfter time.Time) (clearanceSolution, error) {
	m.clearanceMu.Lock()
	cfg, _ := m.clearanceForLocked(node.Scope)
	solveVersion := m.clearanceVersion
	solver := m.solver
	lock := m.clearanceLock
	m.clearanceMu.Unlock()
	if cfg.Mode != "flaresolverr" && cfg.Mode != "on_demand" {
		return clearanceSolution{}, errors.New("FlareSolverr Clearance 未启用")
	}
	timeout := clearanceSolveTimeout(cfg)
	fingerprint := clearanceFingerprint(cfg, proxyURL)
	bindingFingerprint := clearanceBindingFingerprint(cfg, proxyURL)
	interval := clearanceRefreshInterval(cfg)
	if persist && node.ID != 0 && lock != nil {
		release, acquired, err := lock.Acquire(ctx, "egress-clearance:"+strconv.FormatUint(node.ID, 10), timeout+clearanceLockGrace)
		if err != nil {
			return clearanceSolution{}, fmt.Errorf("协调 Clearance 刷新: %w", err)
		}
		if !acquired {
			if !force {
				if solution, refreshedAt, ok := m.loadPersistedClearance(ctx, node.ID, fingerprint, bindingFingerprint, interval); ok {
					m.cacheClearance(key, solution, refreshedAt, solveVersion, fingerprint, bindingFingerprint, interval)
					return solution, nil
				}
			}
			if waitForPeer {
				if solution, refreshedAt, ok := m.waitPersistedClearance(ctx, node.ID, fingerprint, bindingFingerprint, interval, timeout, refreshAfter); ok {
					m.cacheClearance(key, solution, refreshedAt, solveVersion, fingerprint, bindingFingerprint, interval)
					return solution, nil
				}
			}
			return clearanceSolution{}, errors.New("另一个实例正在刷新 Cloudflare Clearance")
		}
		defer release()
		if solution, refreshedAt, ok := m.loadPersistedClearance(ctx, node.ID, fingerprint, bindingFingerprint, interval); ok {
			// A peer may have refreshed the rejected Clearance immediately before
			// this instance acquired the distributed lock. Reuse that newer result
			// instead of performing a duplicate browser solve. A force refresh with
			// no newer persisted generation must still reach the solver.
			if !force || (!refreshAfter.IsZero() && refreshedAt.After(refreshAfter)) {
				m.cacheClearance(key, solution, refreshedAt, solveVersion, fingerprint, bindingFingerprint, interval)
				return solution, nil
			}
		}
	}
	solveCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	solution, err := solver.Solve(solveCtx, cfg, proxyURL)
	if err != nil {
		m.recordClearanceError(ctx, node, persist)
		return clearanceSolution{}, fmt.Errorf("刷新出口 %q 的 Cloudflare Clearance: %w", node.Name, err)
	}
	now := time.Now().UTC()
	if persist && node.ID != 0 {
		encryptedCookies, encryptErr := m.cipher.Encrypt(solution.Cookies)
		if encryptErr != nil {
			return clearanceSolution{}, encryptErr
		}
		if stateRepository, ok := m.repository.(egressStateRepository); ok {
			if updateErr := stateRepository.UpdateEgressNodeClearance(ctx, node.ID, encryptedCookies, solution.UserAgent, fingerprint, bindingFingerprint, now); updateErr != nil {
				return clearanceSolution{}, updateErr
			}
		} else {
			latest, loadErr := m.repository.GetEgressNode(ctx, node.ID)
			if loadErr != nil {
				return clearanceSolution{}, loadErr
			}
			latest.EncryptedCloudflareCookie = encryptedCookies
			latest.UserAgent = solution.UserAgent
			latest.ClearanceFingerprint = fingerprint
			latest.ClearanceBindingFingerprint = bindingFingerprint
			latest.ClearanceRefreshedAt = &now
			latest.LastError = ""
			if _, updateErr := m.repository.UpdateEgressNode(ctx, latest); updateErr != nil {
				return clearanceSolution{}, updateErr
			}
		}
		m.invalidateNodes(node.Scope)
	}
	m.cacheClearance(key, solution, now, solveVersion, fingerprint, bindingFingerprint, interval)
	return solution, nil
}

func (m *Manager) waitPersistedClearance(ctx context.Context, nodeID uint64, fingerprint, bindingFingerprint string, interval, timeout time.Duration, refreshAfter time.Time) (clearanceSolution, time.Time, bool) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-waitCtx.Done():
			return clearanceSolution{}, time.Time{}, false
		case <-ticker.C:
			if solution, refreshedAt, ok := m.loadPersistedClearance(waitCtx, nodeID, fingerprint, bindingFingerprint, interval); ok &&
				(refreshAfter.IsZero() || refreshedAt.After(refreshAfter)) {
				return solution, refreshedAt, true
			}
		}
	}
}

func (m *Manager) loadPersistedClearance(ctx context.Context, nodeID uint64, fingerprint, bindingFingerprint string, interval time.Duration) (clearanceSolution, time.Time, bool) {
	latest, err := m.repository.GetEgressNode(ctx, nodeID)
	if err != nil || latest.ClearanceRefreshedAt == nil || latest.ClearanceFingerprint != fingerprint ||
		(latest.ClearanceBindingFingerprint != "" && latest.ClearanceBindingFingerprint != bindingFingerprint) ||
		time.Since(*latest.ClearanceRefreshedAt) >= interval {
		return clearanceSolution{}, time.Time{}, false
	}
	cookies, err := m.cipher.Decrypt(latest.EncryptedCloudflareCookie)
	if err != nil {
		return clearanceSolution{}, time.Time{}, false
	}
	cookies = application.SanitizeCloudflareCookies(cookies)
	userAgent := strings.TrimSpace(latest.UserAgent)
	if userAgent == "" {
		return clearanceSolution{}, time.Time{}, false
	}
	return clearanceSolution{Cookies: cookies, UserAgent: userAgent}, *latest.ClearanceRefreshedAt, true
}

func (m *Manager) cacheClearance(key string, solution clearanceSolution, refreshedAt time.Time, version uint64, fingerprint, bindingFingerprint string, interval time.Duration) {
	m.clearanceMu.Lock()
	now := time.Now().UTC()
	m.cleanupClearanceCacheLocked(now, interval)
	if _, exists := m.clearances[key]; !exists {
		m.ensureClearanceCacheCapacityLocked()
	}
	m.clearances[key] = clearanceState{
		cookies: solution.Cookies, userAgent: solution.UserAgent, refreshedAt: refreshedAt,
		used: true, version: version, fingerprint: fingerprint, bindingFingerprint: bindingFingerprint, lastUsedAt: now,
	}
	m.clearanceMu.Unlock()
}

func (m *Manager) cleanupClearanceCacheLocked(now time.Time, interval time.Duration) {
	if m.clearances == nil {
		m.clearances = make(map[string]clearanceState)
	}
	if !m.lastClearanceCleanup.IsZero() && now.Sub(m.lastClearanceCleanup) < clearanceCacheCleanupInterval {
		return
	}
	m.lastClearanceCleanup = now
	idleTTL := interval * 2
	if idleTTL < clearanceCacheMinIdleTTL {
		idleTTL = clearanceCacheMinIdleTTL
	}
	for key, state := range m.clearances {
		lastUsedAt := state.lastUsedAt
		if lastUsedAt.IsZero() {
			lastUsedAt = state.refreshedAt
		}
		if !lastUsedAt.IsZero() && now.Sub(lastUsedAt) >= idleTTL {
			delete(m.clearances, key)
		}
	}
}

func (m *Manager) ensureClearanceCacheCapacityLocked() {
	if len(m.clearances) < maxCachedClearances {
		return
	}
	type candidate struct {
		key      string
		lastUsed time.Time
	}
	candidates := make([]candidate, 0, len(m.clearances))
	for key, state := range m.clearances {
		lastUsedAt := state.lastUsedAt
		if lastUsedAt.IsZero() {
			lastUsedAt = state.refreshedAt
		}
		candidates = append(candidates, candidate{key: key, lastUsed: lastUsedAt})
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].lastUsed.Before(candidates[j].lastUsed)
	})
	removeCount := min(clearanceCacheEvictionBatch, len(candidates))
	for _, entry := range candidates[:removeCount] {
		delete(m.clearances, entry.key)
	}
}

func clearanceRefreshInterval(cfg ClearanceConfig) time.Duration {
	if cfg.RefreshInterval > 0 {
		return cfg.RefreshInterval
	}
	return 10 * time.Minute
}

func clearanceFingerprint(cfg ClearanceConfig, proxyURL string) string {
	value := strings.TrimSpace(cfg.FlareSolverrURL) + "\x00" + clearanceBindingFingerprint(cfg, proxyURL)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
}

func clearanceBindingFingerprint(cfg ClearanceConfig, proxyURL string) string {
	value := strings.TrimRight(strings.TrimSpace(cfg.TargetURL), "/") + "\x00" + strings.TrimSpace(proxyURL)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
}

func (m *Manager) recordClearanceError(ctx context.Context, node domain.Node, persist bool) {
	if node.ID == 0 || !persist {
		return
	}
	if stateRepository, ok := m.repository.(egressStateRepository); ok {
		if err := stateRepository.UpdateEgressNodeLastError(ctx, node.ID, "clearance refresh failed"); err == nil {
			m.invalidateNodes(node.Scope)
			return
		}
	}
	latest, err := m.repository.GetEgressNode(ctx, node.ID)
	if err != nil {
		return
	}
	latest.LastError = "clearance refresh failed"
	if _, err := m.repository.UpdateEgressNode(ctx, latest); err == nil {
		m.invalidateNodes(latest.Scope)
	}
}

func (m *Manager) RefreshClearance(ctx context.Context, nodeID uint64) error {
	if nodeID == 0 {
		_, err := m.refreshShared(ctx, domain.Node{Name: "direct", Scope: domain.ScopeWeb, Enabled: true}, "", "direct", false, true, true, time.Time{})
		return err
	}
	node, err := m.repository.GetEgressNode(ctx, nodeID)
	if err != nil {
		return err
	}
	if !isGrokWebScope(node.Scope) {
		return fmt.Errorf("出口节点 %q 不支持 Clearance 刷新", node.Name)
	}
	proxyURL, err := m.cipher.Decrypt(node.EncryptedProxyURL)
	if err != nil {
		return err
	}
	if strings.Contains(proxyURL, application.ProxyAccountPlaceholder) {
		return fmt.Errorf("出口节点 %q 使用账号粘性代理，将在账号请求时按租约自动刷新 Clearance", node.Name)
	}
	proxyURL, err = application.NormalizeProxyURL(proxyURL)
	if err != nil {
		return err
	}
	key := clearanceCacheKey(node.ID, proxyURL, false)
	_, err = m.refreshShared(ctx, node, proxyURL, key, true, true, true, time.Time{})
	return err
}

func (m *Manager) InvalidateClearance(nodeID uint64) {
	m.clearanceMu.Lock()
	prefix := "node:" + strconv.FormatUint(nodeID, 10)
	if nodeID == 0 {
		prefix = "direct"
	}
	for key, state := range m.clearances {
		if key == prefix || strings.HasPrefix(key, prefix+":") {
			state.invalid = true
			state.used = true
			m.clearances[key] = state
		}
	}
	m.clearanceMu.Unlock()
	m.clientMu.Lock()
	stale := m.invalidateClientLocked(nodeID)
	m.clientMu.Unlock()
	closeRequestClients(stale)
}

// ForgetClearance evicts runtime state after an administrator changes or
// removes a node. Unlike a 403 rejection, it does not mark the persisted
// last-known-good cookie as invalid; ensureClearance will still verify its
// binding before using it as a solver-failure fallback.
func (m *Manager) ForgetClearance(nodeID uint64) {
	m.ForgetClearances([]uint64{nodeID})
}

// ForgetClearances evicts a batch of node-scoped runtime state with one cache
// scan and one lock acquisition. Administrative bulk updates can contain
// thousands of nodes, so repeating the global snapshot invalidation per ID
// would add avoidable lock contention and CPU work.
func (m *Manager) ForgetClearances(nodeIDs []uint64) {
	ids := make(map[uint64]struct{}, len(nodeIDs))
	prefixes := make(map[string]struct{}, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		if _, exists := ids[nodeID]; exists {
			continue
		}
		ids[nodeID] = struct{}{}
		prefix := "node:" + strconv.FormatUint(nodeID, 10)
		if nodeID == 0 {
			prefix = "direct"
		}
		prefixes[prefix] = struct{}{}
	}
	if len(ids) == 0 {
		return
	}
	m.clearanceMu.Lock()
	m.nodeMu.Lock()
	m.clientMu.Lock()
	for key := range m.clearances {
		prefix := key
		if strings.HasPrefix(key, "node:") {
			if separator := strings.IndexByte(key[len("node:"):], ':'); separator >= 0 {
				prefix = key[:len("node:")+separator]
			}
		} else if strings.HasPrefix(key, "direct:") {
			prefix = "direct"
		}
		if _, selected := prefixes[prefix]; selected {
			delete(m.clearances, key)
		}
	}
	// Node mutations are rare administration operations. Clearing the small
	// one-second snapshots prevents a just-edited proxy from being used once
	// more before its scope cache expires.
	if m.nodeVersions == nil {
		m.nodeVersions = make(map[domain.Scope]uint64)
	}
	for _, scope := range allEgressScopes() {
		m.nodeVersions[scope]++
	}
	clear(m.nodes)
	clear(m.healthyNodes)
	var stale []requestClient
	for nodeID := range ids {
		m.invalidateClientVersionLocked(nodeID)
	}
	for key, cached := range m.clients {
		if _, selected := ids[key.nodeID]; !selected {
			continue
		}
		delete(m.clients, key)
		stale = append(stale, cached.client)
	}
	m.clientMu.Unlock()
	m.nodeMu.Unlock()
	m.clearanceMu.Unlock()
	closeRequestClients(stale)
}

func (m *Manager) invalidateClearanceKey(key string, client requestClient) {
	m.clearanceMu.Lock()
	state := m.clearances[key]
	state.invalid = true
	state.used = true
	state.lastUsedAt = time.Now().UTC()
	m.clearances[key] = state
	m.clearanceMu.Unlock()
	if client != nil {
		client.CloseIdleConnections()
	}
}

func (m *Manager) RefreshDueClearances(ctx context.Context, force bool) error {
	m.clearanceMu.Lock()
	cfg := m.clearanceConfig
	direct := m.clearances["direct"]
	version := m.clearanceVersion
	m.clearanceMu.Unlock()
	if cfg.Mode != "flaresolverr" {
		return nil
	}
	interval := clearanceRefreshInterval(cfg)
	now := time.Now().UTC()
	nodes, err := m.repository.ListEgressNodes(ctx, "", repository.SortQuery{})
	if err != nil {
		return err
	}
	var refreshErrors []error
	webNodeCount := 0
	for _, node := range nodes {
		if !node.Enabled || !isGrokWebScope(node.Scope) {
			continue
		}
		webNodeCount++
		proxyURL, decryptErr := m.cipher.Decrypt(node.EncryptedProxyURL)
		if decryptErr != nil {
			refreshErrors = append(refreshErrors, decryptErr)
			continue
		}
		if strings.Contains(proxyURL, application.ProxyAccountPlaceholder) {
			// Resin clearance is account/IP bound and has no safe node-wide value
			// for a background task to solve or persist.
			continue
		}
		proxyURL, normalizeErr := application.NormalizeProxyURL(proxyURL)
		if normalizeErr != nil {
			refreshErrors = append(refreshErrors, normalizeErr)
			continue
		}
		m.clearanceMu.Lock()
		key := clearanceCacheKey(node.ID, proxyURL, false)
		state, known := m.clearances[key]
		m.clearanceMu.Unlock()
		fingerprint := clearanceFingerprint(cfg, proxyURL)
		memoryFresh := known && !state.invalid && state.version == version && state.fingerprint == fingerprint && now.Sub(state.refreshedAt) < interval
		persistedFresh := (!known || !state.invalid) && node.ClearanceRefreshedAt != nil && node.ClearanceFingerprint == fingerprint && now.Sub(*node.ClearanceRefreshedAt) < interval
		if !force && (memoryFresh || persistedFresh) {
			continue
		}
		refreshForce := force || (known && state.invalid)
		refreshAfter := time.Time{}
		if refreshForce && known && state.invalid {
			refreshAfter = state.refreshedAt
		}
		_, refreshErr := m.refreshShared(ctx, node, proxyURL, key, true, refreshForce, false, refreshAfter)
		if refreshErr != nil {
			refreshErrors = append(refreshErrors, refreshErr)
		}
	}
	shouldUseDirect := direct.used || force && webNodeCount == 0
	if shouldUseDirect && (force || direct.invalid || direct.userAgent == "" || direct.version != version || now.Sub(direct.refreshedAt) >= interval) {
		_, err := m.refreshShared(ctx, domain.Node{Name: "direct", Scope: domain.ScopeWeb, Enabled: true}, "", "direct", false, force, false, time.Time{})
		if err != nil {
			refreshErrors = append(refreshErrors, err)
		}
	}
	return errors.Join(refreshErrors...)
}

// NormalizeSSOToken extracts the bare token from a raw value or a pasted
// "sso=...;" cookie and strips line breaks and NUL bytes.
func NormalizeSSOToken(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(value), "sso=") {
		value = strings.TrimSpace(value[len("sso="):])
	}
	if token, _, found := strings.Cut(value, ";"); found {
		value = token
	}
	return strings.TrimSpace(strings.NewReplacer("\r", "", "\n", "", "\x00", "").Replace(value))
}

func BuildSSOCookie(token, cloudflareCookies string) string {
	token = NormalizeSSOToken(token)
	cookies := "sso=" + token + "; sso-rw=" + token
	if sanitized := application.SanitizeCloudflareCookies(cloudflareCookies); sanitized != "" {
		cookies += "; " + sanitized
	}
	return cookies
}
