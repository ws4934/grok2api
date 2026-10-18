package chatgpt

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	_ "github.com/bdandy/go-socks4" // registers socks4/socks4a with golang.org/x/net/proxy
	"github.com/chenyme/grok2api/backend/internal/pkg/tunnelproxy"
	xproxy "golang.org/x/net/proxy"
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
	if raw == "" {
		return egressProxy{}, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return egressProxy{}, errors.New("invalid egress proxy")
	}
	switch scheme := strings.ToLower(u.Scheme); {
	case scheme == "http" || scheme == "https":
		return egressProxy{raw: raw, url: u}, nil
	case scheme == "socks4" || scheme == "socks4a" || scheme == "socks5" || scheme == "socks5h":
		dialer, err := xproxy.FromURL(u, &net.Dialer{Timeout: 20 * time.Second})
		if err != nil {
			return egressProxy{}, errors.New("invalid SOCKS egress proxy")
		}
		return egressProxy{raw: raw, dial: contextDial(dialer)}, nil
	case tunnelproxy.IsSupportedScheme(scheme):
		dialer, err := tunnelproxy.NewDialer(raw)
		if err != nil {
			return egressProxy{}, errors.New("invalid tunnel egress proxy")
		}
		return egressProxy{raw: raw, dial: dialer.DialContext}, nil
	default:
		return egressProxy{}, fmt.Errorf("unsupported egress proxy scheme %q", u.Scheme)
	}
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

func contextDial(dialer xproxy.Dialer) func(context.Context, string, string) (net.Conn, error) {
	if contextual, ok := dialer.(xproxy.ContextDialer); ok {
		return contextual.DialContext
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		type result struct {
			conn net.Conn
			err  error
		}
		done := make(chan result, 1)
		go func() {
			conn, err := dialer.Dial(network, address)
			done <- result{conn, err}
		}()
		select {
		case r := <-done:
			return r.conn, r.err
		case <-ctx.Done():
			go func() {
				if r := <-done; r.conn != nil {
					_ = r.conn.Close()
				}
			}()
			return nil, ctx.Err()
		}
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
