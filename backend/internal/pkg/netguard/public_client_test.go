package netguard

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func localPublicClient(t *testing.T, server *httptest.Server) (*http.Client, *atomic.Int32) {
	t.Helper()
	client := NewPublicClient()
	transport := client.Transport.(*publicTransport).transport
	if transport.Proxy != nil || transport.ResponseHeaderTimeout == 0 {
		t.Fatal("public downloads must bypass environment proxies and bound header waits")
	}
	transport.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	endpoint, _ := url.Parse(server.URL)
	transport.TLSClientConfig.ServerName = endpoint.Hostname()
	var dials atomic.Int32
	// Route the test's public hostname to a local TLS server. DNS validation
	// and pinning are exercised independently below, without external traffic.
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	t.Cleanup(client.CloseIdleConnections)
	return client, &dials
}

func TestPublicClientDirectAnonymousRedirectsAndReusesConnections(t *testing.T) {
	var proxyCalls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer proxy.Close()
	for _, key := range []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY"} {
		t.Setenv(key, proxy.URL)
	}
	var mu sync.Mutex
	var requests []*http.Request
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Clone(r.Context()))
		mu.Unlock()
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "https://cdn.example.test/final?signature=kept", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = io.WriteString(w, "video")
	}))
	defer server.Close()
	client, dials := localPublicClient(t, server)
	req, _ := http.NewRequest(http.MethodGet, "https://cdn.example.test/start", nil)
	for _, key := range []string{"authorization", "Proxy-Authorization", "cookie", "DPoP", "X-API-Key", "X-XAI-Token-Auth", "Referer"} {
		req.Header[key] = []string{"private"}
	}
	req.Header.Set("Range", "bytes=0-4")
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || string(raw) != "video" {
		t.Fatalf("download = %q, %v", raw, err)
	}
	response, err = client.Get("https://cdn.example.test/final?signature=kept")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 3 || dials.Load() != 1 || proxyCalls.Load() != 0 {
		t.Fatalf("requests=%d dials=%d proxy calls=%d", len(requests), dials.Load(), proxyCalls.Load())
	}
	for _, r := range requests {
		for _, key := range []string{"Authorization", "Proxy-Authorization", "Cookie", "DPoP", "X-API-Key", "X-XAI-Token-Auth", "Referer"} {
			if r.Header.Get(key) != "" {
				t.Fatalf("download leaked %s", key)
			}
		}
	}
	if requests[1].URL.RawQuery != "signature=kept" || requests[1].Header.Get("Range") != "bytes=0-4" || req.Header["authorization"][0] != "private" {
		t.Fatal("signed query, Range or the caller's headers were changed")
	}
}

func TestPublicClientStreamsAndCancelsBody(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "prefix")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	client, _ := localPublicClient(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://cdn.example.test/video", nil)
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	prefix := make([]byte, 6)
	if _, err := io.ReadFull(response.Body, prefix); err != nil || string(prefix) != "prefix" {
		t.Fatalf("stream prefix = %q, %v", prefix, err)
	}
	cancel()
	if _, err := io.ReadAll(response.Body); !errors.Is(err, context.Canceled) {
		t.Fatalf("body did not honor cancellation: %v", err)
	}
}

func TestPublicClientRejectsUnsafeRequestsAndRedirects(t *testing.T) {
	for _, target := range []string{"http://cdn.example.test/out", "https://user:pass@cdn.example.test/out", "https://127.0.0.1/out", "https://[::1]/out", "https://169.254.169.254/latest"} {
		t.Run(target, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				http.Redirect(w, r, target, http.StatusFound)
			}))
			defer server.Close()
			client, _ := localPublicClient(t, server)
			if _, err := client.Get(target); err == nil || calls.Load() != 0 {
				t.Fatalf("unsafe initial request reached transport: %v", err)
			}
			if _, err := client.Get("https://cdn.example.test/start"); err == nil || calls.Load() != 1 {
				t.Fatalf("unsafe redirect reached transport: calls=%d err=%v", calls.Load(), err)
			}
		})
	}
	client := NewPublicClient()
	req, _ := http.NewRequest(http.MethodPut, "https://cdn.example.test/out", strings.NewReader("input"))
	if _, err := client.Do(req); err == nil {
		t.Fatal("upload used the public download client")
	}
}

type publicResolverFunc func(context.Context, string, string) ([]netip.Addr, error)

func (f publicResolverFunc) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return f(ctx, network, host)
}

func TestPublicDialerPinsEveryConnectionAndRejectsMixedDNS(t *testing.T) {
	lookups, dials := 0, 0
	addresses := []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("1.1.1.1")}
	dialer := publicDialer{
		resolve: publicResolverFunc(func(ctx context.Context, network, host string) ([]netip.Addr, error) {
			lookups++
			if network != "ip" || host != "cdn.example.test" {
				t.Fatalf("lookup %s %s", network, host)
			}
			return addresses, nil
		}),
		dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			dials++
			if address == "8.8.8.8:443" {
				return nil, errors.New("first address unavailable")
			}
			if address != "1.1.1.1:443" {
				t.Fatalf("dial was not pinned to a validated IP: %s", address)
			}
			one, two := net.Pipe()
			_ = two.Close()
			return one, nil
		},
	}
	connection, err := dialer.DialContext(context.Background(), "tcp", "cdn.example.test:443")
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.Close()
	if lookups != 1 || dials != 2 {
		t.Fatalf("lookups=%d dials=%d", lookups, dials)
	}
	// A rebinding/mixed answer on the next connection is rejected in full.
	addresses = append(addresses, netip.MustParseAddr("10.0.0.1"))
	if _, err := dialer.DialContext(context.Background(), "tcp", "cdn.example.test:443"); err == nil || lookups != 2 || dials != 2 {
		t.Fatalf("mixed public/private DNS was dialed: lookups=%d dials=%d err=%v", lookups, dials, err)
	}
	addresses = nil
	if _, err := dialer.DialContext(context.Background(), "tcp", "cdn.example.test:443"); err == nil {
		t.Fatal("empty DNS answer accepted")
	}
}
