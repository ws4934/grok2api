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
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/chatgpt/browserprofile"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

const oauthProfile = "platform_pkce_v1"
const oauthClientID = "app_2SKx67EdpoN0G6j64fRvigXD"
const oauthRedirect = "https://platform.openai.com/auth/callback"
const defaultAccountName = "ChatGPT account"

// credentials is a decrypted ChatGPT login: the tokens plus the provider state
// stored encrypted beside them.
type credentials struct {
	AccessToken    string
	RefreshToken   string
	IDToken        string
	ClientProfile  string
	DeviceID       string
	SessionID      string
	BrowserProfile string
	// ExpiresAt is the access token expiry; it is not part of the state.
	ExpiresAt time.Time
}

func credentialsOf(accessToken, refreshToken string, state map[string]string) credentials {
	return credentials{
		AccessToken: accessToken, RefreshToken: refreshToken, ExpiresAt: tokenExpiry(accessToken),
		IDToken: state["id_token"], ClientProfile: state["client_profile"], DeviceID: state["device_id"],
		SessionID: state["session_id"], BrowserProfile: state["browser_profile"],
	}
}

func (c credentials) state() map[string]string {
	state := map[string]string{}
	for key, value := range map[string]string{"id_token": c.IDToken, "client_profile": c.ClientProfile, "device_id": c.DeviceID, "session_id": c.SessionID, "browser_profile": c.BrowserProfile} {
		if value != "" {
			state[key] = value
		}
	}
	return state
}

func (a *Adapter) decrypt(credential account.Credential) (credentials, error) {
	accessToken, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return credentials{}, err
	}
	refreshToken, err := a.cipher.Decrypt(credential.EncryptedRefreshToken)
	if err != nil {
		return credentials{}, err
	}
	raw, err := a.cipher.Decrypt(credential.EncryptedState)
	if err != nil {
		return credentials{}, err
	}
	state := map[string]string{}
	if raw != "" && json.Unmarshal([]byte(raw), &state) != nil {
		return credentials{}, errors.New("stored ChatGPT state is invalid")
	}
	c := credentialsOf(accessToken, refreshToken, state)
	if !credential.ExpiresAt.IsZero() {
		c.ExpiresAt = credential.ExpiresAt
	}
	return c, nil
}

func (a *Adapter) encryptState(c credentials) (string, error) {
	state := c.state()
	if len(state) == 0 {
		return "", nil
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	return a.cipher.Encrypt(string(raw))
}

// importRecord accepts the reference-project fields, the channel export
// envelope ("credentials") and this adapter's own export.
type importRecord struct {
	Name        string          `json:"name,omitempty"`
	Credentials *importRecord   `json:"credentials,omitempty"`
	AT          string          `json:"access_token,omitempty"`
	AT2         string          `json:"accessToken,omitempty"`
	Token       string          `json:"token,omitempty"`
	RT          string          `json:"refresh_token,omitempty"`
	RT2         string          `json:"refreshToken,omitempty"`
	IDToken     string          `json:"id_token,omitempty"`
	Profile     string          `json:"client_profile,omitempty"`
	Device      string          `json:"oai_device_id,omitempty"`
	Device2     string          `json:"device_id,omitempty"`
	Session     string          `json:"oai_session_id,omitempty"`
	Session2    string          `json:"session_id,omitempty"`
	Browser     json.RawMessage `json:"browser_profile,omitempty"`
}

// ParseImportedCredentials reads one JSON object, a JSON array, {"accounts": [...]},
// JSONL, or one access token per line.
func (a *Adapter) ParseImportedCredentials(data []byte) ([]provider.CredentialSeed, error) {
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
		return nil, provider.ErrCredentialLimit
	}
	if len(values) == 0 {
		return nil, errors.New("empty import")
	}
	out := make([]provider.CredentialSeed, 0, len(values))
	for index, raw := range values {
		seed, err := parseImportRecord(raw)
		if err != nil {
			return nil, fmt.Errorf("record %d: %w", index+1, err)
		}
		out = append(out, seed)
	}
	return out, nil
}

func parseImportRecord(raw json.RawMessage) (provider.CredentialSeed, error) {
	var v importRecord
	if err := json.Unmarshal(raw, &v); err != nil {
		return provider.CredentialSeed{}, errors.New("invalid credential record")
	}
	name := v.Name
	if v.Credentials != nil {
		v = *v.Credentials
	}
	c := credentials{
		AccessToken: firstNonEmptyString([]string{v.AT, v.AT2, v.Token}, ""), RefreshToken: firstNonEmptyString([]string{v.RT, v.RT2}, ""),
		IDToken: v.IDToken, ClientProfile: v.Profile, DeviceID: firstNonEmptyString([]string{v.Device, v.Device2}, ""), SessionID: firstNonEmptyString([]string{v.Session, v.Session2}, ""),
	}
	if len(v.Browser) > 0 && string(v.Browser) != "null" {
		if v.Browser[0] == '"' {
			_ = json.Unmarshal(v.Browser, &c.BrowserProfile)
		} else {
			var p browserprofile.Profile
			if json.Unmarshal(v.Browser, &p) != nil || !p.Valid() {
				return provider.CredentialSeed{}, errors.New("unsupported browser profile")
			}
			encoded, _ := json.Marshal(p)
			c.BrowserProfile = string(encoded)
		}
	}
	if c.AccessToken == "" && c.RefreshToken == "" {
		return provider.CredentialSeed{}, errors.New("access_token or refresh_token is required")
	}
	if len(c.AccessToken) > 32000 || len(c.RefreshToken) > 16000 || len(c.IDToken) > 16000 {
		return provider.CredentialSeed{}, errors.New("credential exceeds size limit")
	}
	if _, err := wire(context.Background(), c); err != nil {
		return provider.CredentialSeed{}, err
	}
	if c.DeviceID == "" {
		c.DeviceID = firstUUID()
	}
	if c.SessionID == "" {
		c.SessionID = firstUUID()
	}
	c.ExpiresAt = tokenExpiry(c.AccessToken)
	return newSeed(name, c, provider.AccountIdentity{}), nil
}

// newSeed prefers the upstream identity and falls back to token claims.
func newSeed(name string, c credentials, identity provider.AccountIdentity) provider.CredentialSeed {
	claims := claimsIdentity(c.AccessToken, c.IDToken)
	identity.UserID = firstNonEmptyString([]string{identity.UserID, claims.UserID}, "")
	identity.TeamID = firstNonEmptyString([]string{identity.TeamID, claims.TeamID}, "")
	identity.Email = firstNonEmptyString([]string{identity.Email, claims.Email}, "")
	identity.Plan = firstNonEmptyString([]string{identity.Plan, claims.Plan}, "")
	key := "chatgpt|" + firstNonEmptyString([]string{c.RefreshToken, c.AccessToken}, "")
	if identity.UserID != "" {
		key = "chatgpt|" + identity.UserID + "|" + identity.TeamID
	}
	return provider.CredentialSeed{
		Provider: account.ProviderChatGPT, AuthType: account.AuthTypeOAuth,
		Name: firstNonEmptyString([]string{name, identity.Email}, defaultAccountName), Email: identity.Email, UserID: identity.UserID, TeamID: identity.TeamID, Plan: identity.Plan,
		SourceKey: "import:" + security.HashToken(key), AccessToken: c.AccessToken, RefreshToken: c.RefreshToken, ExpiresAt: c.ExpiresAt, State: c.state(),
	}
}

func (a *Adapter) MarshalCredentials(values []provider.CredentialSeed) ([]byte, error) {
	out := struct {
		Accounts []importRecord `json:"accounts"`
	}{Accounts: make([]importRecord, 0, len(values))}
	for _, value := range values {
		c := credentialsOf(value.AccessToken, value.RefreshToken, value.State)
		record := importRecord{Name: value.Name, AT: c.AccessToken, RT: c.RefreshToken, IDToken: c.IDToken, Profile: c.ClientProfile, Device: c.DeviceID, Session: c.SessionID}
		if c.BrowserProfile != "" {
			record.Browser, _ = json.Marshal(c.BrowserProfile)
		}
		out.Accounts = append(out.Accounts, record)
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// PrepareImportedCredential exchanges refresh-token-only seeds and resolves
// the upstream identity and plan of every seed.
func (a *Adapter) PrepareImportedCredential(ctx context.Context, seed provider.CredentialSeed) (provider.CredentialSeed, error) {
	c := credentialsOf(seed.AccessToken, seed.RefreshToken, seed.State)
	if !seed.ExpiresAt.IsZero() {
		c.ExpiresAt = seed.ExpiresAt
	}
	pending := account.Credential{Provider: account.ProviderChatGPT}
	refreshed := false
	if c.AccessToken == "" {
		if c.ClientProfile != oauthProfile {
			return seed, errors.New("a refresh_token import requires client_profile " + oauthProfile)
		}
		next, err := a.exchange(ctx, pending, map[string]string{"client_id": oauthClientID, "grant_type": "refresh_token", "refresh_token": c.RefreshToken}, c)
		if err != nil {
			return seed, err
		}
		c, refreshed = next, true
	}
	identity, err := a.identity(ctx, pending, c)
	var upstream *upstreamError
	if err != nil && !refreshed && errors.As(err, &upstream) && upstream.Status == http.StatusUnauthorized {
		return seed, errors.New("access token was rejected by ChatGPT")
	}
	// A refreshed token pair must be kept even when the identity lookup fails:
	// the submitted refresh token may already be revoked.
	name := seed.Name
	if name == defaultAccountName || name == seed.Email {
		name = ""
	}
	return newSeed(name, c, identity), nil
}

func (a *Adapter) RefreshCredential(ctx context.Context, credential account.Credential) (provider.RefreshedCredential, error) {
	c, err := a.decrypt(credential)
	if err != nil {
		return provider.RefreshedCredential{}, &provider.CredentialRefreshError{Code: "credential_decrypt_failed", Message: "Stored credential could not be decrypted", Cause: err}
	}
	if c.RefreshToken == "" {
		return provider.RefreshedCredential{}, &provider.CredentialRefreshError{Code: "missing_refresh_token", Message: "Refresh token is missing", Permanent: true}
	}
	if c.ClientProfile != oauthProfile {
		// Sending a refresh token to another OAuth client can revoke it.
		return provider.RefreshedCredential{}, &provider.CredentialRefreshError{Code: "unsupported_client_profile", Message: "Refresh token was not issued to a supported OAuth client", Permanent: true}
	}
	next, err := a.exchange(ctx, credential, map[string]string{"client_id": oauthClientID, "grant_type": "refresh_token", "refresh_token": c.RefreshToken}, c)
	if err != nil {
		return provider.RefreshedCredential{}, err
	}
	accessToken, err := a.cipher.Encrypt(next.AccessToken)
	if err != nil {
		return provider.RefreshedCredential{}, err
	}
	refreshToken, err := a.cipher.Encrypt(next.RefreshToken)
	if err != nil {
		return provider.RefreshedCredential{}, err
	}
	state, err := a.encryptState(next)
	if err != nil {
		return provider.RefreshedCredential{}, err
	}
	return provider.RefreshedCredential{EncryptedAccessToken: accessToken, EncryptedRefreshToken: refreshToken, EncryptedState: state, ExpiresAt: next.ExpiresAt, RefreshTokenRotated: next.RefreshToken != c.RefreshToken}, nil
}

// exchange calls the OAuth token endpoint through the credential's egress.
// It is never retried: a lost response may already have rotated the token.
func (a *Adapter) exchange(ctx context.Context, credential account.Credential, body map[string]string, old credentials) (credentials, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return old, err
	}
	ctx, lease, err := a.lease(ctx, credential)
	if err != nil {
		return old, &provider.CredentialRefreshError{Code: "oauth_unavailable", Message: "No egress for the OAuth endpoint", Cause: err}
	}
	defer lease.Release()
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, a.tokenURL, bytes.NewReader(raw))
	if err != nil {
		return old, errors.New("invalid OAuth token endpoint")
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "https://platform.openai.com")
	r.Header.Set("Referer", "https://platform.openai.com/")
	client := *a.oauth
	if proxy := environmentFrom(ctx).ProxyURL; proxy != "" {
		egress, err := parseEgressProxy(proxy)
		if err != nil {
			return old, errors.New("invalid OAuth egress")
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		egress.apply(transport)
		defer transport.CloseIdleConnections()
		client.Transport = transport
	}
	resp, err := client.Do(r)
	if err != nil {
		err = &transportError{cause: err}
		a.report(ctx, lease, err)
		return old, &provider.CredentialRefreshError{Code: "oauth_transport_error", Message: "OAuth endpoint unreachable", Cause: err}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var v struct {
		Access      string `json:"access_token"`
		Refresh     string `json:"refresh_token"`
		ID          string `json:"id_token"`
		Expires     int64  `json:"expires_in"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	decodeErr := json.Unmarshal(data, &v)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || decodeErr != nil || v.Access == "" {
		if isCloudflareChallenge(resp.StatusCode, resp.Header, string(data)) {
			a.report(ctx, lease, &upstreamError{Status: resp.StatusCode, Challenge: true})
		}
		message := v.Description
		if len(message) > 512 {
			message = message[:512]
		}
		if message == "" {
			message = "OAuth token endpoint rejected the exchange"
		}
		return old, &provider.CredentialRefreshError{Status: resp.StatusCode, Code: v.Error, Message: message, Permanent: provider.IsPermanentCredentialRefreshErrorCode(v.Error)}
	}
	a.report(ctx, lease, nil)
	next := old
	next.AccessToken = v.Access
	if v.Refresh != "" {
		next.RefreshToken = v.Refresh
	}
	if v.ID != "" {
		next.IDToken = v.ID
	}
	if next.DeviceID == "" {
		next.DeviceID = firstUUID()
	}
	if next.SessionID == "" {
		next.SessionID = firstUUID()
	}
	next.ExpiresAt = tokenExpiry(v.Access)
	if v.Expires > 0 {
		next.ExpiresAt = time.Now().UTC().Add(time.Duration(v.Expires) * time.Second)
	}
	return next, nil
}

func tokenClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var v map[string]any
	_ = json.Unmarshal(b, &v)
	return v
}

func tokenExpiry(token string) time.Time {
	if exp, _ := tokenClaims(token)["exp"].(float64); exp > 0 {
		return time.Unix(int64(exp), 0).UTC()
	}
	return time.Time{}
}

// claimsIdentity reads the OpenAI auth and profile claims of the tokens.
func claimsIdentity(tokens ...string) provider.AccountIdentity {
	var out provider.AccountIdentity
	for _, token := range tokens {
		claims := tokenClaims(token)
		auth, _ := claims["https://api.openai.com/auth"].(map[string]any)
		profile, _ := claims["https://api.openai.com/profile"].(map[string]any)
		out.UserID = firstNonEmptyString([]string{out.UserID, aiString(auth, "chatgpt_user_id", "user_id")}, "")
		out.TeamID = firstNonEmptyString([]string{out.TeamID, aiString(auth, "chatgpt_account_id")}, "")
		out.Plan = firstNonEmptyString([]string{out.Plan, aiString(auth, "chatgpt_plan_type")}, "")
		out.Email = firstNonEmptyString([]string{out.Email, aiString(profile, "email"), aiString(claims, "email")}, "")
	}
	return out
}

func (a *Adapter) getJSON(ctx context.Context, w wireAccount, method, path string, body any) (map[string]any, error) {
	r, err := a.image.do(ctx, method, path, w, body, nil, false)
	if err != nil {
		return nil, err
	}
	var v map[string]any
	return v, decodeOpenAIJSON(r, &v, path)
}

// call runs one read-only request sequence on the credential's egress.
func (a *Adapter) call(ctx context.Context, credential account.Credential, c credentials, fn func(context.Context, wireAccount) error) error {
	_, err := a.attempt(ctx, credential, nil, func(ctx context.Context) error {
		w, err := wire(ctx, c)
		if err != nil {
			return err
		}
		return fn(ctx, w)
	})
	return err
}

func (a *Adapter) identity(ctx context.Context, credential account.Credential, c credentials) (provider.AccountIdentity, error) {
	var out provider.AccountIdentity
	err := a.call(ctx, credential, c, func(ctx context.Context, w wireAccount) error {
		me, err := a.getJSON(ctx, w, http.MethodGet, "/backend-api/me", nil)
		if err != nil {
			return err
		}
		out = provider.AccountIdentity{UserID: aiString(me, "id", "user_id"), Email: aiString(me, "email")}
		if out.UserID == "" {
			return errors.New("upstream did not return a stable user identity")
		}
		accounts, err := a.getJSON(ctx, w, http.MethodGet, "/backend-api/accounts/check/v4-2023-04-27?timezone_offset_min=0", nil)
		if err != nil {
			return err
		}
		catalog, _ := accounts["accounts"].(map[string]any)
		def, _ := catalog["default"].(map[string]any)
		acct, _ := def["account"].(map[string]any)
		out.Plan = aiString(acct, "plan_type")
		out.TeamID = aiString(acct, "account_id", "id")
		return nil
	})
	return out, err
}

func (a *Adapter) SyncAccountIdentity(ctx context.Context, credential account.Credential) (provider.AccountIdentity, error) {
	c, err := a.decrypt(credential)
	if err != nil {
		return provider.AccountIdentity{}, err
	}
	return a.identity(ctx, credential, c)
}

func (a *Adapter) ListModels(ctx context.Context, credential account.Credential) ([]string, error) {
	c, err := a.decrypt(credential)
	if err != nil {
		return nil, err
	}
	var out []string
	err = a.call(ctx, credential, c, func(ctx context.Context, w wireAccount) error {
		models, err := a.getJSON(ctx, w, http.MethodGet, "/backend-api/models", nil)
		if err != nil {
			return err
		}
		out = nil
		capable := map[string]bool{}
		for _, raw := range toAnyList(models["models"]) {
			if m, ok := raw.(map[string]any); ok {
				if slug := aiString(m, "slug"); slug != "" {
					out = append(out, slug)
					capable[slug] = hasImageTool(m)
				}
			}
		}
		a.rememberImageCatalog(credential.ID, capable)
		return nil
	})
	if err == nil && len(out) == 0 {
		err = errors.New("upstream model catalog is empty")
	}
	return out, err
}

// SyncQuota maps conversation/init limits_progress to windows. A feature the
// upstream does not limit gets no window.
func (a *Adapter) SyncQuota(ctx context.Context, credential account.Credential) (provider.QuotaSnapshot, error) {
	c, err := a.decrypt(credential)
	if err != nil {
		return provider.QuotaSnapshot{}, err
	}
	var init map[string]any
	err = a.call(ctx, credential, c, func(ctx context.Context, w wireAccount) error {
		init, err = a.getJSON(ctx, w, http.MethodPost, "/backend-api/conversation/init", map[string]any{"gizmo_id": nil, "requested_default_model": nil, "conversation_id": nil, "timezone_offset_min": 0})
		return err
	})
	if err != nil {
		return provider.QuotaSnapshot{}, err
	}
	now := time.Now().UTC()
	snapshot := provider.QuotaSnapshot{SyncedAt: now}
	for _, raw := range toAnyList(init["limits_progress"]) {
		m, _ := raw.(map[string]any)
		mode := aiString(m, "feature_name", "feature")
		remaining, ok := m["remaining"].(float64)
		if (mode != quotaModeReason && mode != quotaModeImage && mode != quotaModeUpload) || !ok || remaining < 0 {
			continue
		}
		window := account.QuotaWindow{AccountID: credential.ID, Mode: mode, Remaining: int(remaining), SyncedAt: &now, Source: account.QuotaSourceUpstream, UpdatedAt: now}
		if remaining == 0 {
			window.UsagePercent = 100
		}
		if t, err := time.Parse(time.RFC3339, aiString(m, "reset_at", "resets_at")); err == nil {
			window.ResetAt = &t
		}
		snapshot.Windows = append(snapshot.Windows, window)
	}
	return snapshot, nil
}

func (a *Adapter) SyncQuotaMode(ctx context.Context, credential account.Credential, mode string) (account.QuotaWindow, error) {
	snapshot, err := a.SyncQuota(ctx, credential)
	if err != nil {
		return account.QuotaWindow{}, err
	}
	for _, window := range snapshot.Windows {
		if window.Mode == mode {
			return window, nil
		}
	}
	return account.QuotaWindow{}, fmt.Errorf("ChatGPT reports no %s limit", mode)
}

func randomOAuthValue() (string, error) {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// oauthSession is the opaque state of one PKCE login.
type oauthSession struct {
	State       string `json:"state"`
	Verifier    string `json:"verifier"`
	RedirectURI string `json:"redirect_uri"`
	Profile     string `json:"profile"`
}

func (a *Adapter) StartAuthorization(context.Context) (provider.AuthorizationChallenge, error) {
	var values [3]string
	for i := range values {
		value, err := randomOAuthValue()
		if err != nil {
			return provider.AuthorizationChallenge{}, err
		}
		values[i] = value
	}
	state, verifier, nonce := values[0], values[1], values[2]
	hash := sha256.Sum256([]byte(verifier))
	q := url.Values{"issuer": {"https://auth.openai.com"}, "client_id": {oauthClientID}, "audience": {"https://api.openai.com/v1"}, "redirect_uri": {oauthRedirect}, "device_id": {firstUUID()}, "scope": {"openid profile email offline_access"}, "response_type": {"code"}, "response_mode": {"query"}, "state": {state}, "nonce": {nonce}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}, "code_challenge_method": {"S256"}, "screen_hint": {"login_or_signup"}, "max_age": {"0"}}
	session, err := json.Marshal(oauthSession{State: state, Verifier: verifier, RedirectURI: oauthRedirect, Profile: oauthProfile})
	if err != nil {
		return provider.AuthorizationChallenge{}, err
	}
	return provider.AuthorizationChallenge{Kind: provider.AuthorizationRedirect, State: string(session), AuthorizeURL: a.authURL + "?" + q.Encode(), ExpiresIn: 10 * time.Minute}, nil
}

// CompleteAuthorization exchanges the code of the pasted callback URL. The
// code is single use, so the identity lookup is best effort.
func (a *Adapter) CompleteAuthorization(ctx context.Context, challenge provider.AuthorizationChallenge, callback string) (provider.CredentialSeed, error) {
	var start oauthSession
	if json.Unmarshal([]byte(challenge.State), &start) != nil || start.Profile != oauthProfile || start.RedirectURI != oauthRedirect || start.State == "" {
		return provider.CredentialSeed{}, errors.New("OAuth session is invalid")
	}
	u, err := url.Parse(strings.TrimSpace(callback))
	if err != nil || len(callback) > 16384 || u.User != nil || u.Scheme != "https" || u.Host != "platform.openai.com" || u.Path != "/auth/callback" || u.Fragment != "" {
		return provider.CredentialSeed{}, invalidRequest("invalid OAuth callback URL")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(q["state"]) != 1 || q.Get("state") != start.State || q.Get("error") != "" {
		return provider.CredentialSeed{}, fmt.Errorf("%w: OAuth callback state mismatch or login denied", provider.ErrAuthorizationDenied)
	}
	if len(q["code"]) != 1 || q.Get("code") == "" {
		return provider.CredentialSeed{}, invalidRequest("OAuth callback has no authorization code")
	}
	pending := account.Credential{Provider: account.ProviderChatGPT}
	c, err := a.exchange(ctx, pending, map[string]string{"client_id": oauthClientID, "grant_type": "authorization_code", "code": q.Get("code"), "redirect_uri": start.RedirectURI, "code_verifier": start.Verifier}, credentials{ClientProfile: oauthProfile})
	if err != nil {
		return provider.CredentialSeed{}, err
	}
	identity, _ := a.identity(ctx, pending, c)
	return newSeed("", c, identity), nil
}

// hasImageTool reports whether a /backend-api/models entry can generate
// images. Entries without enabled_tools (older catalog shapes) are trusted.
func hasImageTool(model map[string]any) bool {
	tools, ok := model["enabled_tools"].([]any)
	if !ok {
		return true
	}
	for _, tool := range tools {
		if tool == "image_gen_tool_enabled" {
			return true
		}
	}
	return false
}
