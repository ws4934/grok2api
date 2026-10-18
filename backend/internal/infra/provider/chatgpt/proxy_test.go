package chatgpt

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
