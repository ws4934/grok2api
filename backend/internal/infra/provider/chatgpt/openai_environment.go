// Protocol portions adapted from SkyKoo/gptGrok2api at 15773099641575776025e21d1290b5209b058c5a. See LICENSE.reference.
package chatgpt

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/chatgpt/browserprofile"
)

func accountBrowserProfile(account map[string]any) (browserprofile.Profile, error) {
	if value := account["browser_profile"]; value != nil {
		if p, ok := browserprofile.Parse(value); ok {
			return p, nil
		}
		return browserprofile.Profile{}, fmt.Errorf("saved browser profile is unsupported")
	}
	if p, ok := browserprofile.FromFingerprint(account["fp"]); ok {
		return p, nil
	}
	if fp, ok := account["fp"].(map[string]any); ok && aiString(fp, "profile") != "" {
		return browserprofile.Profile{}, fmt.Errorf("saved browser fingerprint is unsupported")
	}
	// Imports without a registered profile use one coherent, deterministic
	// fallback; do not combine arbitrary legacy UA strings with a different TLS.
	return browserprofile.Default(), nil
}

func stableBrowserID(kind, token string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + token))
	sum[6] = (sum[6] & 0x0f) | 0x40
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

type openAIFingerprint struct {
	Profile             browserprofile.Profile
	DeviceID, SessionID string
}

func buildOpenAIFingerprint(account map[string]any, tokens ...string) openAIFingerprint {
	p, _ := accountBrowserProfile(account)
	token := aiString(account, "access_token", "accessToken", "token")
	if len(tokens) > 0 {
		token = tokens[0]
	}
	fp := openAIFingerprint{Profile: p, DeviceID: stableBrowserID("device", token), SessionID: stableBrowserID("session", token)}
	if v := aiString(account, "oai-device-id", "oai_device_id"); v != "" {
		fp.DeviceID = v
	}
	if v := aiString(account, "oai-session-id", "oai_session_id"); v != "" {
		fp.SessionID = v
	}
	if values, ok := account["fp"].(map[string]any); ok {
		if v := aiString(values, "oai-device-id", "oai_device_id"); v != "" {
			fp.DeviceID = v
		}
		if v := aiString(values, "oai-session-id", "oai_session_id"); v != "" {
			fp.SessionID = v
		}
	}
	return fp
}

func applyOpenAIBrowserContext(account map[string]any, payload map[string]any) {
	p, _ := accountBrowserProfile(account)
	info, ok := payload["client_contextual_info"].(map[string]any)
	if !ok {
		info = map[string]any{}
		payload["client_contextual_info"] = info
	}
	for key, value := range map[string]any{"page_height": p.InnerHeight, "page_width": p.InnerWidth, "pixel_ratio": p.DevicePixelRatio, "screen_height": p.ScreenHeight, "screen_width": p.ScreenWidth} {
		info[key] = value
	}
}

// Call with the owning client's browserMu held. Credential/profile changes
// create a fresh jar; separate accounts and egresses never share cookie state.
func accountBrowser(cache map[string]*browserHTTP, origin, proxy, token string, fields map[string]any, timeout time.Duration) (*browserHTTP, error) {
	p, err := accountBrowserProfile(fields)
	if err != nil {
		return nil, err
	}
	key := accountBrowserKey(origin, proxy, token, p, fields)
	if b := cache[key]; b != nil {
		b.lastUsed = time.Now()
		return b, nil
	}
	b, err := newBrowserHTTP(origin, proxy, timeout, p, savedWebCookies(fields, token))
	if err != nil {
		return nil, err
	}
	if len(cache) >= 256 {
		oldest := ""
		for k, v := range cache {
			if oldest == "" || v.lastUsed.Before(cache[oldest].lastUsed) {
				oldest = k
			}
		}
		cache[oldest].CloseIdleConnections()
		delete(cache, oldest)
	}
	b.lastUsed = time.Now()
	cache[key] = b
	return b, nil
}

func browserLanguage(p browserprofile.Profile) string {
	if p.NavigatorLanguage != "" {
		return p.NavigatorLanguage
	}
	return strings.Split(p.AcceptLanguage, ",")[0]
}

func accountBrowserKey(origin, proxy, token string, p browserprofile.Profile, fields map[string]any) string {
	state, _ := json.Marshal([]any{origin, proxy, token, p, savedWebCookies(fields, token), aiString(fields, "cookie_header"), aiString(fields, "cookie"), aiString(fields, "cookies"), aiString(fields, "user_id", "account_id", "email")})
	sum := sha256.Sum256(state)
	return fmt.Sprintf("%x", sum)
}
func webSessionSecrets(fields map[string]any, token string) []string {
	out := []string{token}
	out = append(out, browserprofile.CookieSecrets(fields["web_cookies"])...)
	return out
}

func savedWebCookies(fields map[string]any, token string) []browserprofile.Cookie {
	if token == "" || aiString(fields, "web_cookies_token_hash") != browserprofile.TokenBinding(token) {
		return nil
	}
	return browserprofile.ParseCookies(fields["web_cookies"])
}
