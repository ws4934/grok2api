package muse

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// The login cookie that /api/session needs; the 48-hour hatch_vml is minted from it.
const (
	cookieSession = "hatch_sess"
	cookieVML     = "hatch_vml"
)

const sessionUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"

// --- credential import / export (CredentialCodecAdapter) ---

// importRecord is this service's export line; Cookie also accepts a header string.
type importRecord struct {
	Cookies []cookie `json:"cookies,omitempty"`
	Cookie  string   `json:"cookie,omitempty"`
	Name    string   `json:"name,omitempty"`
	Email   string   `json:"email,omitempty"`
}

// ParseImportedCredentials accepts a Cookie-Editor JSON export (one account),
// JSON/JSONL records of this service's export, or one Cookie header per line
// ("label | " prefix optional). Each account needs hatch_sess; whether it
// still works is checked by PrepareImportedCredential.
func (a *Adapter) ParseImportedCredentials(data []byte) ([]provider.CredentialSeed, error) {
	trimmed := strings.TrimSpace(strings.TrimPrefix(string(data), "\ufeff"))
	if trimmed == "" {
		return nil, nil
	}
	var records []importRecord
	switch {
	case isCookieExport(trimmed):
		var jar []cookie
		if err := json.Unmarshal([]byte(trimmed), &jar); err != nil {
			return nil, fmt.Errorf("解析 Muse Cookie 导出: %w", err)
		}
		records = []importRecord{{Cookies: jar}}
	case trimmed[0] == '{' || trimmed[0] == '[':
		var err error
		if records, err = provider.DecodeCredentialJSONEntries[importRecord]([]byte(trimmed), string(account.ProviderMuse), 0); err != nil {
			return nil, fmt.Errorf("解析 Muse 账号 JSON: %w", err)
		}
	default:
		for _, line := range strings.Split(trimmed, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			record := importRecord{Cookie: line}
			// A "|" inside the cookie is not a separator: the label has no "=".
			if label, rest, ok := strings.Cut(line, "|"); ok && !strings.Contains(label, "=") {
				record = importRecord{Name: strings.TrimSpace(label), Cookie: strings.TrimSpace(rest)}
			}
			records = append(records, record)
		}
	}
	seeds := make([]provider.CredentialSeed, 0, len(records))
	for _, record := range records {
		jar := record.Cookies
		if len(jar) == 0 {
			jar = parseCookieHeader(record.Cookie)
		}
		jar = museCookies(jar)
		sess := cookieValue(jar, cookieSession)
		if sess == "" {
			continue
		}
		encoded, err := encodeJar(jar)
		if err != nil {
			return nil, err
		}
		seeds = append(seeds, provider.CredentialSeed{
			Provider: account.ProviderMuse, AuthType: account.AuthTypeOAuth,
			Name: strings.TrimSpace(record.Name), Email: strings.TrimSpace(record.Email),
			SourceKey:   "muse:" + security.HashToken(sess)[:24],
			AccessToken: encoded, RefreshToken: encoded,
		})
	}
	if len(seeds) == 0 {
		return nil, errors.New("没有找到有效的 Muse Cookie（需要包含 hatch_sess）")
	}
	return seeds, nil
}

// isCookieExport recognizes a browser extension export: a JSON array of
// cookie objects (name + value), not of account records.
func isCookieExport(text string) bool {
	if !strings.HasPrefix(text, "[") {
		return false
	}
	var items []map[string]json.RawMessage
	if json.Unmarshal([]byte(text), &items) != nil || len(items) == 0 {
		return false
	}
	for _, item := range items {
		_, name := item["name"]
		_, value := item["value"]
		_, records := item["cookies"]
		if !name || !value || records {
			return false
		}
	}
	return true
}

// parseCookieHeader turns "a=1; b=2" into cookies with the domains muse.ai
// sets: host-only for __Host- cookies and hatch_native_auth_device, the
// whole site for the rest.
func parseCookieHeader(header string) []cookie {
	header = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(header), "Cookie:"))
	var jar []cookie
	for _, part := range strings.Split(header, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if !ok || name == "" || value == "" {
			continue
		}
		item := cookie{Name: name, Value: value, Domain: ".muse.ai", Path: "/", Secure: true}
		if strings.HasPrefix(name, "__Host-") || name == "hatch_native_auth_device" {
			item.Domain, item.HostOnly = "muse.ai", true
		}
		jar = append(jar, item)
	}
	return jar
}

// museCookies keeps only muse.ai cookies, last one wins per name and domain.
func museCookies(jar []cookie) []cookie {
	var out []cookie
	for _, value := range jar {
		domain := strings.ToLower(strings.TrimPrefix(value.Domain, "."))
		if value.Name == "" || (domain != "" && domain != "muse.ai" && !strings.HasSuffix(domain, ".muse.ai")) {
			continue
		}
		if value.Domain == "" {
			value.Domain = ".muse.ai"
		}
		if value.Path == "" {
			value.Path = "/"
		}
		out = upsertCookie(out, value)
	}
	return out
}

func upsertCookie(jar []cookie, value cookie) []cookie {
	for index, existing := range jar {
		if existing.Name == value.Name && existing.HostOnly == value.HostOnly {
			jar[index] = value
			return jar
		}
	}
	return append(jar, value)
}

// MarshalCredentials exports one JSON line per account with its cookie jar.
func (a *Adapter) MarshalCredentials(values []provider.CredentialSeed) ([]byte, error) {
	var builder strings.Builder
	for _, seed := range values {
		var jar []cookie
		if err := json.Unmarshal([]byte(seed.RefreshToken), &jar); err != nil {
			return nil, fmt.Errorf("decode muse cookie jar: %w", err)
		}
		encoded, err := json.Marshal(importRecord{Cookies: jar, Name: seed.Name, Email: seed.Email})
		if err != nil {
			return nil, err
		}
		builder.Write(encoded)
		builder.WriteByte('\n')
	}
	return []byte(builder.String()), nil
}

// PrepareImportedCredential checks the session with muse.ai before the
// account is stored and keeps the freshly minted hatch_vml.
func (a *Adapter) PrepareImportedCredential(ctx context.Context, seed provider.CredentialSeed) (provider.CredentialSeed, error) {
	var jar []cookie
	if err := json.Unmarshal([]byte(seed.RefreshToken), &jar); err != nil || cookieValue(jar, cookieSession) == "" {
		return seed, &provider.CredentialRefreshError{Status: http.StatusBadRequest, Code: "muse_cookie_invalid", Message: "缺少 Muse 登录 Cookie（hatch_sess）"}
	}
	info, jar, expiresAt, err := a.renewSession(ctx, a.pendingCredential(ctx, seed.SourceKey), jar)
	if err != nil {
		return seed, err
	}
	encoded, err := encodeJar(jar)
	if err != nil {
		return seed, err
	}
	seed.AccessToken, seed.RefreshToken, seed.ExpiresAt = encoded, encoded, expiresAt
	if info.VMID != "" {
		seed.UserID = info.VMID
	}
	// Accounts need a name and muse.ai reports no email: name it after the VM.
	if strings.TrimSpace(seed.Name) == "" {
		id := cmp.Or(info.VMID, strings.TrimPrefix(seed.SourceKey, "muse:"))
		seed.Name = "muse-" + id[:min(8, len(id))]
	}
	return seed, nil
}

// pendingCredential gives an import without an account row its own sticky
// egress identity; re-importing an existing account keeps its egress.
func (a *Adapter) pendingCredential(ctx context.Context, key string) account.Credential {
	if existing, ok := infraegress.ExistingAccount(ctx); ok && existing.Provider == account.ProviderMuse {
		return existing
	}
	return account.Credential{Provider: account.ProviderMuse, EgressIdentity: "muse_pending_" + security.HashToken(key)[:24]}
}

// --- credential refresh (CredentialRefreshAdapter) ---

// RefreshCredential mints a new hatch_vml from the stored jar. The other
// cookies do not rotate.
func (a *Adapter) RefreshCredential(ctx context.Context, credential account.Credential) (provider.RefreshedCredential, error) {
	if credential.EncryptedRefreshToken == "" {
		return provider.RefreshedCredential{}, &provider.CredentialRefreshError{Code: "missing_refresh_token", Permanent: true}
	}
	jar, err := a.jar(credential.EncryptedRefreshToken)
	if err != nil {
		return provider.RefreshedCredential{}, err
	}
	_, jar, expiresAt, err := a.renewSession(ctx, credential, jar)
	if err != nil {
		return provider.RefreshedCredential{}, err
	}
	encrypted, err := a.encryptJar(jar)
	if err != nil {
		return provider.RefreshedCredential{}, err
	}
	return provider.RefreshedCredential{EncryptedAccessToken: encrypted, EncryptedRefreshToken: encrypted, ExpiresAt: expiresAt}, nil
}

func (a *Adapter) CredentialMetadata(account.Credential) provider.CredentialMetadata {
	return provider.CredentialMetadata{CredentialKind: "muse-cookie"}
}

// --- account identity (AccountIdentityAdapter) ---

// SyncAccountIdentity reads the account's cloud VM id, the only stable id
// /api/session reports.
func (a *Adapter) SyncAccountIdentity(ctx context.Context, credential account.Credential) (provider.AccountIdentity, error) {
	jar, err := a.jar(credential.EncryptedAccessToken)
	if err != nil {
		return provider.AccountIdentity{}, err
	}
	info, _, _, err := a.renewSession(ctx, credential, jar)
	if err != nil {
		return provider.AccountIdentity{}, err
	}
	return provider.AccountIdentity{UserID: info.VMID}, nil
}

// --- quota (QuotaAdapter) ---

// SyncQuota reads the weekly usage panel in the browser. A failed read
// returns an error, so stored windows stay.
func (a *Adapter) SyncQuota(ctx context.Context, credential account.Credential) (provider.QuotaSnapshot, error) {
	var read usage
	err := a.withSession(ctx, credential, func(ctx context.Context, s session) error {
		var err error
		read, err = a.engine.Usage(ctx, s)
		return err
	})
	if err != nil {
		return provider.QuotaSnapshot{}, classify(err)
	}
	now := time.Now().UTC()
	return provider.QuotaSnapshot{SyncedAt: now, Windows: []account.QuotaWindow{usageWindow(read, now)}}, nil
}

// usageWindow maps the panel's percentage to a 100-point window.
func usageWindow(read usage, now time.Time) account.QuotaWindow {
	used := math.Min(100, math.Max(0, read.UsedPercent))
	window := account.QuotaWindow{
		Mode: quotaModeWeekly, Total: 100, Remaining: 100 - int(math.Ceil(used)), UsagePercent: used,
		Source: account.QuotaSourceUpstream, SyncedAt: &now,
	}
	if !read.ResetAt.IsZero() {
		reset := read.ResetAt.UTC()
		window.ResetAt = &reset
	}
	return window
}

// QuotaModeSyncReadsSnapshot: SyncQuotaMode is a filtered SyncQuota, so
// post-request refreshes share the snapshot interval floor.
func (a *Adapter) QuotaModeSyncReadsSnapshot() {}

func (a *Adapter) SyncQuotaMode(ctx context.Context, credential account.Credential, mode string) (account.QuotaWindow, error) {
	if mode != quotaModeWeekly {
		return account.QuotaWindow{}, fmt.Errorf("%w: %s", account.ErrQuotaNotLimited, mode)
	}
	snapshot, err := a.SyncQuota(ctx, credential)
	if err != nil {
		return account.QuotaWindow{}, err
	}
	return snapshot.Windows[0], nil
}

// ListModels: the catalog is static (models.go) and every account serves it.
func (a *Adapter) ListModels(context.Context, account.Credential) ([]string, error) {
	return nil, nil
}

// --- /api/session ---

type sessionInfo struct {
	Status  string `json:"status"`
	VMID    string `json:"vm_id"`
	VMState string `json:"vm_state"`
}

// renewSession calls /api/session through the account's egress. muse.ai
// answers with a new 48-hour hatch_vml, merged into the jar; expiresAt is
// when it lapses. A 401 means hatch_sess is gone: the cookies must be
// imported again.
func (a *Adapter) renewSession(ctx context.Context, credential account.Credential, jar []cookie) (sessionInfo, []cookie, time.Time, error) {
	lease, err := a.egress.AcquireCredential(infraegress.WithUserAgent(ctx, sessionUserAgent), scope, credential)
	if err != nil {
		return sessionInfo{}, nil, time.Time{}, err
	}
	defer lease.Release()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, a.sessionURL, nil)
	if err != nil {
		return sessionInfo{}, nil, time.Time{}, err
	}
	userAgent := lease.UserAgent
	if userAgent == "" {
		userAgent = sessionUserAgent
	}
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Origin", "https://muse.ai")
	request.Header.Set("Referer", "https://muse.ai/thread/new")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	request.Header.Set("Sec-Fetch-Mode", "cors")
	request.Header.Set("Sec-Fetch-Dest", "empty")
	request.Header.Set("Cookie", cookieHeader(jar))
	response, err := lease.Do(request)
	if err != nil {
		a.egress.FeedbackForScope(context.WithoutCancel(ctx), scope, lease.NodeID, 0, err)
		return sessionInfo{}, nil, time.Time{}, &transportError{err: err}
	}
	defer response.Body.Close()
	a.egress.FeedbackForScope(context.WithoutCancel(ctx), scope, lease.NodeID, response.StatusCode, nil)
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return sessionInfo{}, nil, time.Time{}, &transportError{err: err}
	}
	switch {
	case response.StatusCode == http.StatusUnauthorized:
		return sessionInfo{}, nil, time.Time{}, &provider.CredentialRefreshError{Status: response.StatusCode, Code: "refresh_token_invalidated", Message: "Muse 会话已失效，请重新导入 Cookie", Permanent: true}
	case response.StatusCode == http.StatusForbidden:
		// 403 is an egress, region or account restriction, not proof the cookies died.
		return sessionInfo{}, nil, time.Time{}, &provider.CredentialRefreshError{Status: response.StatusCode, Code: "muse_session_forbidden", Message: "muse.ai 拒绝了访问，请检查出口地区或账号状态"}
	case response.StatusCode != http.StatusOK:
		return sessionInfo{}, nil, time.Time{}, &provider.CredentialRefreshError{Status: response.StatusCode, Code: "muse_session_failed", Message: fmt.Sprintf("muse.ai /api/session 返回 HTTP %d", response.StatusCode)}
	}
	var info sessionInfo
	if json.Unmarshal(body, &info) != nil || info.Status != "assigned" {
		return sessionInfo{}, nil, time.Time{}, &provider.CredentialRefreshError{Status: response.StatusCode, Code: "muse_session_unassigned", Message: "muse.ai 没有为账号分配工作区，请在官网检查账号"}
	}
	jar = mergeSetCookies(jar, response.Cookies(), time.Now())
	return info, jar, jarExpiry(jar), nil
}

// mergeSetCookies applies Set-Cookie headers: a cookie without Domain is
// host-only, Max-Age<=0 deletes.
func mergeSetCookies(jar []cookie, updates []*http.Cookie, now time.Time) []cookie {
	jar = append([]cookie(nil), jar...)
	for _, update := range updates {
		item := cookie{Name: update.Name, Value: update.Value, Path: update.Path, HTTPOnly: update.HttpOnly, Secure: update.Secure, Domain: "muse.ai", HostOnly: true}
		if domain := strings.TrimPrefix(update.Domain, "."); domain != "" {
			item.Domain, item.HostOnly = "."+domain, false
		}
		if item.Path == "" {
			item.Path = "/"
		}
		switch update.SameSite {
		case http.SameSiteLaxMode:
			item.SameSite = "lax"
		case http.SameSiteStrictMode:
			item.SameSite = "strict"
		case http.SameSiteNoneMode:
			item.SameSite = "no_restriction"
		}
		if update.MaxAge < 0 || update.Value == "" {
			kept := jar[:0]
			for _, existing := range jar {
				if existing.Name != item.Name || existing.HostOnly != item.HostOnly {
					kept = append(kept, existing)
				}
			}
			jar = kept
			continue
		}
		switch {
		case update.MaxAge > 0:
			item.Expires = float64(now.Add(time.Duration(update.MaxAge) * time.Second).Unix())
		case !update.Expires.IsZero():
			item.Expires = float64(update.Expires.Unix())
		}
		jar = upsertCookie(jar, item)
	}
	return jar
}

// jarExpiry is when hatch_vml lapses; without a dated one the account is
// renewed again within a day.
func jarExpiry(jar []cookie) time.Time {
	for _, value := range jar {
		if value.Name == cookieVML && value.Expires > 0 {
			return time.Unix(int64(value.Expires), 0).UTC()
		}
	}
	return time.Now().UTC().Add(24 * time.Hour)
}
