package chatgpt

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/tunnelproxy"
)

// egressProxy is how a request reaches chatgpt.com through the selected egress.
// HTTP(S) proxies use CONNECT; SOCKS and tunnel protocols replace the dialer.
type egressProxy struct {
	raw  string
	url  *url.URL
	dial func(context.Context, string, string) (net.Conn, error)
}

func parseEgressProxy(raw string) (egressProxy, error) {
	raw = strings.TrimSpace(raw)
	connect, dial, err := tunnelproxy.Route(raw, &net.Dialer{Timeout: 20 * time.Second})
	if err != nil {
		return egressProxy{}, fmt.Errorf("invalid egress proxy: %w", err)
	}
	return egressProxy{raw: raw, url: connect, dial: dial}, nil
}

// apply routes an http.Transport through the proxy. Without a proxy the
// caller's direct dialer is kept.
func (p egressProxy) apply(transport *http.Transport) {
	switch {
	case p.url != nil:
		// The proxy host is trusted deployment configuration and may live on a
		// private network; only upstream target hosts are restricted.
		transport.Proxy = http.ProxyURL(p.url)
		transport.DialContext = (&net.Dialer{Timeout: 20 * time.Second}).DialContext
	case p.dial != nil:
		transport.Proxy = nil
		transport.DialContext = p.dial
	}
}

// transportError marks a failure to reach the upstream through the egress.
// The cause stays available to errors.Is/As but never reaches messages, since
// proxy errors can echo proxy addresses.
type transportError struct{ cause error }

func (e *transportError) Error() string { return "upstream transport failed" }
func (e *transportError) Unwrap() error { return e.cause }

// isCloudflareChallenge recognizes a Cloudflare interstitial rather than an
// application-level 403 from chatgpt.com, which can be account-specific.
func isCloudflareChallenge(status int, header http.Header, body string) bool {
	if status != http.StatusForbidden && status != http.StatusServiceUnavailable && status != http.StatusTooManyRequests {
		return false
	}
	if strings.EqualFold(header.Get("cf-mitigated"), "challenge") {
		return true
	}
	lower := strings.ToLower(body)
	return strings.Contains(lower, "just a moment") || strings.Contains(lower, "challenge-platform") ||
		strings.Contains(lower, "__cf_chl") || strings.Contains(lower, "cf-chl-")
}
