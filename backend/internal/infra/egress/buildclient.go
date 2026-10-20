package egress

import (
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/buildtransport"
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
	if err := tunnelproxy.RouteTransport(transport, proxyURL, direct); err != nil {
		return nil, fmt.Errorf("Grok Build 出口代理: %w", err)
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
