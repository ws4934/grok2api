package adobe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
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
	// timeout 限制一次上游调用（含读完响应体）；0 不限制。
	timeout time.Duration
}

const (
	maxResponseBytes = 16 << 20
	// maxDownloadBytes 容纳 4K PNG；超出时报错而不是静默截断成坏图。
	maxDownloadBytes = 64 << 20
)

// pollInterval 是阻塞轮询两次 GET 之间的间隔（测试会调小）。
var pollInterval = 3 * time.Second

type submission struct {
	pollURL string
}

type pollResult struct {
	status     string // IN_PROGRESS | SUCCEEDED | FAILED
	progress   int
	outputURLs []string
	// failure 是 FAILED 时 Adobe 给出的原因。
	failure string
}

type client struct {
	do doer
	rc requestContext
}

var chromeVersion = regexp.MustCompile(`Chrome/(\d+)`)

// browserHeaders 是浏览器每个请求都带的头：User-Agent 与 Chrome 客户端提示。ua 取自
// 出口租约，出口的 TLS 指纹也按它选 Chrome 版本，两者保持一致。
func browserHeaders(ua string) map[string]string {
	version := "146"
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
		"Accept-Language":    "en-US,en;q=0.9",
		"sec-ch-ua":          fmt.Sprintf(`"Chromium";v="%s", "Google Chrome";v="%s", "Not/A)Brand";v="99"`, version, version),
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": platform,
		"sec-fetch-site":     "cross-site",
		"sec-fetch-mode":     "cors",
		"sec-fetch-dest":     "empty",
	}
}

func (c *client) submitHeaders(noncePrompt string) map[string]string {
	headers := map[string]string{}
	headers["Authorization"] = "Bearer " + c.rc.token
	headers["x-api-key"] = c.rc.apiKey
	headers["Content-Type"] = "application/json"
	headers["Accept"] = "*/*"
	headers["Origin"] = "https://firefly.adobe.com"
	headers["Referer"] = "https://firefly.adobe.com/"
	headers["x-arp-session-id"] = c.rc.sherlock
	if noncePrompt != "" {
		if nonce := submitNonce(c.rc.token, noncePrompt); nonce != "" {
			headers["x-nonce"] = nonce
		}
	}
	return headers
}

func (c *client) pollHeaders() map[string]string {
	headers := map[string]string{}
	headers["Authorization"] = "Bearer " + c.rc.token
	headers["Accept"] = "*/*"
	headers["Origin"] = "https://firefly.adobe.com"
	headers["Referer"] = "https://firefly.adobe.com/"
	return headers
}

// send 在单次调用超时内发出请求并读完响应体（最多 limit 字节，超出报错）。
func (c *client) send(ctx context.Context, method, rawURL string, body io.Reader, headers map[string]string, limit int64) (*http.Response, []byte, error) {
	if c.rc.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.rc.timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, nil, &transportError{err: err}
	}
	for k, v := range browserHeaders(c.rc.userAgent) {
		req.Header.Set(k, v)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.do.Do(req)
	if err != nil {
		return nil, nil, &transportError{err: withoutURL(err)}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return resp, nil, &transportError{err: err}
	}
	if int64(len(raw)) > limit {
		return resp, nil, &upstreamError{stage: "response", status: http.StatusBadGateway, body: fmt.Sprintf("response exceeds %d MiB", limit>>20)}
	}
	return resp, raw, nil
}

// withoutURL 去掉 *url.Error 里的完整地址：预签名链接带签名，错误文本会落库并展示。
func withoutURL(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	host := ""
	if parsed, parseErr := url.Parse(ue.URL); parseErr == nil {
		host = parsed.Host
	}
	return fmt.Errorf("%s %s: %w", ue.Op, host, ue.Err)
}

// request 发起一次 JSON 调用，返回解码后的响应体、状态码与响应头。传输失败
// 包装成 *transportError；非 2xx 状态由调用方归类。
func (c *client) request(ctx context.Context, method, rawURL string, body io.Reader, headers map[string]string) (int, map[string]any, http.Header, error) {
	resp, raw, err := c.send(ctx, method, rawURL, body, headers, maxResponseBytes)
	if err != nil {
		if resp == nil {
			return 0, nil, nil, err
		}
		return resp.StatusCode, nil, resp.Header, err
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

// submitJob 提交一个生成任务并返回轮询链接。noncePrompt 非空时带官网的 x-nonce。
func (c *client) submitJob(ctx context.Context, submitURL string, payload map[string]any, noncePrompt string) (submission, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return submission{}, err
	}
	status, data, hdr, reqErr := c.request(ctx, http.MethodPost, submitURL, bytes.NewReader(encoded), c.submitHeaders(noncePrompt))
	if reqErr != nil {
		return submission{}, reqErr
	}
	if status < 200 || status >= 300 {
		return submission{}, classifyUpstream("submit", status, bodyText(data))
	}
	link := resultLink(hdr, data)
	if link == "" {
		return submission{}, &upstreamError{stage: "submit", status: 502, body: "no polling URL"}
	}
	return submission{pollURL: link}, nil
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
	// 官网只在 x-task-status 为 COMPLETED 时收尾：进行中的响应可能已带回部分输出。
	if len(urls) > 0 && !strings.EqualFold(hdr.Get("x-task-status"), "IN_PROGRESS") {
		return pollResult{status: "SUCCEEDED", progress: 100, outputURLs: urls}, nil
	}
	switch taskStatus {
	case "FAILED", "CANCELLED", "ERROR":
		failure := upstreamReason(bodyText(data))
		if failure == "" {
			failure = truncate(bodyText(data), 300)
		}
		return pollResult{status: "FAILED", progress: progress, failure: failure}, nil
	default:
		return pollResult{status: "IN_PROGRESS", progress: progress}, nil
	}
}

// pollUntilDone 阻塞轮询直到任务成功（返回全部输出 URL）、失败或上下文结束。
// 连续的瞬时失败在 provider.VideoPollTransientLimit 次内只重试，不放弃已提交的任务。
func pollUntilDone(ctx context.Context, c *client, pollURL, kind string, progress func(int)) ([]string, error) {
	failures := 0
	for {
		result, err := c.poll(ctx, pollURL, kind)
		switch {
		case err != nil:
			if failures++; !provider.IsTransientVideoPollError(err) || failures >= provider.VideoPollTransientLimit || ctx.Err() != nil {
				return nil, err
			}
		case result.status == "SUCCEEDED":
			return result.outputURLs, nil
		case result.status == "FAILED":
			return nil, &upstreamError{stage: "poll", status: 502, body: "generation failed: " + result.failure, reason: result.failure}
		default:
			failures = 0
			if progress != nil && result.progress > 0 {
				progress(result.progress)
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func (c *client) upload(ctx context.Context, uploadURL string, data []byte, mime, kind string) (string, error) {
	headers := map[string]string{}
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

// download 读取 Adobe 的预签名结果链接。
func (c *client) download(ctx context.Context, outputURL string) ([]byte, error) {
	resp, raw, err := c.send(ctx, http.MethodGet, outputURL, nil, map[string]string{"Accept": "*/*"}, maxDownloadBytes)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, classifyUpstream("download", resp.StatusCode, truncate(string(raw), 300))
	}
	return raw, nil
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

func progressValue(value any) int {
	switch v := value.(type) {
	case float64:
		if v <= 1 {
			v *= 100
		}
		return clampPercent(int(v))
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(v, "%")))
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

// submitNonce 复刻官网的 sha256(`${userId}-${prompt.slice(0, 256)}`) nonce。slice 按
// UTF-16 码元截取，切开的代理对经 TextEncoder 编成 U+FFFD，utf16.Decode 也是如此。
func submitNonce(token, prompt string) string {
	claims := decodeJWTClaims(token)
	userID := firstString(claims, "user_id", "aa_id", "sub")
	if userID == "" || prompt == "" {
		return ""
	}
	if units := utf16.Encode([]rune(prompt)); len(units) > 256 {
		prompt = string(utf16.Decode(units[:256]))
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
