package netguard

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// NewPublicClient downloads anonymous HTTPS resources directly, independently
// of API egress and HTTP_PROXY/HTTPS_PROXY. The caller owns the body and deadline.
// Connections are pooled; each new connection resolves, validates and pins its
// destination, including redirects. It never falls back to an API proxy.
func NewPublicClient() *http.Client {
	dialer := &net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}
	public := publicDialer{resolve: net.DefaultResolver, dial: dialer.DialContext}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = public.DialContext
	transport.ResponseHeaderTimeout = 30 * time.Second
	return &http.Client{
		Transport: &publicTransport{transport: transport},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 3 {
				return errors.New("资源下载重定向次数过多")
			}
			return validatePublicRequest(req)
		},
	}
}

type publicTransport struct{ transport *http.Transport }

func (t *publicTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := validatePublicRequest(req); err != nil {
		return nil, err
	}
	request := req.Clone(req.Context())
	for key := range request.Header {
		switch strings.ToLower(key) {
		case "authorization", "proxy-authorization", "cookie", "dpop", "x-api-key", "x-xai-token-auth", "referer":
			delete(request.Header, key)
		}
	}
	return t.transport.RoundTrip(request)
}

func (t *publicTransport) CloseIdleConnections() { t.transport.CloseIdleConnections() }

func validatePublicRequest(req *http.Request) error {
	if req.URL == nil || req.URL.Scheme != "https" || req.URL.Hostname() == "" || req.URL.User != nil {
		return errors.New("资源下载地址必须是无用户信息的 HTTPS 地址")
	}
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return errors.New("公开资源客户端仅支持下载")
	}
	if address, err := netip.ParseAddr(req.URL.Hostname()); err == nil && !IsPublicAddress(address) {
		return errors.New("资源下载地址不能指向非公网地址")
	}
	return nil
}

type publicResolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

type publicDialer struct {
	resolve publicResolver
	dial    func(context.Context, string, string) (net.Conn, error)
}

func (d publicDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addresses, err := d.resolve.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(addresses) == 0 {
		return nil, errors.New("资源下载地址没有可用的公网 IP")
	}
	for _, ip := range addresses {
		if !IsPublicAddress(ip) {
			return nil, errors.New("资源下载地址解析到非公网 IP")
		}
	}
	for _, ip := range addresses {
		var connection net.Conn
		connection, err = d.dial(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return connection, nil
		}
	}
	return nil, err
}
