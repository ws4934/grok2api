// Protocol portions adapted from SkyKoo/gptGrok2api at 15773099641575776025e21d1290b5209b058c5a. See LICENSE.reference.
package chatgpt

import (
	"io"
	stdhttp "net/http"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/chatgpt/browserprofile"
)

// browserHTTP adapts tls-client's fhttp request/response types to the
// standard library types used by the provider package.
type browserHTTP struct {
	client   tlsclient.HttpClient
	lastUsed time.Time
	profile  browserprofile.Profile
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
	if proxyURL != "" {
		options = append(options, tlsclient.WithProxyUrl(proxyURL))
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
