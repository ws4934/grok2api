package egress

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"golang.org/x/sync/singleflight"
)

const DefaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"
const nodeSnapshotTTL = time.Second
const operationsConfigSnapshotTTL = time.Second

// snapshotLoadTimeout bounds a shared node or operations-config read, which
// no longer stops when the request that started it leaves.
const snapshotLoadTimeout = 30 * time.Second
const proxyPoolRetryLimit = 2
const clientCacheIdleTTL = 30 * time.Minute
const clientCacheCleanupInterval = time.Minute
const clientCacheTouchInterval = time.Minute
const maxCachedClients = 4096
const clearanceLockGrace = 30 * time.Second
const clearanceCacheCleanupInterval = time.Minute
const clearanceCacheMinIdleTTL = 30 * time.Minute
const maxCachedClearances = 16384
const clearanceCacheEvictionBatch = 256

// clientClosedRequestStatus is the conventional proxy status for a client-aborted request.
const clientClosedRequestStatus = 499
const egressIPv4ProbeEndpoint = "https://ipinfo.io/json"
const egressIPv6ProbeEndpoint = "https://v6.ipinfo.io/json"
const cloudflareIPv4ProbeEndpoint = "https://1.1.1.1/cdn-cgi/trace"
const cloudflareIPv6ProbeEndpoint = "https://[2606:4700:4700::1111]/cdn-cgi/trace"
const egressProbeTimeout = 15 * time.Second
const failureProbeCompletionGrace = 5 * time.Second
const failureProbeTimeout = 20 * time.Second
const failureProbeWaitTimeout = 5 * time.Second
const clientCreationRetryLimit = 3
const maxClientVersionEntries = 4096

var errNodeSnapshotInvalidated = errors.New("egress node snapshot invalidated")
var errClientCacheInvalidated = errors.New("egress client cache invalidated")
var errAccountConnectionIsolationDisabled = errors.New("egress account connection isolation disabled")

type Lease struct {
	NodeID           uint64
	NodeName         string
	Scope            domain.Scope
	ProxyURL         string
	UserAgent        string
	CFCookies        string
	client           requestClient
	browser          *browserClient
	newClient        func() (cachedClient, error) // extension scopes: built on first Do
	clientOnce       sync.Once
	clientErr        error
	sticky           bool
	proxyPool        bool
	freshTunnel      bool
	clearanceKey     string
	clearanceManager *Manager
	release          func()
}

type requestClient interface {
	Do(*http.Request) (*http.Response, error)
	CloseIdleConnections()
}

type FailureProber func(context.Context, uint64) (domain.ProbeResult, error)

type failureProbeState struct {
	running       bool
	lastCompleted time.Time
	done          chan struct{}
}

func (l *Lease) Do(request *http.Request) (*http.Response, error) {
	return l.doRequest(request, true)
}

// DoDeferredForbidden executes an HTTP request while leaving 403 clearance
// invalidation to the caller after it has classified the response body.
func (l *Lease) DoDeferredForbidden(request *http.Request) (*http.Response, error) {
	return l.doRequest(request, false)
}

func (l *Lease) doRequest(request *http.Request, invalidateForbidden bool) (*http.Response, error) {
	if l != nil && l.newClient != nil {
		l.clientOnce.Do(func() {
			var client cachedClient
			client, l.clientErr = l.newClient()
			l.client, l.browser = client.client, client.browser
		})
		if l.clientErr != nil {
			return nil, l.clientErr
		}
	}
	if l == nil || l.client == nil {
		return nil, errors.New("出口客户端未初始化")
	}
	// Rotating proxy endpoints choose an exit when a new CONNECT tunnel is
	// established. Reusing a Build keep-alive/HTTP2 connection would pin many
	// otherwise independent requests to one exit and defeat proxy-pool
	// rotation. Proxy-pool mode is explicit, so trade the extra handshake for a
	// fresh tunnel without changing fixed-proxy, direct, Web, or Console paths.
	if l.Scope == domain.ScopeBuild && l.freshTunnel {
		request.Close = true
	}
	response, err := l.do(request)
	recordPhysicalCall(request.Context(), response, err)
	if invalidateForbidden && err == nil && response != nil && response.StatusCode == http.StatusForbidden {
		l.InvalidateClearance()
	}
	return response, err
}

// InvalidateClearance invalidates the exact browser-session binding used by
// this lease after a 403 has been classified as egress-related.
func (l *Lease) InvalidateClearance() {
	if l != nil && l.clearanceManager != nil && l.clearanceKey != "" {
		l.clearanceManager.invalidateClearanceKey(l.clearanceKey, l.client)
	}
}

func (l *Lease) Release() {
	if l != nil && l.release != nil {
		l.release()
		l.release = nil
	}
}

type Manager struct {
	repository             repository.EgressRepository
	cipher                 *security.Cipher
	logger                 *slog.Logger
	nodeMu                 sync.RWMutex
	clientMu               sync.RWMutex
	clearanceMu            sync.Mutex
	operationsMu           sync.RWMutex
	clients                map[clientCacheKey]cachedClient
	inflight               sync.Map
	nodes                  map[domain.Scope]cachedNodeSnapshot
	healthyNodes           map[uint64]time.Time
	nodeVersions           map[domain.Scope]uint64
	nodeLoads              singleflight.Group
	clientLoads            singleflight.Group
	clientVersions         map[uint64]uint64
	clientGeneration       uint64
	buildHeaderTimeout     atomic.Int64
	buildStreamIdleTimeout atomic.Int64
	accountIsolated        atomic.Bool
	operationsConfig       cachedOperationsConfig
	operationsConfigLoad   singleflight.Group
	operationsConfigVer    uint64
	failureProbeMu         sync.Mutex
	failureProber          FailureProber
	failureProbes          map[uint64]failureProbeState
	lastClientCleanup      time.Time
	clearanceLoads         singleflight.Group
	clearanceConfig        ClearanceConfig
	extensionClearance     map[domain.Scope]string
	clearanceVersion       uint64
	clearances             map[string]clearanceState
	lastClearanceCleanup   time.Time
	solver                 clearanceSolver
	clearanceLock          repository.DistributedLock
	newBuildClient         func(string, time.Duration) (requestClient, error)
	newBuildEnvClient      func(time.Duration) (requestClient, error)
	newBrowserClient       func(string, string) (*browserClient, error)
}

type clearanceState struct {
	cookies            string
	userAgent          string
	refreshedAt        time.Time
	invalid            bool
	used               bool
	version            uint64
	fingerprint        string
	bindingFingerprint string
	lastUsedAt         time.Time
}

type egressStateRepository interface {
	UpdateEgressNodeClearance(context.Context, uint64, string, string, string, string, time.Time) error
	UpdateEgressNodeHealth(context.Context, uint64, float64, int, *time.Time, string) error
	UpdateEgressNodeLastError(context.Context, uint64, string) error
}

// operationsConfigRepository is optional so lightweight routing repositories
// retain their narrow contract. The relational implementation supplies it,
// allowing fallback policy to be read only when primary selection fails.
type operationsConfigRepository interface {
	GetEgressOperationsConfig(context.Context) (domain.OperationsConfig, error)
}

type cachedClient struct {
	client   requestClient
	browser  *browserClient
	lastUsed time.Time
}

type clientCacheKey struct {
	nodeID          uint64
	scope           domain.Scope
	fingerprint     string
	accountIdentity string
}

type cachedNodeSnapshot struct {
	values    []domain.Node
	expiresAt time.Time
}

type cachedOperationsConfig struct {
	value     domain.OperationsConfig
	expiresAt time.Time
}

func NewManager(repository repository.EgressRepository, cipher *security.Cipher) *Manager {
	manager := &Manager{
		repository: repository, cipher: cipher,
		clients: make(map[clientCacheKey]cachedClient),
		nodes:   make(map[domain.Scope]cachedNodeSnapshot), healthyNodes: make(map[uint64]time.Time),
		nodeVersions: make(map[domain.Scope]uint64), clientVersions: make(map[uint64]uint64), clearances: make(map[string]clearanceState),
		failureProbes:  make(map[uint64]failureProbeState),
		newBuildClient: newBuildRequestClient, newBuildEnvClient: newBuildEnvironmentRequestClient, newBrowserClient: newBrowserClient,
		solver:          flaresolverrSolver{},
		clearanceConfig: ClearanceConfig{Mode: "manual", TargetURL: "https://grok.com", Timeout: time.Minute, RefreshInterval: 10 * time.Minute},
	}
	manager.buildHeaderTimeout.Store(int64(settingsdomain.DefaultBuildResponseHeaderTimeout))
	manager.buildStreamIdleTimeout.Store(int64(settingsdomain.DefaultBuildStreamIdleTimeout))
	return manager
}

func (m *Manager) SetLogger(logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	m.logger = logger
}

func (m *Manager) log() *slog.Logger {
	if m == nil || m.logger == nil {
		return slog.Default()
	}
	return m.logger
}

// UpdateBuildResponseHeaderTimeout rebuilds only cached Build clients. Active
// requests keep their current transport and are not interrupted.
func (m *Manager) UpdateBuildResponseHeaderTimeout(value time.Duration) {
	if value <= 0 {
		value = settingsdomain.DefaultBuildResponseHeaderTimeout
	}
	if previous := time.Duration(m.buildHeaderTimeout.Swap(int64(value))); previous == value {
		return
	}
	m.clientMu.Lock()
	var stale []requestClient
	for key, cached := range m.clients {
		if key.scope == domain.ScopeBuild {
			stale = append(stale, m.evictClientLocked(key, cached))
		}
	}
	m.clientMu.Unlock()
	closeRequestClients(stale)
}

// UpdateBuildStreamIdleTimeout affects subsequent Build streams. Active
// response bodies retain the deadline captured by their existing wrapper and
// are not interrupted; the underlying HTTP connection pool is unchanged.
func (m *Manager) UpdateBuildStreamIdleTimeout(value time.Duration) {
	if value <= 0 {
		value = settingsdomain.DefaultBuildStreamIdleTimeout
	}
	m.buildStreamIdleTimeout.Store(int64(value))
}

// BuildStreamIdleTimeout returns the configured stream idle deadline for Grok
// Build responses. Returns zero when idle enforcement is disabled.
func (m *Manager) BuildStreamIdleTimeout() time.Duration {
	return time.Duration(m.buildStreamIdleTimeout.Load())
}

// UpdateAccountIsolatedConnections toggles per-account upstream connection pools.
// When enabled, different accounts do not share TCP/HTTP clients so upstream
// egress load balancers can spread traffic by connection; the same account still
// reuses its own pool. Changing the setting rebuilds cached clients without
// interrupting in-flight requests.
func (m *Manager) UpdateAccountIsolatedConnections(enabled bool) {
	m.clientMu.Lock()
	if m.accountIsolated.Load() == enabled {
		m.clientMu.Unlock()
		return
	}
	// Change the mode while holding the same lock used to validate client-cache
	// keys. This makes the mode snapshot and cache invalidation one transition.
	m.accountIsolated.Store(enabled)
	stale := make([]requestClient, 0, len(m.clients))
	for key, cached := range m.clients {
		stale = append(stale, m.evictClientLocked(key, cached))
	}
	m.invalidateAllClientVersionsLocked()
	m.clientMu.Unlock()
	closeRequestClients(stale)
	m.log().Info("egress_account_connection_isolation_updated", "enabled", enabled, "evicted_clients", len(stale))
}

// AccountIsolatedConnections reports whether upstream clients are partitioned by account.
func (m *Manager) AccountIsolatedConnections() bool {
	return m != nil && m.accountIsolated.Load()
}

func isGrokWebScope(scope domain.Scope) bool {
	return scope == domain.ScopeWeb || scope == domain.ScopeWebAsset || scope == domain.ScopeConsole
}

func allEgressScopes() []domain.Scope {
	return append([]domain.Scope{domain.ScopeBuild, domain.ScopeWeb, domain.ScopeConsole, domain.ScopeWebAsset, domain.ScopeConsoleAsset}, domain.ExtensionScopes()...)
}
