package egress

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/buildtransport"
	"github.com/chenyme/grok2api/backend/internal/pkg/socksproxy"
	"github.com/chenyme/grok2api/backend/internal/pkg/tunnelproxy"
)

// newBuildClient keeps Grok Build on the standard Go HTTP/TLS stack used by
// the official CLI-facing transport. Browser TLS impersonation is reserved for
// Grok Web, where the browser fingerprint and User-Agent belong together.
func newBuildClient(proxyURL string, responseHeaderTimeout time.Duration) (*http.Client, error) {
	return newBuildClientWithOptions(proxyURL, responseHeaderTimeout, false)
}

// newBuildEnvironmentClient preserves the process-wide Build direct
// transport's HTTP_PROXY/HTTPS_PROXY behavior while giving the caller an
// independent connection pool.
func newBuildEnvironmentClient(responseHeaderTimeout time.Duration) (*http.Client, error) {
	return newBuildClientWithOptions("", responseHeaderTimeout, true)
}

func newBuildClientWithOptions(proxyURL string, responseHeaderTimeout time.Duration, environmentProxy bool) (*http.Client, error) {
	direct := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           direct.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   128,
		MaxConnsPerHost:       256,
		IdleConnTimeout:       buildtransport.IdleConnTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: responseHeaderTimeout,
		ExpectContinueTimeout: time.Second,
	}
	if environmentProxy {
		transport.Proxy = http.ProxyFromEnvironment
	}
	if strings.TrimSpace(proxyURL) != "" {
		parsed, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("解析 Grok Build 出口代理: %w", err)
		}
		switch strings.ToLower(parsed.Scheme) {
		case "http", "https":
			transport.Proxy = http.ProxyURL(parsed)
		case "socks4", "socks4a", "socks5", "socks5h":
			dial, err := socksproxy.NewDialContext(parsed, direct, direct.Timeout)
			if err != nil {
				return nil, fmt.Errorf("创建 Grok Build SOCKS 代理: %w", err)
			}
			transport.DialContext = dial
		case "trojan", "vless", "ss", "vmess":
			dialer, err := tunnelproxy.NewDialer(proxyURL)
			if err != nil {
				return nil, fmt.Errorf("创建 Grok Build 隧道代理: %w", err)
			}
			transport.DialContext = dialer.DialContext
		default:
			return nil, fmt.Errorf("Grok Build 不支持代理协议 %q", parsed.Scheme)
		}
	}
	if _, err := buildtransport.ConfigureHTTP2Health(transport); err != nil {
		return nil, fmt.Errorf("配置 Grok Build HTTP/2 健康探测: %w", err)
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}
