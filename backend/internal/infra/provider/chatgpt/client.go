package chatgpt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/pkg/netguard"
	"github.com/google/uuid"
)

// Maps below are private wire representations. They never leave this package.
type wireAccount struct {
	Token  string
	Fields map[string]any
}
type wireMessage struct {
	Role    string
	Content any
}
type wireChatRequest struct {
	Model           string
	Messages        []wireMessage
	ReasoningEffort string
}
type upstreamError struct {
	// URL is scheme, host and path of the failed request, without the query.
	URL      string
	Endpoint string
	Status   int
	Message  string
	Body     string
	// Challenge marks a Cloudflare interstitial: the request never reached chatgpt.com.
	Challenge bool
	// RetryAfter is the upstream Retry-After header, when sent.
	RetryAfter string
}

// UpstreamDiagnostic exposes the failed upstream response to request audits.
func (e *upstreamError) UpstreamDiagnostic() (string, provider.DiagnosticResponse) {
	return e.URL, provider.DiagnosticResponse{StatusCode: e.Status, Status: fmt.Sprintf("%d %s", e.Status, http.StatusText(e.Status)), Body: []byte(e.Body)}
}

// Error is for logs and admin views only (no PublicErrorMessage): it carries
// the endpoint and a redacted body excerpt so failures can be diagnosed.
func (e *upstreamError) Error() string {
	text := fmt.Sprintf("ChatGPT upstream HTTP %d", e.Status)
	if e.Endpoint != "" {
		text += " at " + e.Endpoint
	}
	if excerpt := strings.Join(strings.Fields(e.Message), " "); excerpt != "" {
		if len(excerpt) > 300 {
			excerpt = excerpt[:300] + "…"
		}
		text += ": " + excerpt
	}
	return text
}

const openAIClientVersion = "prod-a194cd50d4416d3c0b47c740f206b12ce60f5887"
const openAIClientBuild = "6708908"

func firstUUID() string        { return uuid.NewString() }
func stringValue(v any) string { s, _ := v.(string); return s }
func boolValue(v any) bool     { b, _ := v.(bool); return b }
func aiString(v map[string]any, keys ...string) string {
	for _, k := range keys {
		if s := stringValue(v[k]); s != "" {
			return s
		}
	}
	return ""
}

func (o *OpenAIImage) doAbsolute(ctx context.Context, method, endpoint string, a wireAccount, body io.Reader, extra map[string]string, stream bool) (*http.Response, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.User != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("invalid external transfer URL")
	}
	// Signed upload/download endpoints always use an unauthenticated client.
	if err := validatePublicHost(ctx, u.Hostname()); err != nil {
		return nil, err
	}
	return o.doRequest(ctx, method, endpoint, u.Path, a, body, extra, stream, false)
}
func (o *OpenAIImage) doRequest(ctx context.Context, method, endpoint, path string, a wireAccount, body io.Reader, extra map[string]string, stream, authenticated bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, errors.New("invalid upstream URL")
	}
	if authenticated {
		if _, err := accountBrowserProfile(a.Fields); err != nil {
			return nil, err
		}
		setOpenAIHeaders(req, path, a.Token, a.Fields)
	}
	for k, v := range extra {
		if !authenticated && (strings.EqualFold(k, "Authorization") || strings.EqualFold(k, "Cookie")) {
			continue
		}
		req.Header.Set(k, v)
	}
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	var resp *http.Response
	proxy := environmentFrom(ctx).ProxyURL
	if authenticated && strings.HasPrefix(o.BaseURL, "https://chatgpt.com") {
		o.browserMu.Lock()
		b, e := accountBrowser(o.browsers, o.BaseURL, proxy, a.Token, a.Fields, o.RequestTimeout)
		o.browserMu.Unlock()
		if e != nil {
			return nil, e
		}
		resp, err = b.Do(req)
	} else if authenticated {
		resp, err = o.Client.Do(req)
	} else {
		client, e := publicTransferClient(proxy)
		if e != nil {
			return nil, e
		}
		resp, err = client.Do(req)
	}
	if err != nil {
		return nil, &transportError{cause: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		e := readOpenAIHTTPError(resp, path, a.Token)
		resp.Body.Close()
		return nil, e
	}
	return resp, nil
}

// Resolve and pin public addresses at dial time, including redirect targets.
// Through a proxy only the target host is checked (before the request and on
// redirects); the proxy itself may be on a private network.
func publicTransferClient(proxy string) (*http.Client, error) {
	egress, err := parseEgressProxy(proxy)
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DisableKeepAlives = true
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			if !netguard.IsPublicAddress(ip) && !trustedTransferHost(host) {
				return nil, errors.New("non-public transfer address")
			}
		}
		var last error
		for _, ip := range ips {
			c, e := (&net.Dialer{Timeout: 20 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if e == nil {
				return c, nil
			}
			last = e
		}
		if last == nil {
			last = errors.New("no transfer address")
		}
		return nil, last
	}
	egress.apply(transport)
	return &http.Client{Transport: transport, Timeout: 60 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 3 || req.URL.Scheme != "https" || req.URL.User != nil {
			return errors.New("invalid transfer redirect")
		}
		req.Header.Del("Authorization")
		req.Header.Del("Cookie")
		return validatePublicHost(req.Context(), req.URL.Hostname())
	}}, nil
}

// trustedTransferHost reports OpenAI-owned hosts. Their DNS answers are
// trusted even when a local TUN proxy's fake-ip resolver (Clash, Surge:
// 198.18.0.0/15) returns reserved addresses; other hosts that signed URLs or
// redirects point to must resolve to public addresses.
func trustedTransferHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, domain := range []string{"chatgpt.com", "openai.com", "oaiusercontent.com", "oaistatic.com"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

func validatePublicHost(ctx context.Context, host string) error {
	if trustedTransferHost(host) {
		return nil
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return err
	}
	if len(ips) == 0 {
		return errors.New("no transfer address")
	}
	for _, ip := range ips {
		if !netguard.IsPublicAddress(ip) {
			return errors.New("non-public transfer address")
		}
	}
	return nil
}
func (o *OpenAIImage) prepareInputUpload(ctx context.Context, a wireAccount, payload map[string]any) (map[string]any, error) {
	r, e := o.do(ctx, "POST", "/backend-api/files", a, payload, map[string]string{"Content-Type": "application/json"}, false)
	if e != nil {
		return nil, e
	}
	var v map[string]any
	e = decodeOpenAIJSON(r, &v, "upload prepare")
	return v, e
}
func (o *OpenAIImage) uploadInputBlob(ctx context.Context, a wireAccount, endpoint string, data []byte, headers map[string]string) error {
	r, e := o.doAbsolute(ctx, "PUT", endpoint, a, bytes.NewReader(data), headers, false)
	if e != nil {
		return e
	}
	defer r.Body.Close()
	_, e = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
	return e
}
func (o *OpenAIImage) pollConversationOnce(ctx context.Context, a wireAccount, id string) (any, error) {
	r, e := o.do(ctx, "GET", "/backend-api/conversation/"+url.PathEscape(id), a, nil, nil, false)
	if e != nil {
		return nil, e
	}
	var v any
	e = decodeOpenAIJSON(r, &v, "conversation")
	return v, e
}
func setOpenAIHeaders(req *http.Request, path, token string, a map[string]any) {
	fp := buildOpenAIFingerprint(a, token)
	for k, v := range fp.Profile.Headers() {
		req.Header.Set(k, v)
	}
	for k, v := range map[string]string{"Authorization": "Bearer " + token, "Accept": "application/json", "Origin": "https://chatgpt.com", "Referer": "https://chatgpt.com/", "Cache-Control": "no-cache", "OAI-Device-Id": fp.DeviceID, "OAI-Session-Id": fp.SessionID, "OAI-Language": browserLanguage(fp.Profile), "OAI-Client-Version": openAIClientVersion, "OAI-Client-Build-Number": openAIClientBuild, "X-OpenAI-Target-Path": path, "X-OpenAI-Target-Route": path} {
		req.Header.Set(k, v)
	}
}

func numberValue(v any) (float64, bool) { n, ok := v.(float64); return n, ok }
func (o *OpenAIImage) imagePollDeadline(ctx context.Context) time.Time {
	d := time.Now().Add(10 * time.Minute)
	if parent, ok := ctx.Deadline(); ok && parent.Before(d) {
		d = parent
	}
	return d
}
func readBounded(r io.Reader, limit int64) ([]byte, error) {
	b, e := io.ReadAll(io.LimitReader(r, limit+1))
	if e != nil {
		return nil, e
	}
	if int64(len(b)) > limit {
		return nil, errors.New("body too large")
	}
	return b, nil
}
