package muse

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

var scope = egressdomain.Scope(account.ProviderMuse)

// RemoteFetcher downloads a client's https input image (with SSRF protection).
type RemoteFetcher func(ctx context.Context, rawURL string) ([]byte, error)

// AssetStore archives generated images and videos where the gateway serves them.
type AssetStore interface {
	provider.ImageAssetStore
	SaveVideo(ctx context.Context, jobID, contentType string, body io.Reader) (media.Asset, error)
}

// Adapter serves Muse chat, images and videos through the browser engine.
type Adapter struct {
	mu          sync.RWMutex
	cfg         Config
	egress      *infraegress.Manager
	cipher      *security.Cipher
	assets      AssetStore
	fetchRemote RemoteFetcher
	engine      engine
	logger      *slog.Logger
	// sessionURL renews the session cookie; a var for tests.
	sessionURL string
}

// NewAdapter builds the Muse adapter. fetchRemote nil accepts only data URI inputs.
func NewAdapter(cfg Config, egress *infraegress.Manager, cipher *security.Cipher, assets AssetStore, fetchRemote RemoteFetcher) *Adapter {
	a := &Adapter{cfg: cfg, egress: egress, cipher: cipher, assets: assets, fetchRemote: fetchRemote, sessionURL: "https://muse.ai/api/session"}
	a.engine = newBrowserEngine(a.config, a.log)
	return a
}

func (a *Adapter) Provider() account.Provider { return account.ProviderMuse }

func (a *Adapter) SetLogger(logger *slog.Logger) { a.logger = logger }

func (a *Adapter) log() *slog.Logger {
	if a.logger != nil {
		return a.logger
	}
	return slog.Default()
}

func (a *Adapter) UpdateConfig(cfg Config) {
	a.mu.Lock()
	a.cfg = cfg
	a.mu.Unlock()
}

func (a *Adapter) config() Config {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg
}

// TestBrowser checks that an unsaved DevTools endpoint answers like the engine needs.
func (a *Adapter) TestBrowser(ctx context.Context, endpoint string) error {
	return probeBrowser(ctx, endpoint)
}

// withSession runs fn in a browser session for the account. The account's
// egress lease is held for the whole page and its node becomes the browser
// context's proxy.
func (a *Adapter) withSession(ctx context.Context, credential account.Credential, fn func(context.Context, session) error) error {
	jar, err := a.jar(credential.EncryptedAccessToken)
	if err != nil {
		return err
	}
	lease, err := a.egress.AcquireCredential(ctx, scope, credential)
	if err != nil {
		return err
	}
	defer lease.Release()
	proxy, err := browserProxy(lease.ProxyURL)
	if err != nil {
		return err
	}
	return fn(ctx, session{Cookies: jar, ProxyServer: proxy, Identity: a.identityOf(credential), Timezone: usTimezones[credential.ID%uint64(len(usTimezones))]})
}

// identityOf is the browser identity stored with the account. An account
// stored without one gets a stable pick by its ID.
func (a *Adapter) identityOf(credential account.Credential) browserIdentity {
	if credential.EncryptedState != "" {
		if raw, err := a.cipher.Decrypt(credential.EncryptedState); err == nil {
			var state map[string]string
			if json.Unmarshal([]byte(raw), &state) == nil {
				if identity, ok := identityByKey(state[stateBrowser]); ok {
					return identity
				}
			}
		}
	}
	return browserIdentities[credential.ID%uint64(len(browserIdentities))]
}

// browserProxy turns an egress proxy URL into Chrome's proxyServer. Chrome
// takes no proxy credentials on the command and speaks none of the tunnel
// protocols, like FlareSolverr.
func browserProxy(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	unsupported := &museError{status: http.StatusServiceUnavailable, code: "egress_unsupported",
		message: "Muse 浏览器只能使用无认证的 HTTP、HTTPS、SOCKS4 或 SOCKS5 出口，不支持带账号密码的代理和 Trojan、VLESS、SS、VMess 隧道"}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return "", unsupported
	}
	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "socks5h":
		scheme = "socks5"
	case "socks4a":
		scheme = "socks4"
	case "http", "https", "socks4", "socks5":
	default:
		return "", unsupported
	}
	return scheme + "://" + parsed.Host, nil
}

// jar decrypts a stored cookie jar.
func (a *Adapter) jar(encrypted string) ([]cookie, error) {
	if encrypted == "" {
		return nil, &museError{status: http.StatusUnauthorized, code: "invalid_credential", message: "Muse 账号没有 Cookie"}
	}
	raw, err := a.cipher.Decrypt(encrypted)
	if err != nil {
		return nil, err
	}
	var jar []cookie
	if err := json.Unmarshal([]byte(raw), &jar); err != nil {
		return nil, fmt.Errorf("decode muse cookie jar: %w", err)
	}
	return jar, nil
}

func encodeJar(jar []cookie) (string, error) {
	data, err := json.Marshal(jar)
	return string(data), err
}

func (a *Adapter) encryptJar(jar []cookie) (string, error) {
	encoded, err := encodeJar(jar)
	if err != nil {
		return "", err
	}
	return a.cipher.Encrypt(encoded)
}

// cookieValue returns the named cookie's value.
func cookieValue(jar []cookie, name string) string {
	for _, value := range jar {
		if value.Name == name {
			return value.Value
		}
	}
	return ""
}

// cookieHeader renders the jar as a Cookie request header.
func cookieHeader(jar []cookie) string {
	parts := make([]string, 0, len(jar))
	for _, value := range jar {
		if value.Name != "" && value.Value != "" {
			parts = append(parts, value.Name+"="+value.Value)
		}
	}
	return strings.Join(parts, "; ")
}

// transportError is an egress failure: no upstream response arrived.
type transportError struct{ err error }

func (e *transportError) Error() string { return "muse transport error: " + e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }
