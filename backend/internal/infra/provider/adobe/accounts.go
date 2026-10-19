package adobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// quotaModeCredits 是账号积分余额窗口；所有 Adobe 路由都消费它。
const quotaModeCredits = "credits"

// --- 凭据导入 / 导出（CredentialCodecAdapter）---

type importRecord struct {
	Cookie      string `json:"cookie"`
	AccessToken string `json:"access_token"`
	Email       string `json:"email"`
	Name        string `json:"name"`
}

// ParseImportedCredentials 接受每行一个 Adobe Cookie，或 JSON/JSONL 记录。
// Cookie 是否可用由 PrepareImportedCredential 的 IMS 续期来验证。
func (a *Adapter) ParseImportedCredentials(data []byte) ([]provider.CredentialSeed, error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil, nil
	}
	var records []importRecord
	if trimmed[0] == '[' {
		if err := json.Unmarshal([]byte(trimmed), &records); err != nil {
			return nil, errors.New("invalid JSON array")
		}
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
		if !looksLikeCookie(cookie) {
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
	if len(seeds) == 0 {
		return nil, errors.New("没有找到有效的 Adobe Cookie（形如 name=value; name2=value2）")
	}
	return seeds, nil
}

// looksLikeCookie 粗筛一行是否是 Cookie 头（至少一个 name=value）。
func looksLikeCookie(value string) bool {
	name, _, ok := strings.Cut(value, "=")
	return ok && strings.TrimSpace(name) != "" && !strings.ContainsAny(value, "\r\n")
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
	if !looksLikeCookie(cookie) {
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
			return errNoToken
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

// --- 额度（QuotaAdapter）：积分余额作为 credits 窗口 ---

// SyncQuota 查询积分余额。查询失败返回错误（不写任何窗口）；拿不到数值时不发
// 窗口，账号按未限制处理。
func (a *Adapter) SyncQuota(ctx context.Context, credential account.Credential) (provider.QuotaSnapshot, error) {
	window, ok, err := a.fetchCreditsWindow(ctx, credential)
	if err != nil {
		return provider.QuotaSnapshot{}, err
	}
	snapshot := provider.QuotaSnapshot{SyncedAt: time.Now().UTC()}
	if ok {
		snapshot.Windows = []account.QuotaWindow{window}
	}
	return snapshot, nil
}

// SyncQuotaMode 刷新 credits 窗口。绝不返回凭空的零值窗口：落库时它会被记成
// 上游来源的"余额 0"，账号就被误判为积分耗尽。
func (a *Adapter) SyncQuotaMode(ctx context.Context, credential account.Credential, mode string) (account.QuotaWindow, error) {
	window, ok, err := a.fetchCreditsWindow(ctx, credential)
	if err != nil {
		return account.QuotaWindow{}, err
	}
	if !ok {
		return account.QuotaWindow{}, fmt.Errorf("%w: %s", account.ErrQuotaNotLimited, quotaModeCredits)
	}
	return window, nil
}

// fetchCreditsWindow 查询积分余额并映射成 credits 窗口；解析不到任何数值时 ok 为 false。
func (a *Adapter) fetchCreditsWindow(ctx context.Context, credential account.Credential) (account.QuotaWindow, bool, error) {
	accountID := credential.UserID
	var window account.QuotaWindow
	var ok bool
	err := a.withClient(ctx, credential, func(ctx context.Context, c *client) error {
		if c.rc.token == "" {
			return errNoToken
		}
		if accountID == "" {
			accountID = firstString(decodeJWTClaims(c.rc.token), "user_id", "aa_id", "sub")
		}
		total, available, err := c.creditBalance(ctx, accountID)
		if err != nil || (total <= 0 && available <= 0) {
			return err
		}
		now := time.Now().UTC()
		window = account.QuotaWindow{Mode: quotaModeCredits, Total: total, Remaining: available, Source: account.QuotaSourceUpstream, SyncedAt: &now}
		if total > 0 {
			window.UsagePercent = float64(total-available) / float64(total) * 100
		}
		ok = true
		return nil
	})
	return window, ok, err
}

// --- 模型目录（ModelCatalogAdapter）---

// ListModels 不按账号发现模型：目录是全局手写表（models.go），每个账号都服务全部模型
// （StaticModel）。返回空可避免账号同步把模型登记成对话路由。
func (a *Adapter) ListModels(ctx context.Context, credential account.Credential) ([]string, error) {
	return nil, nil
}

// --- IMS / 积分 上游调用 ---

// imsRefresh 用 Cookie 调 IMS check/v6/token 换取 access token。401/403 视为
// Cookie 永久失效，返回永久错误让账号转入需重新导入状态。
func (c *client) imsRefresh(ctx context.Context, cookie string) (string, time.Time, error) {
	body := url.Values{"client_id": {c.rc.apiKey}, "guest_allowed": {"true"}, "scope": {refreshScope}}
	headers := map[string]string{}
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
