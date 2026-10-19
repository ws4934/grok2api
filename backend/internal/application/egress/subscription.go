package egress

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chenyme/grok2api/backend/internal/pkg/socksproxy"
	"github.com/chenyme/grok2api/backend/internal/pkg/tunnelproxy"
)

const (
	maxSubscriptionBytes     = 2 << 20
	maxSubscriptionEntries   = 10000
	maxSubscriptionHops      = 3
	subscriptionFetchTimeout = 20 * time.Second
	proxyHandshakeTimeout    = 10 * time.Second
)

var blockedSubscriptionPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001:db8::/32"),
}

type subscriptionEntry struct {
	ProxyURL string
	Key      string
}

func normalizeSubscriptionURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxProxyURLBytes {
		return "", errors.New("订阅地址为空或过长")
	}
	if strings.IndexFunc(value, func(character rune) bool { return character < 0x20 || character == 0x7f }) >= 0 {
		return "", errors.New("订阅地址包含控制字符")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" {
		return "", errors.New("订阅地址格式无效")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("订阅地址必须使用 HTTP 或 HTTPS")
	}
	if parsed.Fragment != "" {
		return "", errors.New("订阅地址不能包含片段")
	}
	return parsed.String(), nil
}

func fetchProxySubscription(ctx context.Context, value string, viaProxy string) ([]byte, error) {
	normalized, err := normalizeSubscriptionURL(value)
	if err != nil {
		return nil, err
	}
	transport, err := subscriptionTransport(viaProxy)
	if err != nil {
		return nil, err
	}
	defer transport.CloseIdleConnections()
	proxied := strings.TrimSpace(viaProxy) != ""
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= maxSubscriptionHops {
				return errors.New("订阅重定向次数过多")
			}
			redirectURL, err := normalizeSubscriptionURL(request.URL.String())
			if err != nil {
				return errors.New("订阅重定向地址无效")
			}
			if proxied {
				if err := validatePublicSubscriptionTarget(request.Context(), redirectURL); err != nil {
					return errors.New("订阅重定向地址不能指向内网")
				}
			}
			return nil
		},
	}
	requestCtx, cancel := context.WithTimeout(ctx, subscriptionFetchTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, normalized, nil)
	if err != nil {
		return nil, err
	}
	if proxied {
		if err := validatePublicSubscriptionTarget(requestCtx, normalized); err != nil {
			return nil, err
		}
	}
	request.Header.Set("Accept", "text/plain, text/*;q=0.9, */*;q=0.1")
	request.Header.Set("User-Agent", "Clash.Meta")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("订阅服务返回 HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxSubscriptionBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxSubscriptionBytes {
		return nil, errors.New("订阅内容超过大小限制")
	}
	return body, nil
}

// subscriptionTransport builds the HTTP transport used to pull remote proxy
// lists. Without a via-proxy the dialer rejects private destinations (SSRF).
// With a via-proxy the admin-configured proxy endpoint may be private, while
// fetchProxySubscription separately requires every subscription target and
// redirect to resolve exclusively to public addresses.
func subscriptionTransport(viaProxy string) (*http.Transport, error) {
	direct := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           publicDialContext(net.DefaultResolver),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          2,
		MaxIdleConnsPerHost:   1,
		IdleConnTimeout:       15 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	viaProxy = strings.TrimSpace(viaProxy)
	if viaProxy == "" {
		return transport, nil
	}
	parsed, err := url.Parse(viaProxy)
	if err != nil || parsed.Host == "" {
		return nil, errors.New("订阅拉取代理地址无效")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		transport.Proxy = http.ProxyURL(parsed)
		// Dial the proxy endpoint itself; private admin proxies are allowed.
		transport.DialContext = direct.DialContext
	case "socks4", "socks4a", "socks5", "socks5h":
		dial, err := socksproxy.NewDialContext(parsed, direct, proxyHandshakeTimeout)
		if err != nil {
			return nil, fmt.Errorf("创建订阅拉取 SOCKS 代理: %w", err)
		}
		transport.DialContext = dial
	case "trojan", "vless", "ss", "vmess":
		dialer, err := tunnelproxy.NewDialer(viaProxy)
		if err != nil {
			return nil, fmt.Errorf("创建订阅拉取隧道代理: %w", err)
		}
		transport.DialContext = dialer.DialContext
	default:
		return nil, errors.New("订阅拉取代理协议不受支持")
	}
	return transport, nil
}

// validatePublicSubscriptionTarget closes the SSRF gap introduced by remote
// proxy resolution. Requiring every locally resolved address to be public also
// rejects split-horizon names that expose an internal address on this host.
func validatePublicSubscriptionTarget(ctx context.Context, value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" {
		return errors.New("订阅地址格式无效")
	}
	host := strings.Trim(strings.TrimSpace(parsed.Hostname()), "[]")
	if address, parseErr := netip.ParseAddr(host); parseErr == nil {
		if !isPublicAddress(address) {
			return errors.New("订阅地址不能指向内网")
		}
		return nil
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("解析订阅地址: %w", err)
	}
	if len(addresses) == 0 {
		return errors.New("订阅地址没有可用的公网 IP")
	}
	for _, address := range addresses {
		if !isPublicAddress(address) {
			return errors.New("订阅地址不能指向内网")
		}
	}
	return nil
}

// publicDialContext resolves every destination immediately before dialing. It
// rejects loopback, private, link-local, multicast, and carrier-grade NAT
// addresses, including redirect destinations, to avoid subscription SSRF.
func publicDialContext(resolver *net.Resolver) func(context.Context, string, string) (net.Conn, error) {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addresses, err := resolvePublicAddresses(ctx, resolver, host)
		if err != nil {
			return nil, err
		}
		dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}
		var lastErr error
		for _, ip := range addresses {
			connection, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if dialErr == nil {
				return connection, nil
			}
			lastErr = dialErr
		}
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, errors.New("订阅地址没有可用的公网 IP")
	}
}

func resolvePublicAddresses(ctx context.Context, resolver *net.Resolver, host string) ([]netip.Addr, error) {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if parsed, err := netip.ParseAddr(host); err == nil {
		if !isPublicAddress(parsed) {
			return nil, errors.New("订阅地址不能指向内网")
		}
		return []netip.Addr{parsed.Unmap()}, nil
	}
	addresses, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	public := make([]netip.Addr, 0, len(addresses))
	for _, address := range addresses {
		if isPublicAddress(address) {
			public = append(public, address.Unmap())
		}
	}
	if len(public) == 0 {
		return nil, errors.New("订阅地址不能指向内网")
	}
	return public, nil
}

func isPublicAddress(value netip.Addr) bool {
	value = value.Unmap()
	if !value.IsValid() || !value.IsGlobalUnicast() || value.IsLoopback() || value.IsPrivate() || value.IsLinkLocalUnicast() || value.IsLinkLocalMulticast() || value.IsMulticast() || value.IsUnspecified() {
		return false
	}
	for _, prefix := range blockedSubscriptionPrefixes {
		if prefix.Contains(value) {
			return false
		}
	}
	return true
}

func parseProxySubscription(value string) ([]subscriptionEntry, int, error) {
	entries, skipped := parseProxyLines(value)
	if len(entries) > 0 {
		return entries, skipped, nil
	}
	compact := strings.Map(func(character rune) rune {
		if character == ' ' || character == '\t' || character == '\r' || character == '\n' {
			return -1
		}
		return character
	}, strings.TrimPrefix(value, "\ufeff"))
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		decoded, err := encoding.DecodeString(compact)
		if err != nil || len(decoded) == 0 || len(decoded) > maxSubscriptionBytes {
			continue
		}
		entries, decodedSkipped := parseProxyLines(string(decoded))
		if len(entries) > 0 {
			// The original Base64 text is not an invalid proxy entry. Once it
			// decodes to a valid list, report only invalid decoded entries.
			return entries, decodedSkipped, nil
		}
	}
	if entries, clashSkipped, matched := parseClashSubscription(value); matched && len(entries) > 0 {
		return entries, clashSkipped, nil
	}
	return nil, skipped, errors.New("订阅中没有可用的代理节点")
}

func parseProxyLines(value string) ([]subscriptionEntry, int) {
	value = strings.TrimPrefix(value, "\ufeff")
	seen := make(map[string]struct{})
	entries := make([]subscriptionEntry, 0)
	skipped := 0
	for line := range strings.SplitSeq(value, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		normalized, err := NormalizeProxyURL(line)
		if err != nil {
			skipped++
			continue
		}
		digest := sha256.Sum256([]byte(normalized))
		key := hex.EncodeToString(digest[:])
		if _, exists := seen[key]; exists {
			skipped++
			continue
		}
		seen[key] = struct{}{}
		entries = append(entries, subscriptionEntry{ProxyURL: normalized, Key: key})
		if len(entries) > maxSubscriptionEntries {
			return nil, skipped
		}
	}
	return entries, skipped
}

func sourceNodeName(sourceName string, index int) string {
	suffix := fmt.Sprintf(" %03d", index+1)
	value := strings.TrimSpace(sourceName)
	for len(value)+len(suffix) > 160 && value != "" {
		_, size := utf8.DecodeLastRuneInString(value)
		value = value[:len(value)-size]
	}
	return strings.TrimSpace(value) + suffix
}
