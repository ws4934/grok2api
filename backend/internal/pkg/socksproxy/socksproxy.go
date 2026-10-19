// Package socksproxy dials through SOCKS4, SOCKS4a, SOCKS5 and SOCKS5h proxies.
package socksproxy

import (
	"context"
	"net"
	"net/url"
	"time"

	_ "github.com/bdandy/go-socks4" // registers socks4/socks4a with golang.org/x/net/proxy
	xproxy "golang.org/x/net/proxy"
)

// NewDialContext returns a dial function through the SOCKS proxy at proxyURL.
// go-socks4 has no context-aware dial and no handshake deadline, so every dial
// runs its handshake on a proxy connection that is closed when ctx ends and
// expires handshakeTimeout after connecting. The deadline is cleared once the
// proxy has connected to address.
func NewDialContext(proxyURL *url.URL, forward *net.Dialer, handshakeTimeout time.Duration) (func(context.Context, string, string) (net.Conn, error), error) {
	if _, err := xproxy.FromURL(proxyURL, forward); err != nil {
		return nil, err
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		bound := &boundDialer{ctx: ctx, forward: forward, timeout: handshakeTimeout}
		dialer, err := xproxy.FromURL(proxyURL, bound)
		if err != nil {
			return nil, err
		}
		var conn net.Conn
		if contextual, ok := dialer.(xproxy.ContextDialer); ok {
			conn, err = contextual.DialContext(ctx, network, address)
		} else {
			conn, err = dialer.Dial(network, address)
		}
		if !bound.release() && err == nil {
			err = ctx.Err() // ctx ended during the handshake and closed the connection
		}
		if err == nil {
			err = conn.SetDeadline(time.Time{})
		}
		if err != nil {
			if conn != nil {
				_ = conn.Close()
			}
			return nil, err
		}
		return conn, nil
	}, nil
}

// boundDialer opens the proxy connection for a single dial.
type boundDialer struct {
	ctx     context.Context
	forward *net.Dialer
	timeout time.Duration
	stop    func() bool
}

func (d *boundDialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(d.ctx, network, address)
}

func (d *boundDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	conn, err := d.forward.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	if err := conn.SetDeadline(time.Now().Add(d.timeout)); err != nil {
		_ = conn.Close()
		return nil, err
	}
	d.stop = context.AfterFunc(ctx, func() { _ = conn.Close() })
	return conn, nil
}

// release stops the close-on-cancel hook and reports whether it had not run.
func (d *boundDialer) release() bool {
	return d.stop == nil || d.stop()
}
