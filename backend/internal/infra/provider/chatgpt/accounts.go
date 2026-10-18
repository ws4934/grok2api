package chatgpt

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/channel"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/chatgpt/browserprofile"
)

const oauthProfile = "platform_pkce_v1"
const oauthClientID = "app_2SKx67EdpoN0G6j64fRvigXD"
const oauthRedirect = "https://platform.openai.com/auth/callback"

func (a *Adapter) ParseImport(data []byte) ([]channel.ImportRecord, error) {
	if len(data) > 16<<20 {
		return nil, errors.New("import exceeds 16 MiB")
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, errors.New("empty import")
	}
	var values []json.RawMessage
	if data[0] == '[' {
		if err := json.Unmarshal(data, &values); err != nil {
			return nil, errors.New("invalid JSON array")
		}
	} else if json.Valid(data) {
		var envelope struct {
			Accounts []json.RawMessage `json:"accounts"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			return nil, errors.New("invalid JSON")
		}
		if envelope.Accounts != nil {
			values = envelope.Accounts
		} else {
			values = []json.RawMessage{data}
		}
	} else {
		scanner := bufio.NewScanner(bytes.NewReader(data))
		scanner.Buffer(make([]byte, 4096), 1<<20)
		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				continue
			}
			if line[0] == '{' {
				values = append(values, append([]byte(nil), line...))
			} else {
				b, _ := json.Marshal(map[string]string{"access_token": string(line)})
				values = append(values, b)
			}
		}
		if scanner.Err() != nil {
			return nil, errors.New("import line too large")
		}
	}
	if len(values) > 10000 {
		return nil, errors.New("import exceeds 10000 records")
	}
	out := make([]channel.ImportRecord, 0, len(values))
	for _, raw := range values {
		out = append(out, parseImportRecord(raw))
	}
	return out, nil
}
func parseImportRecord(raw json.RawMessage) channel.ImportRecord {
	var v struct {
		Name        string               `json:"name"`
		Credentials *channel.Credentials `json:"credentials"`
		AT          string               `json:"access_token"`
		AT2         string               `json:"accessToken"`
		Token       string               `json:"token"`
		RT          string               `json:"refresh_token"`
		RT2         string               `json:"refreshToken"`
		IDToken     string               `json:"id_token"`
		Profile     string               `json:"client_profile"`
		Device      string               `json:"oai_device_id"`
		Session     string               `json:"oai_session_id"`
		Browser     json.RawMessage      `json:"browser_profile"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return channel.ImportRecord{Error: "invalid credential record"}
	}
	c := channel.Credentials{Version: 1, AccessToken: firstNonEmptyString([]string{v.AT, v.AT2, v.Token}, ""), RefreshToken: firstNonEmptyString([]string{v.RT, v.RT2}, ""), IDToken: v.IDToken, ClientProfile: v.Profile, DeviceID: v.Device, SessionID: v.Session, BrowserProfile: ""}
	if len(v.Browser) > 0 && string(v.Browser) != "null" {
		if v.Browser[0] == '"' {
			_ = json.Unmarshal(v.Browser, &c.BrowserProfile)
		} else {
			var p browserprofile.Profile
			if json.Unmarshal(v.Browser, &p) != nil || !p.Valid() {
				return channel.ImportRecord{Error: "unsupported browser profile"}
			}
			encoded, _ := json.Marshal(p)
			c.BrowserProfile = string(encoded)
		}
	}
	if v.Credentials != nil {
		c = *v.Credentials
	}
	if c.Version == 0 {
		c.Version = 1
	}
	if c.Version != 1 {
		return channel.ImportRecord{Error: "unsupported credentials version"}
	}
	if c.AccessToken == "" && c.RefreshToken == "" {
		return channel.ImportRecord{Error: "access_token or explicit refresh_token is required"}
	}
	if len(c.AccessToken) > 32000 || len(c.RefreshToken) > 16000 || len(c.IDToken) > 16000 {
		return channel.ImportRecord{Error: "credential exceeds size limit"}
	}
	if _, err := wire(c); err != nil {
		return channel.ImportRecord{Error: "unsupported browser profile"}
	}
	if c.DeviceID == "" {
		c.DeviceID = firstUUID()
	}
	if c.SessionID == "" {
		c.SessionID = firstUUID()
	}
	if c.ExpiresAt == nil {
		c.ExpiresAt = tokenExpiry(c.AccessToken)
	}
	return channel.ImportRecord{Name: v.Name, Credentials: c}
}
func (a *Adapter) Prepare(ctx context.Context, c channel.Credentials) (channel.Credentials, error) {
	if c.DeviceID == "" {
		c.DeviceID = firstUUID()
	}
	if c.SessionID == "" {
		c.SessionID = firstUUID()
	}
	if c.AccessToken == "" {
		return a.Refresh(ctx, c)
	}
	if c.ExpiresAt == nil {
		c.ExpiresAt = tokenExpiry(c.AccessToken)
	}
	return c, nil
}
func tokenExpiry(token string) *time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	b, e := base64.RawURLEncoding.DecodeString(parts[1])
	if e != nil {
		return nil
	}
	var v struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(b, &v) != nil || v.Exp <= 0 {
		return nil
	}
	t := time.Unix(v.Exp, 0).UTC()
	return &t
}
func (a *Adapter) Validate(ctx context.Context, c channel.Credentials) (channel.Snapshot, error) {
	var out channel.Snapshot
	w, e := wire(c)
	if e != nil {
		return out, e
	}
	get := func(method, path string, body any) (map[string]any, error) {
		r, e := a.image.do(ctx, method, path, w, body, nil, false)
		if e != nil {
			return nil, e
		}
		var v map[string]any
		e = decodeOpenAIJSON(r, &v, path)
		return v, e
	}
	me, e := get("GET", "/backend-api/me", nil)
	if e != nil {
		return out, e
	}
	out.Identity = channel.Identity{UserID: aiString(me, "id", "user_id"), Email: aiString(me, "email")}
	if out.Identity.UserID == "" {
		return out, errors.New("upstream did not return a stable user identity")
	}
	accounts, e := get("GET", "/backend-api/accounts/check/v4-2023-04-27?timezone_offset_min=0", nil)
	if e != nil {
		return out, e
	}
	catalog, _ := accounts["accounts"].(map[string]any)
	def, _ := catalog["default"].(map[string]any)
	acct, _ := def["account"].(map[string]any)
	out.Identity.Plan = aiString(acct, "plan_type")
	out.Identity.WorkspaceID = aiString(acct, "account_id", "id")
	models, e := get("GET", "/backend-api/models", nil)
	if e != nil {
		return out, e
	}
	for _, raw := range toAnyList(models["models"]) {
		if m, ok := raw.(map[string]any); ok {
			if slug := aiString(m, "slug"); slug != "" {
				out.Models = append(out.Models, slug)
			}
		}
	}
	if len(out.Models) == 0 {
		return out, errors.New("upstream model catalog is empty")
	}
	init, e := get("POST", "/backend-api/conversation/init", map[string]any{"gizmo_id": nil, "requested_default_model": nil, "conversation_id": nil, "timezone_offset_min": 0})
	if e != nil {
		return out, e
	}
	now := time.Now().UTC()
	for _, feature := range []string{"reason", "image_gen", "file_upload"} {
		q := channel.FeatureQuota{Feature: feature, ObservedAt: now}
		for _, raw := range toAnyList(init["limits_progress"]) {
			m, _ := raw.(map[string]any)
			if aiString(m, "feature_name", "feature") == feature {
				if n, ok := m["remaining"].(float64); ok && n >= 0 {
					v := int64(n)
					q.Remaining = &v
				}
				if s := aiString(m, "reset_at", "resets_at"); s != "" {
					if t, e := time.Parse(time.RFC3339, s); e == nil {
						q.ResetAt = &t
					}
				}
			}
		}
		out.Quotas = append(out.Quotas, q)
	}
	return out, nil
}
func randomOAuthValue() (string, error) {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func (a *Adapter) StartOAuth(ctx context.Context) (channel.OAuthStart, error) {
	state, e := randomOAuthValue()
	if e != nil {
		return channel.OAuthStart{}, e
	}
	verifier, e := randomOAuthValue()
	if e != nil {
		return channel.OAuthStart{}, e
	}
	nonce, e := randomOAuthValue()
	if e != nil {
		return channel.OAuthStart{}, e
	}
	hash := sha256.Sum256([]byte(verifier))
	q := url.Values{"issuer": {"https://auth.openai.com"}, "client_id": {oauthClientID}, "audience": {"https://api.openai.com/v1"}, "redirect_uri": {oauthRedirect}, "device_id": {firstUUID()}, "scope": {"openid profile email offline_access"}, "response_type": {"code"}, "response_mode": {"query"}, "state": {state}, "nonce": {nonce}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}, "code_challenge_method": {"S256"}, "screen_hint": {"login_or_signup"}, "max_age": {"0"}}
	return channel.OAuthStart{URL: a.authURL + "?" + q.Encode(), State: state, Verifier: verifier, RedirectURI: oauthRedirect, Profile: oauthProfile}, nil
}
func (a *Adapter) ExchangeOAuth(ctx context.Context, start channel.OAuthStart, callback string) (channel.Credentials, error) {
	u, e := url.Parse(callback)
	if e != nil || u.User != nil || u.Scheme != "https" || u.Host != "platform.openai.com" || u.Path != "/auth/callback" || u.Fragment != "" {
		return channel.Credentials{}, errors.New("invalid OAuth callback URL")
	}
	q, e := url.ParseQuery(u.RawQuery)
	if e != nil || len(q["state"]) != 1 || q.Get("state") == "" || q.Get("state") != start.State || len(q["code"]) != 1 || q.Get("code") == "" || q.Get("error") != "" {
		return channel.Credentials{}, errors.New("OAuth callback state or code is invalid")
	}
	if start.Profile != oauthProfile || start.RedirectURI != oauthRedirect {
		return channel.Credentials{}, errors.New("OAuth profile mismatch")
	}
	return a.exchange(ctx, map[string]string{"client_id": oauthClientID, "grant_type": "authorization_code", "code": q.Get("code"), "redirect_uri": start.RedirectURI, "code_verifier": start.Verifier}, channel.Credentials{Version: 1, ClientProfile: oauthProfile})
}
func (a *Adapter) Refresh(ctx context.Context, c channel.Credentials) (channel.Credentials, error) {
	if c.RefreshToken == "" {
		return c, errors.New("refresh token is required")
	}
	if c.ClientProfile != oauthProfile {
		return c, errors.New("refresh token requires a supported explicit client_profile")
	}
	return a.exchange(ctx, map[string]string{"client_id": oauthClientID, "grant_type": "refresh_token", "refresh_token": c.RefreshToken}, c)
}
func (a *Adapter) exchange(ctx context.Context, body map[string]string, old channel.Credentials) (channel.Credentials, error) {
	raw, e := json.Marshal(body)
	if e != nil {
		return old, e
	}
	r, e := http.NewRequestWithContext(ctx, "POST", a.tokenURL, bytes.NewReader(raw))
	if e != nil {
		return old, errors.New("invalid OAuth token endpoint")
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "https://platform.openai.com")
	r.Header.Set("Referer", "https://platform.openai.com/")
	client := *a.oauth
	if env := channel.EnvironmentFrom(ctx); env.ProxyURL != "" {
		egress, err := parseEgressProxy(env.ProxyURL)
		if err != nil {
			return old, errors.New("invalid OAuth egress")
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		egress.apply(transport)
		defer transport.CloseIdleConnections()
		client.Transport = transport
	}
	resp, e := client.Do(r)
	if e != nil {
		return old, &transportError{cause: e}
	}
	defer resp.Body.Close()
	var v struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		ID      string `json:"id_token"`
		Expires int64  `json:"expires_in"`
		Error   string `json:"error"`
	}
	if e := decodeJSONLimited(resp.Body, &v); e != nil {
		return old, errors.New("invalid OAuth token response")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || v.Access == "" {
		if v.Error == "invalid_grant" {
			return old, &channel.Failure{Kind: channel.FailureAuth, Code: "invalid_grant", Message: "credentials require reauthorization", Unsent: true}
		}
		return old, errors.New("OAuth token endpoint rejected exchange")
	}
	old.AccessToken = v.Access
	if old.DeviceID == "" {
		old.DeviceID = firstUUID()
	}
	if old.SessionID == "" {
		old.SessionID = firstUUID()
	}
	if v.Refresh != "" {
		old.RefreshToken = v.Refresh
	}
	if v.ID != "" {
		old.IDToken = v.ID
	}
	old.ExpiresAt = tokenExpiry(v.Access)
	if v.Expires > 0 {
		t := time.Now().UTC().Add(time.Duration(v.Expires) * time.Second)
		old.ExpiresAt = &t
	}
	return old, nil
}
