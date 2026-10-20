package tunnelproxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/pkg/socksproxy"
)

// Route resolves how a connection reaches its target through proxyURL: an
// HTTP(S) proxy is returned as connect, to be used with CONNECT, and a SOCKS
// or tunnel proxy as dial. direct reaches the proxy itself and bounds a SOCKS
// handshake. An empty proxyURL returns neither.
func Route(proxyURL string, direct *net.Dialer) (connect *url.URL, dial func(context.Context, string, string) (net.Conn, error), err error) {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return nil, nil, nil
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil || parsed.Host == "" {
		return nil, nil, errors.New("代理地址无效")
	}
	switch scheme := strings.ToLower(parsed.Scheme); {
	case scheme == "http" || scheme == "https":
		return parsed, nil, nil
	case scheme == "socks4" || scheme == "socks4a" || scheme == "socks5" || scheme == "socks5h":
		dial, err := socksproxy.NewDialContext(parsed, direct, direct.Timeout)
		if err != nil {
			return nil, nil, fmt.Errorf("创建 SOCKS 代理: %w", err)
		}
		return nil, dial, nil
	case IsSupportedScheme(scheme):
		dialer, err := NewDialer(proxyURL)
		if err != nil {
			return nil, nil, fmt.Errorf("创建隧道代理: %w", err)
		}
		return nil, dialer.DialContext, nil
	default:
		return nil, nil, fmt.Errorf("不支持代理协议 %q", parsed.Scheme)
	}
}

// RouteTransport sends transport through proxyURL as Route resolves it and
// leaves transport unchanged when proxyURL is empty.
func RouteTransport(transport *http.Transport, proxyURL string, direct *net.Dialer) error {
	connect, dial, err := Route(proxyURL, direct)
	switch {
	case err != nil:
		return err
	case connect != nil:
		transport.Proxy = http.ProxyURL(connect)
		transport.DialContext = direct.DialContext
	case dial != nil:
		transport.Proxy = nil
		transport.DialContext = dial
	}
	return nil
}
