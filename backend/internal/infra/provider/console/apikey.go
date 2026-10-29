package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"golang.org/x/sync/singleflight"
	"google.golang.org/protobuf/encoding/protowire"
)

// Free Console accounts must send the team's apiKeyId as x-apikey on every
// /v1 inference request; without it the upstream answers 429 "provision an API
// key". The key is read from the Console home page SSR payload and created
// only when the team has no usable key, because CreateApiKey has no
// idempotency guard.
const (
	consoleAPIKeyHeader       = "x-apikey"
	consoleAPIKeyName         = "xAI Chat Playground"
	consoleCreateAPIKeyPath   = "/auth_mgmt.AuthManagement/CreateApiKey"
	consoleHomeBodyLimit      = 8 << 20
	consoleAPIKeyCacheLimit   = 8192
	consoleAPIKeyACLEndpoint  = "api-key:endpoint:*"
	consoleAPIKeyACLModel     = "api-key:model:*"
	consoleAPIKeyScanDistance = 4096
	consoleHomeMaxRedirects   = 3
)

var (
	errConsoleAPIKeyUnavailable = errors.New("Console 首页未提供可用的 apiKeyId")
	consoleAPIKeyIDPattern      = regexp.MustCompile(`"apiKeyId"\s*:\s*"([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})"`)
	consoleTeamPathPattern      = regexp.MustCompile(`/team/([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})`)
	consoleTeamIDPattern        = regexp.MustCompile(`"teamId"\s*:\s*"([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})"`)
	consoleHomeUnescape         = strings.NewReplacer(`\\`, `\`, `\"`, `"`)
)

// CredentialCacheStore persists Console-only acceleration caches. Both values
// are recoverable from the SSO alone, so write failures never fail a request.
type CredentialCacheStore interface {
	UpdateConsoleAPIKeyID(ctx context.Context, accountID uint64, apiKeyID string) error
	UpdateConsoleWorkOSSession(ctx context.Context, accountID uint64, encryptedSession string) error
}

// consoleAPIKeyCache keeps resolved keys between the persistent write and the
// next candidate snapshot reload. Entries are keyed by account and SSO hash so
// a replaced SSO can never reuse another team's key.
type consoleAPIKeyCache struct {
	mu     sync.Mutex
	values map[string]string
	loads  singleflight.Group
}

func newConsoleAPIKeyCache() *consoleAPIKeyCache {
	return &consoleAPIKeyCache{values: make(map[string]string)}
}

func consoleAPIKeyCacheKey(credential account.Credential, ssoToken string) string {
	return fmt.Sprintf("%d|%s", credential.ID, security.HashToken(ssoToken))
}

func (c *consoleAPIKeyCache) get(key string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.values[key]
}

func (c *consoleAPIKeyCache) set(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.values) >= consoleAPIKeyCacheLimit {
		for existing := range c.values {
			delete(c.values, existing)
			break
		}
	}
	c.values[key] = value
}

func (c *consoleAPIKeyCache) invalidate(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if current, ok := c.values[key]; ok && (value == "" || current == value) {
		delete(c.values, key)
	}
}

// consoleAPIKey returns the current account's x-apikey. A rejected key skips
// both caches and is excluded from the team's candidates during recovery.
func (a *Adapter) consoleAPIKey(ctx context.Context, credential account.Credential, ssoToken string, lease *infraegress.Lease, rejectedKey string) (string, error) {
	key := consoleAPIKeyCacheKey(credential, ssoToken)
	if rejectedKey == "" {
		if value := a.apiKeys.get(key); value != "" {
			return value, nil
		}
		if value := strings.TrimSpace(credential.ConsoleAPIKeyID); value != "" {
			return value, nil
		}
	}
	// Coalesce concurrent resolutions per account: two parallel misses must not
	// both reach CreateApiKey and leave a stray key on the team.
	value, err, _ := a.apiKeys.loads.Do(key, func() (any, error) {
		if rejectedKey == "" {
			if cached := a.apiKeys.get(key); cached != "" {
				return cached, nil
			}
		}
		resolved, resolveErr := a.resolveConsoleAPIKey(ctx, credential, ssoToken, lease, rejectedKey)
		if resolveErr != nil {
			return "", resolveErr
		}
		a.apiKeys.set(key, resolved)
		if resolved != strings.TrimSpace(credential.ConsoleAPIKeyID) {
			a.saveConsoleAPIKeyID(ctx, credential.ID, resolved)
		}
		return resolved, nil
	})
	if err != nil {
		return "", err
	}
	resolved, _ := value.(string)
	if resolved == "" || resolved == rejectedKey {
		return "", errConsoleAPIKeyUnavailable
	}
	return resolved, nil
}

func (a *Adapter) invalidateConsoleAPIKey(credential account.Credential, ssoToken, value string) {
	a.apiKeys.invalidate(consoleAPIKeyCacheKey(credential, ssoToken), value)
}

func (a *Adapter) saveConsoleAPIKeyID(ctx context.Context, accountID uint64, apiKeyID string) {
	store := a.cacheStore()
	if store == nil || accountID == 0 {
		return
	}
	if err := store.UpdateConsoleAPIKeyID(context.WithoutCancel(ctx), accountID, apiKeyID); err != nil {
		a.logger().Warn("console_api_key_cache_write_failed", "account_id", accountID, "error", err)
	}
}

// Reuse an enabled key of the selected team, excluding one explicitly refused
// upstream. Create only when no usable key remains, then confirm its id.
func (a *Adapter) resolveConsoleAPIKey(ctx context.Context, credential account.Credential, ssoToken string, lease *infraegress.Lease, rejectedKey string) (string, error) {
	home, err := a.readConsoleHome(ctx, ssoToken, lease)
	if err != nil {
		return "", err
	}
	if keyID := home.availableAPIKey(rejectedKey); keyID != "" {
		return keyID, nil
	}
	if home.teamID == "" {
		return "", fmt.Errorf("%w: 缺少 teamId", errConsoleAPIKeyUnavailable)
	}
	if err := a.createConsoleAPIKey(ctx, credential, ssoToken, lease, home.teamID); err != nil {
		return "", err
	}
	// The create response carries the secret key body, not its id. Re-read the
	// home page so only the stable apiKeyId is ever used and stored.
	confirmed, err := a.readConsoleHome(ctx, ssoToken, lease)
	if err != nil {
		return "", err
	}
	if confirmed.teamID != home.teamID {
		return "", fmt.Errorf("%w: 创建后回读的 teamId 已改变", errConsoleAPIKeyUnavailable)
	}
	keyID := confirmed.availableAPIKey(rejectedKey)
	if keyID == "" {
		return "", fmt.Errorf("%w: 创建后回读未找到", errConsoleAPIKeyUnavailable)
	}
	return keyID, nil
}

type consoleHome struct {
	apiKeyIDs []string
	teamID    string
}

func (h consoleHome) availableAPIKey(rejectedKey string) string {
	for _, id := range h.apiKeyIDs {
		if id != rejectedKey {
			return id
		}
	}
	return ""
}

// readConsoleHome loads the Console landing page, which 307-redirects to
// /team/<teamId>. Only same-origin hops are followed; the team id is taken
// from the final path because it is authoritative for the SSO's team.
func (a *Adapter) readConsoleHome(ctx context.Context, ssoToken string, lease *infraegress.Lease) (consoleHome, error) {
	origin := consoleOrigin(a.config().BaseURL)
	current, err := url.Parse(origin + "/")
	if err != nil {
		return consoleHome{}, err
	}
	extraCookies := map[string]string{}
	for hop := 0; hop <= consoleHomeMaxRedirects; hop++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, current.String(), nil)
		if err != nil {
			return consoleHome{}, err
		}
		applyBrowserHeaders(request, ssoToken, lease)
		request.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		request.Header.Set("Sec-Fetch-Dest", "document")
		request.Header.Set("Sec-Fetch-Mode", "navigate")
		request.Header.Set("Sec-Fetch-Site", "none")
		request.Header.Del("Origin")
		request.Header.Del("Referer")
		if len(extraCookies) > 0 {
			parts := make([]string, 0, len(extraCookies))
			for name, value := range extraCookies {
				parts = append(parts, name+"="+value)
			}
			request.Header.Set("Cookie", joinCookieHeaders(request.Header.Get("Cookie"), strings.Join(parts, "; ")))
		}
		response, err := lease.DoDeferredForbidden(request)
		if err != nil {
			return consoleHome{}, err
		}
		if response.StatusCode >= 300 && response.StatusCode < 400 {
			location := response.Header.Get("Location")
			for _, cookie := range response.Cookies() {
				if cookie.Name != "sso" && cookie.Name != "sso-rw" {
					extraCookies[cookie.Name] = cookie.Value
				}
			}
			_, _, _ = provider.ReadDiagnosticBody(response.Body)
			_ = response.Body.Close()
			next, err := current.Parse(location)
			if location == "" || err != nil || !strings.EqualFold(next.Host, current.Host) {
				return consoleHome{}, fmt.Errorf("Console 首页跳转到非同源地址（status %d）", response.StatusCode)
			}
			current = next
			continue
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			data, _, _ := provider.ReadDiagnosticBody(response.Body)
			_ = response.Body.Close()
			if response.StatusCode == http.StatusForbidden && shouldInvalidateConsoleClearance(data) {
				lease.InvalidateClearance()
			}
			if response.StatusCode == http.StatusUnauthorized {
				return consoleHome{}, fmt.Errorf("%w: Console 首页返回 401", provider.ErrUnauthorized)
			}
			return consoleHome{}, fmt.Errorf("Console 首页返回 %d", response.StatusCode)
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, consoleHomeBodyLimit))
		_ = response.Body.Close()
		if err != nil {
			return consoleHome{}, err
		}
		teamID := ""
		if match := consoleTeamPathPattern.FindStringSubmatch(current.Path); match != nil {
			teamID = match[1]
		}
		return parseConsoleHome(data, teamID), nil
	}
	return consoleHome{}, fmt.Errorf("Console 首页跳转次数超过 %d", consoleHomeMaxRedirects)
}

// The Next.js payload contains raw JSON or JSON string literals escaped once
// or twice. Decode one escape level at a time so quotes inside key names stay
// valid while the containing key object is parsed.
func parseConsoleHome(data []byte, activeTeamID string) consoleHome {
	text := string(data)
	result := consoleHome{teamID: strings.ToLower(strings.TrimSpace(activeTeamID))}
	seen := make(map[string]bool)
	for depth := 0; depth <= 2; depth++ {
		if result.teamID == "" {
			if match := consoleTeamPathPattern.FindStringSubmatch(text); match != nil {
				result.teamID = strings.ToLower(match[1])
			} else if match := consoleTeamIDPattern.FindStringSubmatch(text); match != nil {
				result.teamID = strings.ToLower(match[1])
			}
		}
		matches := consoleAPIKeyIDPattern.FindAllStringSubmatchIndex(text, -1)
		for _, match := range matches {
			key, ok := parseConsoleHomeKey(text, match)
			if !ok || key.Disabled || (key.TeamID != "" && result.teamID != "" && !strings.EqualFold(key.TeamID, result.teamID)) {
				continue
			}
			id := strings.ToLower(key.APIKeyID)
			if !seen[id] {
				result.apiKeyIDs = append(result.apiKeyIDs, id)
				seen[id] = true
			}
		}
		next := consoleHomeUnescape.Replace(text)
		if next == text {
			break
		}
		text = next
	}
	return result
}

type consoleHomeAPIKey struct {
	APIKeyID string `json:"apiKeyId"`
	TeamID   string `json:"teamId"`
	Disabled bool   `json:"disabled"`
}

// Decode the containing key object, not a span extending into the next key:
// teamId and disabled can appear before or after apiKeyId.
func parseConsoleHomeKey(text string, match []int) (consoleHomeAPIKey, bool) {
	left, right := max(0, match[0]-consoleAPIKeyScanDistance), min(len(text), match[1]+consoleAPIKeyScanDistance)
	for end := match[0]; end > left; {
		start := strings.LastIndexByte(text[left:end], '{')
		if start < 0 {
			break
		}
		start += left
		var key consoleHomeAPIKey
		if json.NewDecoder(strings.NewReader(text[start:right])).Decode(&key) == nil && strings.EqualFold(key.APIKeyID, text[match[2]:match[3]]) {
			return key, true
		}
		end = start
	}
	return consoleHomeAPIKey{}, false
}

func (a *Adapter) createConsoleAPIKey(ctx context.Context, credential account.Credential, ssoToken string, lease *infraegress.Lease, teamID string) error {
	endpoint := consoleOrigin(a.config().BaseURL) + consoleCreateAPIKeyPath
	response, err := a.doDPoPRequestWithOptions(ctx, credential, ssoToken, lease, http.MethodPost, endpoint, consoleCreateAPIKeyBody(teamID), dpopRequestOptions{
		contentType: "application/connect+proto",
		accept:      "*/*",
		referer:     consoleOrigin(a.config().BaseURL) + "/team/" + url.PathEscape(teamID) + "/chat",
		skipAPIKey:  true,
	})
	if err != nil {
		return err
	}
	data, _, _ := provider.ReadDiagnosticBody(response.Body)
	_ = response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == http.StatusForbidden && shouldInvalidateConsoleClearance(data) {
			lease.InvalidateClearance()
		}
		return fmt.Errorf("Console CreateApiKey 返回 %d", response.StatusCode)
	}
	a.logger().Info("console_api_key_created", "account_id", credential.ID)
	return nil
}

// consoleCreateAPIKeyBody builds the Connect envelope (flag byte + big-endian
// length) around CreateApiKeyRequest{name=1, team_id=2, acls=3...}.
func consoleCreateAPIKeyBody(teamID string) []byte {
	var payload []byte
	payload = protowire.AppendTag(payload, 1, protowire.BytesType)
	payload = protowire.AppendString(payload, consoleAPIKeyName)
	payload = protowire.AppendTag(payload, 2, protowire.BytesType)
	payload = protowire.AppendString(payload, teamID)
	for _, acl := range []string{consoleAPIKeyACLEndpoint, consoleAPIKeyACLModel} {
		payload = protowire.AppendTag(payload, 3, protowire.BytesType)
		payload = protowire.AppendString(payload, acl)
	}
	envelope := make([]byte, 5, 5+len(payload))
	length := len(payload)
	envelope[1], envelope[2], envelope[3], envelope[4] = byte(length>>24), byte(length>>16), byte(length>>8), byte(length)
	return append(envelope, payload...)
}

// Key provisioning failures (429/403) and explicitly rejected keys (400) are
// repairable by resolving the team's key again. Other 400s remain terminal.
func isConsoleRepairableAPIKeyBody(status int, body []byte) bool {
	code, _, message := provider.ExtractUpstreamErrorMetadata(body)
	if provider.IsBadRequestAPIKeyRejection(status, code, message) {
		return true
	}
	if status != http.StatusTooManyRequests && status != http.StatusForbidden {
		return false
	}
	text := strings.ToLower(string(body))
	return strings.Contains(text, "provision an api key") || strings.Contains(text, "not the owner")
}

func consoleOrigin(baseURL string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	baseURL = strings.TrimSuffix(baseURL, "/v1")
	if parsed, err := url.Parse(baseURL); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		return parsed.Scheme + "://" + parsed.Host
	}
	return baseURL
}
