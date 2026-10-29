package cli

import (
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
	"github.com/chenyme/grok2api/backend/internal/infra/buildtransport"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/netguard"
	"github.com/chenyme/grok2api/backend/internal/pkg/reasoningreplay"
)

type Config struct {
	BaseURL               string
	FallbackBaseURL       string
	ClientVersion         string
	ClientIdentifier      string
	TokenAuth             string
	UserAgent             string
	ResponseHeaderTimeout time.Duration
	StreamIdleTimeout     time.Duration
}

const (
	subscriptionTierTimeout = 10 * time.Second
	buildControlTimeout     = 30 * time.Second
	buildGrok45Model        = "grok-4.5"
	buildGrok46Model        = "grok-4.6"
)

// Adapter implements the Grok Build CLI Responses, model, Billing, and OAuth protocols.
type Adapter struct {
	cfgMu          sync.RWMutex
	cfg            Config
	http           *http.Client
	public         *http.Client
	oauth          *oauthClient
	cipher         *security.Cipher
	base           *buildDirectTransport
	agentID        string
	modelsMu       sync.Mutex
	modelsETags    map[uint64]string
	fallbackMarker FallbackMarker
	uploadIssuer   VideoUploadIssuer
	replay         *reasoningreplay.ReasoningReplay
	// conversationReasoningCache bridges Chat/Messages tool calls to the
	// upstream Responses reasoning proof. It is deliberately separate from
	// the persistent native Responses replay, and its keys never contain an
	// account ID: encrypted reasoning is portable across Build accounts.
	conversationReasoningCache *conversation.ReasoningCache
	compaction                 *gatewayCompactionCodec
	logger                     *slog.Logger
}

func NewAdapter(cfg Config, cipher *security.Cipher) *Adapter {
	cfg.ResponseHeaderTimeout = normalizeBuildResponseHeaderTimeout(cfg.ResponseHeaderTimeout)
	cfg.StreamIdleTimeout = normalizeBuildStreamIdleTimeout(cfg.StreamIdleTimeout)
	transport := newBuildDirectTransport(cfg.ResponseHeaderTimeout)
	httpClient := &http.Client{Transport: transport}
	// The official CLI uses a persistent machine identity. The gateway does not collect machine fingerprints;
	// instead each backend process generates one random UUID for its lifetime as the Agent identity.
	agentID := uuid.NewString()
	adapter := &Adapter{
		cfg: cfg, http: httpClient, cipher: cipher, base: transport,
		public:  netguard.NewPublicClient(),
		agentID: agentID, modelsETags: make(map[uint64]string),
		conversationReasoningCache: conversation.NewReasoningCache(0, 0),
		compaction:                 newGatewayCompactionCodec(cipher), logger: slog.Default(),
	}
	adapter.oauth = newOAuthClient(httpClient, func() string { return adapter.config().ClientVersion })
	return adapter
}

func (a *Adapter) SetLogger(logger *slog.Logger) {
	if logger != nil {
		a.logger = logger
	}
}

func (a *Adapter) SetEgress(manager *infraegress.Manager) {
	if manager != nil {
		a.http.Transport = &egressTransport{manager: manager, fallback: a.base}
	}
}

// SetReasoningReplay injects the optional server-side reasoning replay cache.
func (a *Adapter) SetReasoningReplay(replay *reasoningreplay.ReasoningReplay) {
	a.replay = replay
}

func (a *Adapter) Provider() account.Provider { return account.ProviderBuild }

func (a *Adapter) UpdateConfig(cfg Config) {
	cfg.ResponseHeaderTimeout = normalizeBuildResponseHeaderTimeout(cfg.ResponseHeaderTimeout)
	cfg.StreamIdleTimeout = normalizeBuildStreamIdleTimeout(cfg.StreamIdleTimeout)
	a.cfgMu.Lock()
	previousTimeout := a.cfg.ResponseHeaderTimeout
	a.cfg = cfg
	a.cfgMu.Unlock()
	if previousTimeout != cfg.ResponseHeaderTimeout && a.base != nil {
		a.base.UpdateResponseHeaderTimeout(cfg.ResponseHeaderTimeout)
	}
}

type buildDirectTransport struct {
	current atomic.Pointer[http.Transport]
}

func newBuildDirectTransport(responseHeaderTimeout time.Duration) *buildDirectTransport {
	value := &buildDirectTransport{}
	value.current.Store(newBuildHTTPTransport(responseHeaderTimeout))
	return value
}

func (t *buildDirectTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return t.current.Load().RoundTrip(request)
}

func (t *buildDirectTransport) UpdateResponseHeaderTimeout(responseHeaderTimeout time.Duration) {
	next := newBuildHTTPTransport(responseHeaderTimeout)
	previous := t.current.Swap(next)
	if previous != nil {
		previous.CloseIdleConnections()
	}
}

func newBuildHTTPTransport(responseHeaderTimeout time.Duration) *http.Transport {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true,
		MaxIdleConns: 256, MaxIdleConnsPerHost: 128, MaxConnsPerHost: 256,
		IdleConnTimeout: buildtransport.IdleConnTimeout, TLSHandshakeTimeout: 10 * time.Second,
		ResponseHeaderTimeout: normalizeBuildResponseHeaderTimeout(responseHeaderTimeout),
		ExpectContinueTimeout: time.Second,
	}
	if _, err := buildtransport.ConfigureHTTP2Health(transport); err != nil {
		slog.Warn("build_http2_health_config_failed", "error", err)
	}
	return transport
}

func normalizeBuildResponseHeaderTimeout(value time.Duration) time.Duration {
	if value <= 0 {
		return settingsdomain.DefaultBuildResponseHeaderTimeout
	}
	return value
}

func normalizeBuildStreamIdleTimeout(value time.Duration) time.Duration {
	if value <= 0 {
		return settingsdomain.DefaultBuildStreamIdleTimeout
	}
	return value
}

func (a *Adapter) config() Config {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return a.cfg
}

func (a *Adapter) applyHeaders(req *http.Request, credential account.Credential, accessToken, model, promptCacheKey string, trace bool) error {
	cfg := a.config()
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("X-XAI-Token-Auth", cfg.TokenAuth)
	req.Header.Set("x-grok-client-version", cfg.ClientVersion)
	req.Header.Set("x-grok-client-identifier", cfg.ClientIdentifier)
	req.Header.Set("x-grok-client-mode", "headless")

	if trace {
		requestID := uuid.NewString()
		// Set x-grok-conv-id and session-id only when a stable session exists.
		// Never generate a random UUID per request; it breaks xAI session affinity and keeps cached_tokens at zero.
		sessionID, err := grokSessionID(promptCacheKey)
		if err != nil {
			return err
		}
		req.Header.Set("x-authenticateresponse", "authenticate-response")
		req.Header.Set("x-grok-agent-id", a.agentID)
		if sessionID != "" {
			req.Header.Set("x-grok-session-id", sessionID)
			req.Header.Set("x-grok-conv-id", sessionID)
			// grok-build derives x-grok-conv-group-id from the root session so the
			// proxy can group a root conversation with its subagent descendants.
			req.Header.Set("x-grok-conv-group-id", grokConversationGroupID(sessionID))
		}
		req.Header.Set("x-grok-req-id", requestID)
		// The gateway cannot reliably recover the CLI prompt index from a stateless API request.
		// The field is optional in the official protocol, so do not fabricate x-grok-turn-idx.
		if credential.UserID != "" {
			req.Header.Set("x-grok-user-id", credential.UserID)
		}
		traceID, traceErr := randomHex(16)
		if traceErr != nil {
			return traceErr
		}
		spanID, spanErr := randomHex(8)
		if spanErr != nil {
			return spanErr
		}
		req.Header.Set("traceparent", "00-"+traceID+"-"+spanID+"-01")
	} else {
		if credential.UserID != "" {
			req.Header.Set("x-userid", credential.UserID)
		}
		if credential.Email != "" {
			req.Header.Set("x-email", credential.Email)
		}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("User-Agent", cfg.UserAgent)
	if model != "" {
		req.Header.Set("x-grok-model-override", model)
	}
	return nil
}

// grokSessionID converts a stable session key to the upstream x-grok-conv-id.
// An empty key returns an empty string; stateless requests never receive a random ID.
func grokSessionID(promptCacheKey string) (string, error) {
	key := strings.TrimSpace(promptCacheKey)
	if key == "" {
		return "", nil
	}
	if parsed, err := uuid.Parse(key); err == nil {
		return parsed.String(), nil
	}
	return uuid.NewHash(sha256.New(), uuid.NameSpaceURL, []byte("grok2api:session:"+key), 8).String(), nil
}

// grokConversationGroupIDNamespace mirrors xai-grok-shell `CONVERSATION_GROUP_NAMESPACE`.
const grokConversationGroupIDNamespace = "xai:grok-build:conversation-group:"

// grokConversationGroupID reproduces grok-build `derive_conversation_group_id`:
// UUIDv5(NAMESPACE_OID, namespace + root_session_id). The gateway treats each
// stable session as its own root conversation, so the group id derives from the
// same session id sent as x-grok-conv-id.
func grokConversationGroupID(rootSessionID string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(grokConversationGroupIDNamespace+rootSessionID)).String()
}

func randomHex(bytesLength int) (string, error) {
	value := make([]byte, bytesLength)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func normalizeGzipResponse(response *http.Response) error {
	if response == nil || response.Body == nil || !strings.EqualFold(strings.TrimSpace(response.Header.Get("Content-Encoding")), "gzip") {
		return nil
	}
	reader, err := gzip.NewReader(response.Body)
	if err != nil {
		_ = response.Body.Close()
		return err
	}
	response.Body = &gzipResponseBody{Reader: reader, source: response.Body}
	response.Header.Del("Content-Encoding")
	response.Header.Del("Content-Length")
	response.ContentLength = -1
	return nil
}

type gzipResponseBody struct {
	*gzip.Reader
	source io.Closer
}

func (b *gzipResponseBody) Close() error {
	readerErr := b.Reader.Close()
	sourceErr := b.source.Close()
	if readerErr != nil {
		return readerErr
	}
	return sourceErr
}

func (a *Adapter) url(path string) string {
	return strings.TrimRight(a.config().BaseURL, "/") + "/" + strings.TrimLeft(path, "/")
}
