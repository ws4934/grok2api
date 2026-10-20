package chatgpt

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/chatgpt/browserprofile"
)

func TestSignedTransfersReachPrivateProxies(t *testing.T) {
	connects := make(chan string, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connects <- r.Method + " " + r.Host
		w.WriteHeader(http.StatusForbidden)
	}))
	defer proxy.Close()
	client, err := publicTransferClient(proxy.URL) // a loopback proxy, like a local sidecar
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Get("https://example.com/upload")
	if err == nil || strings.Contains(err.Error(), "non-public transfer address") {
		t.Fatalf("private proxy hop was rejected: %v", err)
	}
	if got := <-connects; got != "CONNECT example.com:443" {
		t.Fatalf("proxy saw %q", got)
	}
}

// A proxy that accepts TCP and never answers must fail a browser request at
// its deadline and must not block a request to another host.
func TestBrowserHTTPProxyHandshakeEndsWithRequest(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var held []net.Conn
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range held {
			_ = conn.Close()
		}
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, conn)
			mu.Unlock()
		}
	}()
	for _, scheme := range []string{"socks5", "http"} {
		t.Run(scheme, func(t *testing.T) {
			client, err := newBrowserHTTP("https://chatgpt.com", scheme+"://"+listener.Addr().String(), time.Minute, browserprofile.Default(), nil)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 2)
			for _, target := range []string{"https://203.0.113.1/", "https://203.0.113.2/"} {
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
					defer cancel()
					request, _ := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
					response, err := client.Do(request)
					if err == nil {
						_ = response.Body.Close()
					}
					done <- err
				}()
			}
			for range 2 {
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("request through a silent proxy succeeded")
					}
				case <-time.After(time.Second):
					t.Fatal("request still blocked in the proxy handshake after its deadline")
				}
			}
		})
	}
}

func TestEgressProxySchemes(t *testing.T) {
	for raw, want := range map[string]string{
		"":                           "direct",
		"http://u:p@10.0.0.2:8080":   "connect",
		"socks5h://u:p@proxy:1080":   "dialer",
		"socks4://proxy:1080":        "dialer",
		"ftp://proxy:21":             "error",
		"not a url with no host ://": "error",
	} {
		p, err := parseEgressProxy(raw)
		got := "direct"
		switch {
		case err != nil:
			got = "error"
		case p.url != nil:
			got = "connect"
		case p.dial != nil:
			got = "dialer"
		}
		if got != want {
			t.Errorf("%q: got %s, want %s", raw, got, want)
		}
	}
}

func TestFailureResponseSeparatesEgressFromAccountFailures(t *testing.T) {
	challenge := func(header http.Header, body string) error {
		r := &http.Response{StatusCode: 403, Header: header, Body: io.NopCloser(strings.NewReader(body))}
		return readOpenAIHTTPError(r, "/backend-api/conversation")
	}
	for name, tc := range map[string]struct {
		err  error
		code string
	}{
		"cf header":   {challenge(http.Header{"Cf-Mitigated": {"challenge"}}, ""), "cloudflare_challenge"},
		"cf page":     {challenge(http.Header{}, "<title>Just a moment...</title>"), "cloudflare_challenge"},
		"account 403": {challenge(http.Header{}, `{"detail":"Unusual activity"}`), "upstream_forbidden"},
	} {
		response, err := failureResponse("responses", tc.err)
		if err != nil || response.StatusCode != 403 {
			t.Fatalf("%s: %+v, %v", name, response, err)
		}
		var body struct {
			Error struct{ Code string } `json:"error"`
		}
		if json.NewDecoder(response.Body).Decode(&body) != nil || body.Error.Code != tc.code {
			t.Errorf("%s: code %q", name, body.Error.Code)
		}
	}
	transport := &transportError{cause: errors.New("proxyconnect tcp: refused")}
	if response, err := failureResponse("responses", transport); response != nil || err != transport {
		t.Fatalf("transport failure became a response: %+v", response)
	}
	if strings.Contains((&transportError{cause: errors.New("dial http://user:secret@proxy")}).Error(), "secret") {
		t.Fatal("transport error leaked the proxy address")
	}
}

func TestTransferHostTrustAndPublicCheck(t *testing.T) {
	for _, host := range []string{"chatgpt.com", "files.oaiusercontent.com", "sdmntprwestus.oaiusercontent.com.", "cdn.oaistatic.com"} {
		if !trustedTransferHost(host) {
			t.Fatalf("%s should be trusted", host)
		}
	}
	for _, host := range []string{"evilchatgpt.com", "oaiusercontent.com.evil.test", "localhost"} {
		if trustedTransferHost(host) {
			t.Fatalf("%s must not be trusted", host)
		}
	}
	if err := validatePublicHost(context.Background(), "localhost"); err == nil || !strings.Contains(err.Error(), "non-public transfer address") {
		t.Fatalf("localhost transfer = %v", err)
	}
}

func TestUpstreamErrorTextCarriesEndpointAndBody(t *testing.T) {
	err := &upstreamError{Status: 422, Endpoint: "/backend-api/files/x/download", Message: "{\n \"detail\": \"bad file\"\n}"}
	if got := err.Error(); got != `ChatGPT upstream HTTP 422 at /backend-api/files/x/download: { "detail": "bad file" }` {
		t.Fatalf("error = %q", got)
	}
}
