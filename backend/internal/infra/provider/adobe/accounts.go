package adobe

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// quotaModeCredits 是账号积分余额的展示窗口；没有路由会消费它。
const quotaModeCredits = "credits"

// validSherlock 校验 x-arp-session-id 的结构是否完整。
func validSherlock(value string) bool {
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		if raw, err = base64.RawStdEncoding.DecodeString(value); err != nil {
			return false
		}
	}
	var fields struct {
		SID string `json:"sid"`
		ARK string `json:"ark"`
		BFP string `json:"bfp"`
		FTR string `json:"ftr"`
	}
	if json.Unmarshal(raw, &fields) != nil {
		return false
	}
	return fields.SID != "" && fields.ARK != "" && fields.BFP != "" && fields.FTR != ""
}

// --- 凭据导入 / 导出（CredentialCodecAdapter）---

type importRecord struct {
	Cookie      string `json:"cookie"`
	AccessToken string `json:"access_token"`
	Email       string `json:"email"`
	Name        string `json:"name"`
}

// ParseImportedCredentials 接受每行一个 Adobe refreshprofile Cookie，或 JSON/
// JSONL 记录。只有能从 Cookie 里解出完整 Sherlock 的记录才被接受。
func (a *Adapter) ParseImportedCredentials(data []byte) ([]provider.CredentialSeed, error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil, nil
	}
	var records []importRecord
	if trimmed[0] == '[' {
		_ = json.Unmarshal(data, &records)
	} else {
		// 逐行解析：以 '{' 开头的行按 JSON 记录处理，其余按裸 Cookie 处理。
		for _, line := range strings.Split(trimmed, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if strings.HasPrefix(line, "{") {
				var record importRecord
				if json.Unmarshal([]byte(line), &record) == nil {
					records = append(records, record)
					continue
				}
			}
			records = append(records, importRecord{Cookie: line})
		}
	}
	seeds := make([]provider.CredentialSeed, 0, len(records))
	for _, record := range records {
		cookie := strings.TrimSpace(record.Cookie)
		if cookie == "" || extractSherlock(cookie) == "" {
			continue
		}
		seeds = append(seeds, provider.CredentialSeed{
			Provider:     account.ProviderAdobe,
			AuthType:     account.AuthTypeOAuth,
			Email:        strings.TrimSpace(record.Email),
			Name:         strings.TrimSpace(record.Name),
			SourceKey:    "adobe:" + security.HashToken(cookie)[:24],
			RefreshToken: cookie,
			AccessToken:  strings.TrimSpace(record.AccessToken),
		})
	}
	return seeds, nil
}

// MarshalCredentials 把凭据按 JSONL 导出，Cookie 放在 refresh token 字段里。
func (a *Adapter) MarshalCredentials(values []provider.CredentialSeed) ([]byte, error) {
	var builder strings.Builder
	for _, seed := range values {
		record := importRecord{Cookie: seed.RefreshToken, Email: seed.Email, Name: seed.Name}
		encoded, err := json.Marshal(record)
		if err != nil {
			return nil, err
		}
		builder.Write(encoded)
		builder.WriteByte('\n')
	}
	return []byte(builder.String()), nil
}

// PrepareImportedCredential 在落库前用 Cookie 换取首个 access token 并查询身份，
// 让账号导入后立即可用。
func (a *Adapter) PrepareImportedCredential(ctx context.Context, seed provider.CredentialSeed) (provider.CredentialSeed, error) {
	cookie := strings.TrimSpace(seed.RefreshToken)
	if cookie == "" || extractSherlock(cookie) == "" {
		return seed, &provider.CredentialRefreshError{Status: http.StatusBadRequest, Code: "adobe_cookie_invalid", Message: "缺少有效的 Adobe Cookie"}
	}
	pending := a.pendingCredential(ctx, seed.SourceKey)
	var token string
	var expiresAt time.Time
	var email, userID string
	err := a.withClient(ctx, pending, func(ctx context.Context, c *client) error {
		var refreshErr error
		token, expiresAt, refreshErr = c.imsRefresh(ctx, cookie)
		if refreshErr != nil {
			return refreshErr
		}
		c.rc.token = token
		email, userID = c.imsProfile(ctx)
		return nil
	})
	if err != nil {
		return seed, err
	}
	seed.AccessToken = token
	seed.ExpiresAt = expiresAt
	if userID != "" {
		seed.UserID = userID
	} else {
		seed.UserID = firstString(decodeJWTClaims(token), "user_id", "aa_id", "sub")
	}
	if email != "" {
		seed.Email = email
	}
	return seed, nil
}

// --- 凭据续期（CredentialRefreshAdapter）---

// RefreshCredential 用存储的 Cookie 换取新的 access token。Cookie 本身不轮换，
// 原样保留在 refresh token 字段里。
func (a *Adapter) RefreshCredential(ctx context.Context, credential account.Credential) (provider.RefreshedCredential, error) {
	if credential.EncryptedRefreshToken == "" {
		return provider.RefreshedCredential{}, &provider.CredentialRefreshError{Code: "missing_refresh_token", Permanent: true}
	}
	cookie, err := a.cipher.Decrypt(credential.EncryptedRefreshToken)
	if err != nil {
		return provider.RefreshedCredential{}, err
	}
	var token string
	var expiresAt time.Time
	err = a.withClient(ctx, credential, func(ctx context.Context, c *client) error {
		token, expiresAt, err = c.imsRefresh(ctx, cookie)
		return err
	})
	if err != nil {
		return provider.RefreshedCredential{}, err
	}
	encrypted, err := a.cipher.Encrypt(token)
	if err != nil {
		return provider.RefreshedCredential{}, err
	}
	return provider.RefreshedCredential{
		EncryptedAccessToken:  encrypted,
		EncryptedRefreshToken: credential.EncryptedRefreshToken,
		ExpiresAt:             expiresAt,
	}, nil
}

// --- 凭据展示（CredentialMetadataAdapter）---

func (a *Adapter) CredentialMetadata(credential account.Credential) provider.CredentialMetadata {
	return provider.CredentialMetadata{CredentialKind: "firefly-cookie"}
}

// --- 账号身份（AccountIdentityAdapter）---

func (a *Adapter) SyncAccountIdentity(ctx context.Context, credential account.Credential) (provider.AccountIdentity, error) {
	var identity provider.AccountIdentity
	err := a.withClient(ctx, credential, func(ctx context.Context, c *client) error {
		if c.rc.token == "" {
			return &transportError{err: errNoToken}
		}
		email, userID := c.imsProfile(ctx)
		identity.Email = email
		identity.UserID = userID
		if identity.UserID == "" {
			identity.UserID = firstString(decodeJWTClaims(c.rc.token), "user_id", "aa_id", "sub")
		}
		return nil
	})
	if err != nil {
		return provider.AccountIdentity{}, err
	}
	return identity, nil
}

// --- 额度（QuotaAdapter）：积分余额仅用于展示 ---

func (a *Adapter) SyncQuota(ctx context.Context, credential account.Credential) (provider.QuotaSnapshot, error) {
	window, ok := a.fetchCreditsWindow(ctx, credential)
	snapshot := provider.QuotaSnapshot{SyncedAt: time.Now().UTC()}
	if ok {
		snapshot.Windows = []account.QuotaWindow{window}
	}
	return snapshot, nil
}

func (a *Adapter) SyncQuotaMode(ctx context.Context, credential account.Credential, mode string) (account.QuotaWindow, error) {
	if window, ok := a.fetchCreditsWindow(ctx, credential); ok {
		return window, nil
	}
	return account.QuotaWindow{Mode: quotaModeCredits}, nil
}

// fetchCreditsWindow 查询积分余额并映射成展示窗口；失败时返回 false（不影响调度）。
func (a *Adapter) fetchCreditsWindow(ctx context.Context, credential account.Credential) (account.QuotaWindow, bool) {
	accountID := credential.UserID
	var window account.QuotaWindow
	var ok bool
	_ = a.withClient(ctx, credential, func(ctx context.Context, c *client) error {
		if c.rc.token == "" {
			return nil
		}
		if accountID == "" {
			accountID = firstString(decodeJWTClaims(c.rc.token), "user_id", "aa_id", "sub")
		}
		total, available, err := c.creditBalance(ctx, accountID)
		if err != nil {
			return err
		}
		// 解析不到任何积分数值时不发窗口，避免把账号误判为耗尽（路由按"无窗口=可用"处理）。
		if total <= 0 && available <= 0 {
			return nil
		}
		now := time.Now().UTC()
		window = account.QuotaWindow{Mode: quotaModeCredits, Total: total, Remaining: available, Source: account.QuotaSourceUpstream, SyncedAt: &now}
		if total > 0 {
			window.UsagePercent = float64(total-available) / float64(total) * 100
		}
		ok = true
		return nil
	})
	return window, ok
}

// --- 模型目录（ModelCatalogAdapter）：静态目录，每个账号都具备全部媒体模型 ---

func (a *Adapter) ListModels(ctx context.Context, credential account.Credential) ([]string, error) {
	return staticUpstreamModels(), nil
}

func (a *Adapter) NormalizeAccountModelCapabilities(models []string, _ *account.Billing, _ account.Credential) []string {
	return staticUpstreamModels()
}

func staticUpstreamModels() []string {
	models := make([]string, 0, len(imageModels)+len(videoModels))
	for _, m := range imageModels {
		models = append(models, m.id)
	}
	for _, m := range videoModels {
		models = append(models, m.id)
	}
	return models
}

// --- IMS / 积分 上游调用 ---

// imsRefresh 用 Cookie 调 IMS check/v6/token 换取 access token。401/403 视为
// Cookie 永久失效，返回永久错误让账号转入需重新导入状态。
func (c *client) imsRefresh(ctx context.Context, cookie string) (string, time.Time, error) {
	body := url.Values{"client_id": {c.rc.apiKey}, "guest_allowed": {"true"}, "scope": {refreshScope}}
	headers := c.browserHeaders()
	headers["Accept"] = "*/*"
	headers["Accept-Language"] = "en-US,en;q=0.9"
	headers["Content-Type"] = "application/x-www-form-urlencoded;charset=UTF-8"
	headers["Cookie"] = cookie
	headers["Origin"] = "https://firefly.adobe.com"
	headers["Referer"] = "https://firefly.adobe.com/"
	status, data, _, err := c.request(ctx, http.MethodPost, refreshURL, strings.NewReader(body.Encode()), headers)
	if err != nil {
		return "", time.Time{}, err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return "", time.Time{}, &provider.CredentialRefreshError{Status: status, Code: "refresh_token_invalidated", Message: "Adobe Cookie 已失效", Permanent: true}
	}
	if status < 200 || status >= 300 {
		return "", time.Time{}, &provider.CredentialRefreshError{Status: status, Code: "adobe_refresh_failed", Message: "Adobe 刷新失败"}
	}
	token := firstString(data, "access_token", "accessToken")
	if token == "" {
		return "", time.Time{}, &provider.CredentialRefreshError{Status: status, Code: "adobe_refresh_empty", Message: "Adobe 刷新未返回 access token"}
	}
	expiresIn := 3600.0
	if v, ok := data["expires_in"].(float64); ok && v > 0 {
		expiresIn = v
	} else if v, ok := data["expiresIn"].(float64); ok && v > 0 {
		expiresIn = v
	}
	if expiresIn < 60 {
		expiresIn = 60
	}
	return token, time.Now().Add(time.Duration(expiresIn) * time.Second), nil
}

// imsProfile 查询账号展示身份；失败只影响展示，返回空串。
func (c *client) imsProfile(ctx context.Context) (email, userID string) {
	for _, path := range profileURLs {
		status, data, _, err := c.request(ctx, http.MethodGet, path, nil, map[string]string{
			"Authorization": "Bearer " + c.rc.token, "Accept": "application/json",
		})
		if err != nil || status < 200 || status >= 300 {
			continue
		}
		email = firstString(data, "email")
		userID = firstString(data, "userId", "authId")
		if email != "" || userID != "" {
			return email, userID
		}
	}
	return "", ""
}

// creditBalance 查询账号积分（data.total.quota.{total,available}）。
func (c *client) creditBalance(ctx context.Context, accountID string) (total, available int, err error) {
	status, data, _, reqErr := c.request(ctx, http.MethodGet, creditsURL, nil, map[string]string{
		"Authorization": "Bearer " + c.rc.token, "x-api-key": c.rc.apiKey,
		"x-account-id": accountID, "Accept": "application/json", "Content-Type": "application/json",
		"Origin": "https://firefly.adobe.com", "Referer": "https://firefly.adobe.com/",
	})
	if reqErr != nil {
		return 0, 0, reqErr
	}
	if status < 200 || status >= 300 {
		return 0, 0, classifyUpstream("credits", status, bodyText(data))
	}
	totalObj, _ := data["total"].(map[string]any)
	quota, _ := totalObj["quota"].(map[string]any)
	return intField(quota, "total"), intField(quota, "available"), nil
}

func intField(data map[string]any, key string) int {
	if data == nil {
		return 0
	}
	if v, ok := data[key].(float64); ok {
		return int(v)
	}
	return 0
}
