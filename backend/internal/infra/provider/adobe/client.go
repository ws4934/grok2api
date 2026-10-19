package adobe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// doer 通过绑定的出口发起一次 HTTP 请求；egress 的 Lease 即实现了它。
type doer interface {
	Do(*http.Request) (*http.Response, error)
}

// requestContext 携带单次调用的 Adobe 凭据与设置。
type requestContext struct {
	token     string
	sherlock  string
	apiKey    string
	userAgent string
}

const maxResponseBytes = 16 << 20

type submission struct {
	pollURL        string
	upstreamTaskID string
}

type pollResult struct {
	status     string // IN_PROGRESS | SUCCEEDED | FAILED
	progress   int
	outputURL  string
	outputURLs []string
	mimeType   string
}

type client struct {
	do doer
	rc requestContext
}

// browserHeaders 返回稳定的 Chrome 客户端提示头。
func (c *client) browserHeaders() map[string]string {
	ua := c.rc.userAgent
	version := "145"
	if m := chromeVersion.FindStringSubmatch(ua); len(m) == 2 {
		version = m[1]
	}
	platform := `"Windows"`
	switch {
	case strings.Contains(ua, "Macintosh"):
		platform = `"macOS"`
	case strings.Contains(ua, "Linux"):
		platform = `"Linux"`
	}
	return map[string]string{
		"User-Agent":         ua,
		"sec-ch-ua":          fmt.Sprintf(`"Chromium";v="%s", "Google Chrome";v="%s", "Not/A)Brand";v="99"`, version, version),
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": platform,
		"sec-fetch-site":     "cross-site",
		"sec-fetch-mode":     "cors",
		"sec-fetch-dest":     "empty",
	}
}

func (c *client) submitHeaders(prompt string, video bool) (map[string]string, error) {
	if c.rc.sherlock == "" {
		return nil, &upstreamError{stage: "submit", status: 503, body: "missing x-arp-session-id", retryable: true}
	}
	headers := c.browserHeaders()
	headers["Authorization"] = "Bearer " + c.rc.token
	headers["x-api-key"] = c.rc.apiKey
	headers["Content-Type"] = "application/json"
	headers["Accept"] = "*/*"
	headers["Origin"] = "https://firefly.adobe.com"
	headers["Referer"] = "https://firefly.adobe.com/"
	headers["Accept-Language"] = "en-US,en;q=0.9"
	headers["x-arp-session-id"] = c.rc.sherlock
	if prompt != "" && !video {
		if nonce := submitNonce(c.rc.token, prompt); nonce != "" {
			headers["x-nonce"] = nonce
		}
	}
	return headers, nil
}

func (c *client) pollHeaders() map[string]string {
	headers := c.browserHeaders()
	headers["Authorization"] = "Bearer " + c.rc.token
	headers["Accept"] = "*/*"
	headers["Origin"] = "https://firefly.adobe.com"
	headers["Referer"] = "https://firefly.adobe.com/"
	return headers
}

// request 发起一次调用，返回解码后的 JSON 体、状态码与响应头。传输失败包装成
// *transportError；非 2xx 状态由调用方归类。
func (c *client) request(ctx context.Context, method, rawURL string, body io.Reader, headers map[string]string) (int, map[string]any, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return 0, nil, nil, &transportError{err: err}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.do.Do(req)
	if err != nil {
		return 0, nil, nil, &transportError{err: err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return resp.StatusCode, nil, resp.Header, &transportError{err: err}
	}
	var data map[string]any
	if len(bytes.TrimSpace(raw)) > 0 {
		_ = json.Unmarshal(raw, &data)
	}
	if data == nil {
		data = map[string]any{"_raw": string(raw)}
	}
	return resp.StatusCode, data, resp.Header, nil
}

func (c *client) submitImage(ctx context.Context, submitURL string, payloads []map[string]any, prompt string) (submission, error) {
	headers, err := c.submitHeaders(prompt, false)
	if err != nil {
		return submission{}, err
	}
	var last error
	for _, payload := range payloads {
		encoded, _ := json.Marshal(payload)
		status, data, hdr, reqErr := c.request(ctx, http.MethodPost, submitURL, bytes.NewReader(encoded), headers)
		if reqErr != nil {
			last = reqErr
			break
		}
		if status >= 200 && status < 300 {
			link := resultLink(hdr, data)
			if link == "" {
				return submission{}, &upstreamError{stage: "submit", status: 502, body: "no polling URL"}
			}
			return submission{pollURL: link, upstreamTaskID: jobIDFromURL(link)}, nil
		}
		ue := classifyUpstream("submit", status, bodyText(data))
		last = ue
		// 只有普通 4xx 拒绝才停止尝试后续候选 payload。
		if status == 401 || status == 403 || (status < 500 && status != 429 && status != 451) {
			break
		}
	}
	if last == nil {
		last = &upstreamError{stage: "submit", status: 502, body: "image submission failed"}
	}
	return submission{}, last
}

func (c *client) submitVideo(ctx context.Context, submitURL string, payload map[string]any) (submission, error) {
	headers, err := c.submitHeaders("", true)
	if err != nil {
		return submission{}, err
	}
	encoded, _ := json.Marshal(payload)
	status, data, hdr, reqErr := c.request(ctx, http.MethodPost, submitURL, bytes.NewReader(encoded), headers)
	if reqErr != nil {
		return submission{}, reqErr
	}
	if status < 200 || status >= 300 {
		return submission{}, classifyUpstream("submit_video", status, bodyText(data))
	}
	link := resultLink(hdr, data)
	if link == "" {
		return submission{}, &upstreamError{stage: "submit_video", status: 502, body: "no polling URL"}
	}
	return submission{pollURL: link, upstreamTaskID: jobIDFromURL(link)}, nil
}

func (c *client) poll(ctx context.Context, pollURL, kind string) (pollResult, error) {
	status, data, hdr, reqErr := c.request(ctx, http.MethodGet, pollURL, nil, c.pollHeaders())
	if reqErr != nil {
		return pollResult{}, reqErr
	}
	if status < 200 || status >= 300 {
		return pollResult{}, classifyUpstream("poll", status, bodyText(data))
	}
	taskStatus := strings.ToUpper(strings.TrimSpace(stringField(data, "status")))
	if taskStatus == "" {
		taskStatus = strings.ToUpper(hdr.Get("x-task-status"))
	}
	progress := progressValue(data["progress"])
	var urls []string
	if outputs, ok := data["outputs"].([]any); ok {
		for _, item := range outputs {
			obj, _ := item.(map[string]any)
			media, _ := obj[kind].(map[string]any)
			link := stringField(media, "presignedUrl")
			if link == "" {
				link = stringField(media, "url")
			}
			if link == "" {
				link = stringField(obj, "presignedUrl")
			}
			if link != "" {
				urls = append(urls, link)
			}
		}
	}
	mime := "image/png"
	if kind == "video" {
		mime = "video/mp4"
	}
	if len(urls) > 0 {
		return pollResult{status: "SUCCEEDED", progress: 100, outputURL: urls[0], outputURLs: urls, mimeType: mime}, nil
	}
	switch taskStatus {
	case "FAILED", "CANCELLED", "ERROR":
		return pollResult{status: "FAILED", progress: progress}, nil
	default:
		return pollResult{status: "IN_PROGRESS", progress: progress}, nil
	}
}

func (c *client) upload(ctx context.Context, uploadURL string, data []byte, mime, kind string) (string, error) {
	headers := c.browserHeaders()
	headers["Authorization"] = "Bearer " + c.rc.token
	headers["x-api-key"] = c.rc.apiKey
	headers["Content-Type"] = mime
	headers["Accept"] = "application/json"
	status, body, _, reqErr := c.request(ctx, http.MethodPost, uploadURL, bytes.NewReader(data), headers)
	if reqErr != nil {
		return "", reqErr
	}
	if status < 200 || status >= 300 {
		return "", classifyUpstream("upload_"+kind, status, bodyText(body))
	}
	if id := uploadedID(body, kind); id != "" {
		return id, nil
	}
	return "", &upstreamError{stage: "upload_" + kind, status: 502, body: "no asset ID"}
}

func (c *client) download(ctx context.Context, outputURL string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, outputURL, nil)
	if err != nil {
		return nil, "", &transportError{err: err}
	}
	req.Header.Set("Accept", "*/*")
	resp, err := c.do.Do(req)
	if err != nil {
		return nil, "", &transportError{err: err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, "", &transportError{err: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", classifyUpstream("download", resp.StatusCode, truncate(string(raw), 300))
	}
	return raw, resp.Header.Get("Content-Type"), nil
}

// --- 解析辅助 ---

func uploadedID(data map[string]any, kind string) string {
	if images, ok := data["images"].([]any); ok && len(images) > 0 {
		if obj, ok := images[0].(map[string]any); ok {
			if id := stringField(obj, "id"); id != "" {
				return id
			}
		}
	}
	if id := stringField(data, "id"); id != "" {
		return id
	}
	for _, key := range []string{kind + "s", "assets"} {
		if arr, ok := data[key].([]any); ok {
			for _, item := range arr {
				if obj, ok := item.(map[string]any); ok {
					if id := stringField(obj, "id"); id != "" {
						return id
					}
				}
			}
		}
	}
	return ""
}

func resultLink(headers http.Header, data map[string]any) string {
	if override := headers.Get("x-override-status-link"); override != "" {
		return override
	}
	links, _ := data["links"].(map[string]any)
	switch result := links["result"].(type) {
	case string:
		return result
	case map[string]any:
		return stringField(result, "href")
	}
	return ""
}

func jobIDFromURL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

func progressValue(value any) int {
	switch v := value.(type) {
	case float64:
		if v <= 1 {
			v *= 100
		}
		return clampPercent(int(v))
	case string:
		n := atoi(strings.TrimSuffix(v, "%"))
		return clampPercent(n)
	}
	return 0
}

func clampPercent(v int) int {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func stringField(data map[string]any, key string) string {
	if data == nil {
		return ""
	}
	if v, ok := data[key].(string); ok {
		return v
	}
	return ""
}

func bodyText(data map[string]any) string {
	if raw, ok := data["_raw"].(string); ok {
		return raw
	}
	encoded, _ := json.Marshal(data)
	return string(encoded)
}

// submitNonce 复刻官网的 sha256(userId-prompt[:256]) 图像 nonce。
func submitNonce(token, prompt string) string {
	claims := decodeJWTClaims(token)
	userID := firstString(claims, "user_id", "aa_id", "sub")
	if userID == "" || prompt == "" {
		return ""
	}
	if len(prompt) > 256 {
		prompt = prompt[:256]
	}
	sum := sha256.Sum256([]byte(userID + "-" + prompt))
	return hex.EncodeToString(sum[:])
}

func decodeJWTClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		if raw, err = base64.StdEncoding.DecodeString(parts[1]); err != nil {
			return nil
		}
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return nil
	}
	return claims
}

func firstString(claims map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := claims[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
