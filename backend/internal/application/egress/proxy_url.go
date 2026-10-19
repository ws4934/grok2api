package egress

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/tunnelproxy"
)

const (
	maxProxyURLBytes         = 8192
	maxCloudflareCookieBytes = 16 << 10
	ProxyAccountPlaceholder  = "{account}"
	proxyAccountSentinel     = "grok2api_account_placeholder"
)

func (s *Service) proxyMetadata(encrypted string) (string, string, bool) {
	if s == nil || s.cipher == nil || strings.TrimSpace(encrypted) == "" {
		return "", "", false
	}
	proxyURL, err := s.cipher.Decrypt(encrypted)
	if err != nil {
		return "", "", false
	}
	proxyURL, err = NormalizeProxyURL(proxyURL)
	if err != nil || proxyURL == "" {
		return "", "", false
	}
	return ProxyDisplay(proxyURL), security.HashToken(proxyURL)[:12], strings.Contains(proxyURL, ProxyAccountPlaceholder)
}

// ProxyDisplay preserves the routable endpoint and, for standard proxies, the
// username, while ensuring passwords and tunnel credentials never enter list
// responses. The short fingerprint lets operators identify duplicate physical
// proxies without revealing the secret.
func ProxyDisplay(proxyURL string) string {
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return ""
	}
	if tunnelproxy.IsSupportedScheme(parsed.Scheme) {
		config, parseErr := tunnelproxy.Parse(proxyURL)
		if parseErr != nil {
			return ""
		}
		return strings.ToLower(config.Scheme) + "://***@" + config.Server
	}
	if parsed.Host == "" {
		return ""
	}
	if parsed.User != nil {
		username := parsed.User.Username()
		if _, hasPassword := parsed.User.Password(); hasPassword {
			parsed.User = url.UserPassword(username, "***")
		} else {
			parsed.User = url.User(username)
		}
	}
	parsed.Path, parsed.RawPath, parsed.RawQuery, parsed.Fragment = "", "", "", ""
	return parsed.String()
}

func (s *Service) accountBoundProxy(value domain.Node) bool {
	if s == nil || s.cipher == nil || strings.TrimSpace(value.EncryptedProxyURL) == "" {
		return false
	}
	proxyURL, err := s.cipher.Decrypt(value.EncryptedProxyURL)
	return err == nil && strings.Contains(proxyURL, ProxyAccountPlaceholder)
}

func NormalizeProxyURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if len(value) > maxProxyURLBytes || strings.IndexFunc(value, func(character rune) bool { return character < 0x20 || character == 0x7f }) >= 0 {
		return "", errors.New("代理地址过长或包含控制字符")
	}
	hasAccountPlaceholder := strings.Contains(value, ProxyAccountPlaceholder)
	if strings.Count(value, ProxyAccountPlaceholder) > 1 {
		return "", errors.New("代理地址最多包含一个 {account} 占位符")
	}
	if hasAccountPlaceholder && strings.Contains(value, proxyAccountSentinel) {
		return "", errors.New("代理地址包含保留的账号占位符文本")
	}
	parseValue := strings.ReplaceAll(value, ProxyAccountPlaceholder, proxyAccountSentinel)
	if normalized, ok := normalizeLegacyHTTPProxy(parseValue); ok {
		parseValue = normalized
	}
	parsed, err := url.Parse(parseValue)
	if err != nil {
		return "", errors.New("代理地址格式无效")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if tunnelproxy.IsSupportedScheme(scheme) {
		if hasAccountPlaceholder {
			return "", errors.New("隧道代理不支持 {account} 占位符")
		}
		normalized, normalizeErr := tunnelproxy.Normalize(value)
		if normalizeErr != nil {
			return "", normalizeErr
		}
		return normalized, nil
	}
	if parsed.Host == "" || parsed.Hostname() == "" {
		return "", errors.New("代理地址格式无效")
	}
	switch scheme {
	case "http", "https", "socks4", "socks4a", "socks5", "socks5h":
	default:
		return "", errors.New("代理地址协议必须是 HTTP、HTTPS、SOCKS4、SOCKS5、Trojan、VLESS、SS 或 VMess")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New("代理地址不能包含路径、查询参数或片段")
	}
	if hasAccountPlaceholder {
		if parsed.User == nil || !strings.Contains(parsed.User.Username(), proxyAccountSentinel) {
			return "", errors.New("{account} 只能用于代理认证用户名")
		}
		return strings.ReplaceAll(parsed.String(), proxyAccountSentinel, ProxyAccountPlaceholder), nil
	}
	return parsed.String(), nil
}

func normalizeLegacyHTTPProxy(value string) (string, bool) {
	parts := strings.SplitN(value, ":", 4)
	if len(parts) != 4 || net.ParseIP(parts[0]).To4() == nil || parts[2] == "" || parts[3] == "" {
		return "", false
	}
	port, err := strconv.ParseUint(parts[1], 10, 16)
	if err != nil || port == 0 {
		return "", false
	}
	return (&url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(parts[0], strconv.FormatUint(port, 10)),
		User:   url.UserPassword(parts[2], parts[3]),
	}).String(), true
}

func SanitizeCloudflareCookies(value string) string {
	allowed := make([]string, 0, 4)
	seen := make(map[string]struct{})
	for part := range strings.SplitSeq(value, ";") {
		name, cookieValue, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		lower := strings.ToLower(name)
		if lower != "cf_clearance" && lower != "__cf_bm" && lower != "_cfuvid" && !strings.HasPrefix(lower, "cf_chl_") {
			continue
		}
		if _, exists := seen[lower]; exists {
			continue
		}
		cookieValue = strings.TrimSpace(cookieValue)
		if cookieValue == "" || len(cookieValue) > maxCloudflareCookieBytes || strings.IndexFunc(cookieValue, func(character rune) bool { return character < 0x20 || character == 0x7f }) >= 0 {
			continue
		}
		seen[lower] = struct{}{}
		allowed = append(allowed, lower+"="+cookieValue)
	}
	return strings.Join(allowed, "; ")
}
