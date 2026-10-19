package egress

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
)

func (m *Manager) clientFor(id uint64, scope domain.Scope, proxyURL, userAgent, cookies string, sticky bool, accountIdentity string) (cachedClient, error) {
	return m.clientForWithOptions(id, scope, proxyURL, userAgent, cookies, sticky, accountIdentity, clientOptions{})
}

func (m *Manager) clientForWithOptions(id uint64, scope domain.Scope, proxyURL, userAgent, cookies string, sticky bool, accountIdentity string, options clientOptions) (cachedClient, error) {
	clientKind := "browser"
	buildHeaderTimeout := time.Duration(0)
	if scope == domain.ScopeBuild {
		clientKind = "build"
		buildHeaderTimeout = time.Duration(m.buildHeaderTimeout.Load())
		if buildHeaderTimeout <= 0 {
			buildHeaderTimeout = settingsdomain.DefaultBuildResponseHeaderTimeout
		}
		clientKind += "\x00" + strconv.FormatInt(int64(buildHeaderTimeout), 10)
		if options.buildEnvironmentProxy {
			clientKind += "\x00environment-proxy"
		}
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(clientKind+"\x00"+proxyURL+"\x00"+userAgent+"\x00"+cookies)))
	cacheScope := scope
	if cacheScope == domain.ScopeWebAsset {
		cacheScope = domain.ScopeWeb
	}
	for attempt := 0; attempt < clientCreationRetryLimit; attempt++ {
		isolated := m.accountIsolated.Load()
		if options.requireAccountIsolation && !isolated {
			return cachedClient{}, errAccountConnectionIsolationDisabled
		}
		keyAccountIdentity := ""
		if isolated {
			keyAccountIdentity = strings.TrimSpace(accountIdentity)
			if keyAccountIdentity == "" {
				keyAccountIdentity = "shared"
			}
		}
		key := clientCacheKey{nodeID: id, scope: cacheScope, fingerprint: fingerprint, accountIdentity: keyAccountIdentity}
		loadKey := strconv.FormatUint(key.nodeID, 10) + "\x00" + string(key.scope) + "\x00" + key.fingerprint + "\x00" + key.accountIdentity
		now := time.Now().UTC()
		m.clientMu.RLock()
		cached, cachedOK := m.clients[key]
		cleanupDue := m.lastClientCleanup.IsZero() || now.Sub(m.lastClientCleanup) >= clientCacheCleanupInterval
		touchDue := cachedOK && (cached.lastUsed.IsZero() || now.Sub(cached.lastUsed) >= clientCacheTouchInterval)
		m.clientMu.RUnlock()
		if cachedOK && !cleanupDue && !touchDue {
			return cached, nil
		}

		m.clientMu.Lock()
		stale := m.cleanupClientCacheLocked(now)
		if cached, ok := m.clients[key]; ok {
			cached.lastUsed = now
			m.clients[key] = cached
			m.clientMu.Unlock()
			closeRequestClients(stale)
			return cached, nil
		}
		m.clientMu.Unlock()
		closeRequestClients(stale)

		loaded, err, _ := m.clientLoads.Do(loadKey, func() (any, error) {
			return m.createAndCacheClient(key, id, scope, proxyURL, userAgent, sticky, buildHeaderTimeout, options)
		})
		if errors.Is(err, errClientCacheInvalidated) {
			continue
		}
		if err != nil {
			return cachedClient{}, err
		}
		return loaded.(cachedClient), nil
	}
	return cachedClient{}, errClientCacheInvalidated
}

func (m *Manager) createAndCacheClient(key clientCacheKey, id uint64, scope domain.Scope, proxyURL, userAgent string, sticky bool, buildHeaderTimeout time.Duration, options clientOptions) (cachedClient, error) {
	now := time.Now().UTC()
	m.clientMu.Lock()
	stale := m.cleanupClientCacheLocked(now)
	if (key.accountIdentity != "") != m.accountIsolated.Load() {
		m.clientMu.Unlock()
		closeRequestClients(stale)
		return cachedClient{}, errClientCacheInvalidated
	}
	if cached, ok := m.clients[key]; ok {
		cached.lastUsed = now
		m.clients[key] = cached
		m.clientMu.Unlock()
		closeRequestClients(stale)
		return cached, nil
	}
	version := m.clientVersionLocked(id)
	m.clientMu.Unlock()
	closeRequestClients(stale)

	value, err := m.buildCachedClient(scope, proxyURL, userAgent, buildHeaderTimeout, options)
	if err != nil {
		return cachedClient{}, err
	}
	value.lastUsed = time.Now().UTC()

	m.clientMu.Lock()
	stale = m.cleanupClientCacheLocked(value.lastUsed)
	if (key.accountIdentity != "") != m.accountIsolated.Load() {
		m.clientMu.Unlock()
		closeRequestClients(append(stale, value.client))
		return cachedClient{}, errClientCacheInvalidated
	}
	if cached, ok := m.clients[key]; ok {
		cached.lastUsed = value.lastUsed
		m.clients[key] = cached
		m.clientMu.Unlock()
		closeRequestClients(append(stale, value.client))
		return cached, nil
	}
	if m.clientVersionLocked(id) != version {
		m.clientMu.Unlock()
		closeRequestClients(append(stale, value.client))
		return cachedClient{}, errClientCacheInvalidated
	}
	if id != 0 && !sticky {
		for previousKey, previous := range m.clients {
			if previousKey.nodeID != id || previousKey.scope != key.scope {
				continue
			}
			// Keep other accounts' pools when isolation is on.
			if key.accountIdentity != "" && previousKey.accountIdentity != key.accountIdentity {
				continue
			}
			stale = append(stale, m.evictClientLocked(previousKey, previous))
		}
	}
	stale = append(stale, m.ensureClientCacheCapacityLocked()...)
	m.clients[key] = value
	m.clientMu.Unlock()
	closeRequestClients(stale)
	return value, nil
}

func (m *Manager) buildCachedClient(scope domain.Scope, proxyURL, userAgent string, buildHeaderTimeout time.Duration, options clientOptions) (cachedClient, error) {
	if scope == domain.ScopeBuild {
		if options.buildEnvironmentProxy {
			factory := m.newBuildEnvClient
			if factory == nil {
				factory = newBuildEnvironmentRequestClient
			}
			client, err := factory(buildHeaderTimeout)
			if err != nil {
				return cachedClient{}, err
			}
			return cachedClient{client: client}, nil
		}
		factory := m.newBuildClient
		if factory == nil {
			factory = newBuildRequestClient
		}
		client, err := factory(proxyURL, buildHeaderTimeout)
		if err != nil {
			return cachedClient{}, err
		}
		return cachedClient{client: client}, nil
	}
	factory := m.newBrowserClient
	if factory == nil {
		factory = newBrowserClient
	}
	client, err := factory(proxyURL, userAgent)
	if err != nil {
		return cachedClient{}, err
	}
	return cachedClient{client: client, browser: client}, nil
}

func newBuildRequestClient(proxyURL string, responseHeaderTimeout time.Duration) (requestClient, error) {
	return newBuildClient(proxyURL, responseHeaderTimeout)
}

func newBuildEnvironmentRequestClient(responseHeaderTimeout time.Duration) (requestClient, error) {
	return newBuildEnvironmentClient(responseHeaderTimeout)
}

func (m *Manager) cleanupClientCacheLocked(now time.Time) []requestClient {
	if m.clients == nil {
		m.clients = make(map[clientCacheKey]cachedClient)
	}
	if !m.lastClientCleanup.IsZero() && now.Sub(m.lastClientCleanup) < clientCacheCleanupInterval {
		return nil
	}
	m.lastClientCleanup = now
	var stale []requestClient
	for key, value := range m.clients {
		if !value.lastUsed.IsZero() && now.Sub(value.lastUsed) >= clientCacheIdleTTL {
			stale = append(stale, m.evictClientLocked(key, value))
		}
	}
	return stale
}

func (m *Manager) ensureClientCacheCapacityLocked() []requestClient {
	var stale []requestClient
	for len(m.clients) >= maxCachedClients {
		var oldestKey clientCacheKey
		var oldest cachedClient
		found := false
		for key, value := range m.clients {
			if !found || value.lastUsed.Before(oldest.lastUsed) {
				oldestKey, oldest, found = key, value, true
			}
		}
		if !found {
			break
		}
		stale = append(stale, m.evictClientLocked(oldestKey, oldest))
	}
	return stale
}

func (m *Manager) evictClientLocked(key clientCacheKey, value cachedClient) requestClient {
	delete(m.clients, key)
	return value.client
}

func closeRequestClients(values []requestClient) {
	for _, value := range values {
		if value != nil {
			value.CloseIdleConnections()
		}
	}
}

func (m *Manager) clientVersionLocked(nodeID uint64) uint64 {
	return m.clientGeneration + m.clientVersions[nodeID]
}

func (m *Manager) invalidateClientVersionLocked(nodeID uint64) {
	if m.clientVersions == nil {
		m.clientVersions = make(map[uint64]uint64)
	}
	if _, exists := m.clientVersions[nodeID]; !exists && len(m.clientVersions) >= maxClientVersionEntries {
		// A generation bump invalidates in-flight creations before the tombstone map is reset.
		m.clientGeneration++
		clear(m.clientVersions)
	}
	m.clientVersions[nodeID]++
}

func (m *Manager) invalidateAllClientVersionsLocked() {
	m.clientGeneration++
	clear(m.clientVersions)
}

func (m *Manager) invalidateClientLocked(nodeID uint64) []requestClient {
	m.invalidateClientVersionLocked(nodeID)
	var stale []requestClient
	for key, cached := range m.clients {
		if key.nodeID != nodeID {
			continue
		}
		delete(m.clients, key)
		stale = append(stale, cached.client)
	}
	return stale
}

func (m *Manager) invalidateClientForScopeLocked(nodeID uint64, scope domain.Scope) []requestClient {
	m.invalidateClientVersionLocked(nodeID)
	if scope == domain.ScopeWebAsset {
		scope = domain.ScopeWeb
	}
	var stale []requestClient
	for key, cached := range m.clients {
		if key.nodeID != nodeID || key.scope != scope {
			continue
		}
		delete(m.clients, key)
		stale = append(stale, cached.client)
	}
	return stale
}
