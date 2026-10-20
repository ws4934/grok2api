package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

var errLiteImageReady = errors.New("Lite 图片已完成")

func (a *Adapter) generateLiteImage(ctx context.Context, request provider.ImageGenerationRequest, count int, format string) (*provider.Response, error) {
	spec, _ := Resolve(request.Model)
	urls := make([]string, 0, count)
	for len(urls) < count {
		value, err := a.generateLiteImageURL(ctx, request.Credential, spec, request.Prompt)
		if err != nil {
			var mediaErr *webMediaUpstreamError
			if errors.As(err, &mediaErr) && len(urls) == 0 {
				return mediaErr.providerResponse(), nil
			}
			var upstreamErr *liteUpstreamError
			if errors.As(err, &upstreamErr) && len(urls) == 0 {
				return upstreamErr.Response(), nil
			}
			if len(urls) > 0 {
				return jsonProviderResponse(http.StatusBadGateway, map[string]any{"error": map[string]any{
					"message": fmt.Sprintf("Lite 图片仅完成 %d/%d 张: %v", len(urls), count, err),
					"type":    "server_error", "code": "image_generation_incomplete",
				}}), nil
			}
			return nil, err
		}
		urls = append(urls, value)
	}
	response, err := a.imageResponse(ctx, request.Credential, urls, nil, count, format)
	if response != nil {
		response.QuotaUnits = count
	}
	return response, err
}

type liteUpstreamError struct {
	StatusCode int
	Status     string
	Body       []byte
}

func (e *liteUpstreamError) Error() string {
	return fmt.Sprintf("Lite 图片上游返回 %d", e.StatusCode)
}

func (e *liteUpstreamError) Response() *provider.Response {
	return &provider.Response{StatusCode: e.StatusCode, Status: e.Status, Header: jsonHeaders(), Body: io.NopCloser(bytes.NewReader(e.Body))}
}

func (a *Adapter) generateLiteImageURL(ctx context.Context, credential account.Credential, spec ModelSpec, prompt string) (string, error) {
	for attempt := 0; attempt < 2; attempt++ {
		upstream, lease, _, err := a.openGatewayChat(ctx, credential, "", spec, normalizedChatInput{Prompt: "Drawing: " + prompt}, gatewayOpenOptions{deferForbidden: true})
		if err != nil {
			return "", err
		}
		if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(upstream.Body, webMediaDiagnosticBodyLimit+1))
			_ = upstream.Body.Close()
			truncated := len(body) > webMediaDiagnosticBodyLimit
			if truncated {
				body = body[:webMediaDiagnosticBodyLimit]
			}
			if upstream.StatusCode == http.StatusForbidden {
				upstreamErr := newWebMediaUpstreamError(upstream.StatusCode, body, truncated)
				a.logWebMediaUpstreamRejection("image_lite_handshake", upstream, upstreamErr)
				if isClearanceRefreshableMediaError(upstreamErr) {
					// The failed WebSocket handshake invalidates the current browser
					// session; retry once on a fresh lease.
					lease.InvalidateClearance()
					if attempt == 0 {
						lease.Release()
						continue
					}
				}
				lease.Release()
				return "", upstreamErr
			}
			a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, upstream.StatusCode, nil)
			lease.Release()
			return "", &liteUpstreamError{StatusCode: upstream.StatusCode, Status: upstream.Status, Body: body}
		}
		firstImage := ""
		capture := &boundedCapture{limit: 8 << 20}
		parsed, consumeErr := consumeUpstream(io.TeeReader(upstream.Body, capture), func(kind, delta string) error {
			if kind != "image" || strings.TrimSpace(delta) == "" {
				return nil
			}
			firstImage = delta
			return errLiteImageReady
		})
		_ = upstream.Body.Close()
		if consumeErr != nil && !errors.Is(consumeErr, errLiteImageReady) {
			if errors.Is(consumeErr, errWebUsageLimit) {
				lease.Release()
				response := jsonProviderResponse(http.StatusTooManyRequests, map[string]any{"error": map[string]any{
					"message": "Grok Imagine 速率限制中，请稍后重试",
					"type":    "rate_limit_error",
					"code":    "usage_limit_reached",
				}})
				body, _ := io.ReadAll(response.Body)
				_ = response.Body.Close()
				return "", &liteUpstreamError{StatusCode: http.StatusTooManyRequests, Status: "429 Too Many Requests", Body: body}
			}
			status := 0
			if errors.Is(consumeErr, errWebAntiBot) {
				status = http.StatusForbidden
				// A challenge can arrive inside an otherwise successful stream,
				// so the handshake path cannot invalidate it for us.
				lease.InvalidateClearance()
				if attempt == 0 {
					lease.Release()
					continue
				}
			}
			a.feedbackStreamError(ctx, lease, consumeErr)
			lease.Release()
			if status == http.StatusForbidden {
				response := antiBotProviderResponse()
				body, _ := io.ReadAll(response.Body)
				_ = response.Body.Close()
				return "", &liteUpstreamError{StatusCode: status, Status: "403 Forbidden", Body: body}
			}
			return "", consumeErr
		}
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, http.StatusOK, nil)
		lease.Release()
		if firstImage != "" {
			return firstImage, nil
		}
		if len(parsed.Images) == 0 {
			parsed.Images = extractMarkdownImages(parsed.Text.String())
		}
		if len(parsed.Images) == 0 {
			parsed.Images = extractCapturedImageURLs(capture.Bytes())
		}
		if len(parsed.Images) == 0 {
			diagnostics := inspectLiteCapture(capture.Bytes())
			a.log().Warn("web_lite_image_not_found",
				"account_id", credential.ID,
				"captured_bytes", len(capture.Bytes()),
				"frames", diagnostics.Frames,
				"response_fields", diagnostics.ResponseFields,
				"message_tags", diagnostics.MessageTags,
				"image_chunks", diagnostics.ImageChunks,
				"image_urls", diagnostics.ImageURLs,
				"image_fields", diagnostics.ImageFields,
				"max_progress", diagnostics.MaxProgress,
				"soft_stop", diagnostics.SoftStop,
				"upstream_error_code", diagnostics.ErrorCode,
				"upstream_error", diagnostics.ErrorMessage,
			)
			return "", fmt.Errorf("Grok Web Lite 响应结束但未解析到最终图片")
		}
		// Lite 上游固定生成两张，但每次查询只计一次 Fast 额度；按旧协议取首张并为 n 重复查询。
		return parsed.Images[0], nil
	}
	return "", fmt.Errorf("Grok Web Lite 图片签名刷新失败")
}
