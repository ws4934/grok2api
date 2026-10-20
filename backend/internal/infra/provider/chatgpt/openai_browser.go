// Protocol portions adapted from SkyKoo/gptGrok2api at 15773099641575776025e21d1290b5209b058c5a. See LICENSE.reference.
package chatgpt

import (
	"io"
	"net"
	stdhttp "net/http"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/chatgpt/browserprofile"
)

// browserHTTP adapts tls-client's fhttp request/response types to the
// standard library types used by the provider package.
type browserHTTP struct {
	client   tlsclient.HttpClient
	lastUsed time.Time
	profile  browserprofile.Profile

	// The page bootstrap (Sentinel scripts and build) of this browser session;
	// its cookies stay in the client's jar.
	bootMu      sync.Mutex
	bootAt      time.Time
	bootScripts []string
	bootBuild   string
}

// bootstrapTTL is how long a browser session reuses its page bootstrap, as a
// browser tab keeps the page it loaded.
const bootstrapTTL = 15 * time.Minute

func (c *browserHTTP) bootstrapped() ([]string, string, bool) {
	c.bootMu.Lock()
	defer c.bootMu.Unlock()
	if c.bootAt.IsZero() || time.Since(c.bootAt) > bootstrapTTL {
		return nil, "", false
	}
	return c.bootScripts, c.bootBuild, true
}

func (c *browserHTTP) rememberBootstrap(scripts []string, build string) {
	c.bootMu.Lock()
	defer c.bootMu.Unlock()
	c.bootAt, c.bootScripts, c.bootBuild = time.Now(), scripts, build
}

func newBrowserHTTP(origin, proxyURL string, timeout time.Duration, profile browserprofile.Profile, savedCookies any) (*browserHTTP, error) {
	seconds := int(timeout / time.Second)
	if seconds < 1 {
		seconds = 30
	}
	jar := browserprofile.NewCookieJar(origin)
	jar.Restore(savedCookies)
	options := []tlsclient.HttpClientOption{
		tlsclient.WithClientProfile(profile.TLSClientProfile()),
		tlsclient.WithTimeoutSeconds(seconds),
		tlsclient.WithNotFollowRedirects(),
		tlsclient.WithCookieJar(jar),
	}
	egress, err := parseEgressProxy(proxyURL)
	if err != nil {
		return nil, err
	}
	switch {
	case egress.dial != nil:
		options = append(options, tlsclient.WithDialContext(egress.dial))
	case egress.url != nil && egress.url.Scheme == "http":
		// tls-client's own CONNECT dialer ignores the request context and dials
		// under a client-wide lock: one silent proxy would stall every request
		// on this browser. HTTPS proxies keep tls-client's dialer.
		options = append(options, tlsclient.WithDialContext(infraegress.ConnectDialContext(egress.url, &net.Dialer{Timeout: 20 * time.Second})))
	case egress.url != nil:
		options = append(options, tlsclient.WithProxyUrl(egress.raw))
	}
	client, err := tlsclient.NewHttpClient(tlsclient.NewNoopLogger(), options...)
	if err != nil {
		return nil, err
	}
	return &browserHTTP{client: client, profile: profile}, nil
}

func (c *browserHTTP) Do(req *stdhttp.Request) (*stdhttp.Response, error) {
	converted, err := fhttp.NewRequestWithContext(req.Context(), req.Method, req.URL.String(), req.Body)
	if err != nil {
		return nil, err
	}
	converted.Header = cloneFHTTPHeader(req.Header)
	converted.ContentLength = req.ContentLength
	converted.Close = req.Close
	converted.Host = req.Host

	response, err := c.client.Do(converted)
	if err != nil {
		return nil, err
	}
	return cloneStdHTTPResponse(response), nil
}

func (c *browserHTTP) CloseIdleConnections() {
	if c != nil && c.client != nil {
		c.client.CloseIdleConnections()
	}
}

func cloneFHTTPHeader(source stdhttp.Header) fhttp.Header {
	target := make(fhttp.Header, len(source)+1)
	for key, values := range source {
		target[key] = append([]string(nil), values...)
	}
	browserprofile.SetHeaderOrder(target)
	return target
}

func cloneStdHTTPResponse(source *fhttp.Response) *stdhttp.Response {
	target := &stdhttp.Response{
		Status:           source.Status,
		StatusCode:       source.StatusCode,
		Proto:            source.Proto,
		ProtoMajor:       source.ProtoMajor,
		ProtoMinor:       source.ProtoMinor,
		Header:           cloneStdHeader(source.Header),
		Body:             source.Body,
		ContentLength:    source.ContentLength,
		TransferEncoding: append([]string(nil), source.TransferEncoding...),
		Close:            source.Close,
		Uncompressed:     source.Uncompressed,
		Trailer:          cloneStdHeader(source.Trailer),
	}
	if target.Body == nil {
		target.Body = io.NopCloser(nilReader{})
	}
	return target
}

func cloneStdHeader(source fhttp.Header) stdhttp.Header {
	target := make(stdhttp.Header, len(source))
	for key, values := range source {
		if key == fhttp.HeaderOrderKey || key == fhttp.PHeaderOrderKey {
			continue
		}
		target[key] = append([]string(nil), values...)
	}
	return target
}

type nilReader struct{}

func (nilReader) Read([]byte) (int, error) { return 0, io.EOF }
