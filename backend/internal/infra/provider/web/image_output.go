package web

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/grokimage"
)

func (a *Adapter) imageResponse(ctx context.Context, credential account.Credential, urls, blobs []string, count int, format string) (*provider.Response, error) {
	data := make([]any, 0, min(count, len(urls)))
	var firstError error
	for index := 0; index < count && index < len(urls); index++ {
		blob := ""
		if index < len(blobs) {
			blob = blobs[index]
		}
		item, err := a.imageDataItem(ctx, credential, imagineImageValue{URL: urls[index], Blob: blob}, format)
		if err != nil {
			if firstError == nil {
				firstError = err
			}
			continue
		}
		data = append(data, item)
	}
	if len(data) == 0 && firstError != nil {
		return nil, firstError
	}
	if len(data) < count {
		a.log().Warn("web_image_result_partial", "requested", count, "delivered", len(data))
	}
	return jsonProviderResponse(http.StatusOK, map[string]any{"created": time.Now().Unix(), "data": data}), nil
}

func (a *Adapter) imageDataItem(ctx context.Context, credential account.Credential, image imagineImageValue, format string) (map[string]any, error) {
	if a.assets == nil {
		return nil, provider.NewMediaPostProcessingError(provider.MediaPostProcessingStorage, fmt.Errorf("图片媒体存储未配置"))
	}
	raw, err := a.imageBytes(ctx, credential, image)
	if err != nil {
		return nil, provider.NewMediaPostProcessingError(provider.MediaPostProcessingDownload, err)
	}
	asset, err := grokimage.SaveWithRetry(ctx, a.assets, raw)
	if err != nil {
		return nil, provider.NewMediaPostProcessingError(provider.MediaPostProcessingStorage, err)
	}
	if format != "b64_json" {
		return map[string]any{"url": a.assets.PublicImageURL(asset.ID), "mime_type": asset.MIMEType, "revised_prompt": ""}, nil
	}
	return map[string]any{"b64_json": base64.StdEncoding.EncodeToString(raw), "mime_type": asset.MIMEType, "revised_prompt": ""}, nil
}

func (a *Adapter) imageBytes(ctx context.Context, credential account.Credential, image imagineImageValue) ([]byte, error) {
	if strings.TrimSpace(image.Blob) != "" {
		raw, err := decodeImageBlob(image.Blob)
		if err == nil {
			return raw, nil
		}
		if strings.TrimSpace(image.URL) == "" {
			return nil, err
		}
	}
	return a.downloadImage(ctx, credential, image.URL)
}

func imageOutputFormat(raw []byte) string {
	mimeType := http.DetectContentType(raw)
	switch mimeType {
	case "image/png":
		return "png"
	case "image/webp":
		return "webp"
	default:
		return "jpeg"
	}
}

func (a *Adapter) saveStreamImage(ctx context.Context, raw []byte) error {
	if a.assets == nil {
		return provider.NewMediaPostProcessingError(provider.MediaPostProcessingStorage, fmt.Errorf("图片媒体存储未配置"))
	}
	if _, err := grokimage.SaveWithRetry(ctx, a.assets, raw); err != nil {
		return provider.NewMediaPostProcessingError(provider.MediaPostProcessingStorage, err)
	}
	return nil
}

func (a *Adapter) downloadImage(ctx context.Context, credential account.Credential, rawURL string) ([]byte, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || !grokimage.TrustedAssetHost(parsed.Hostname()) || parsed.User != nil {
		return nil, fmt.Errorf("图片内容 URL 不受信任")
	}
	token, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return nil, err
	}
	downloadCtx, cancel := context.WithTimeout(ctx, imageDownloadTimeout)
	defer cancel()
	var lastErr error
	for attempt := 0; attempt < grokimage.OutputAttempts; attempt++ {
		raw, retryable, attemptErr := a.downloadImageAttempt(downloadCtx, credential, token, parsed.String())
		if attemptErr == nil {
			return raw, nil
		}
		lastErr = attemptErr
		if !retryable || downloadCtx.Err() != nil || attempt+1 >= grokimage.OutputAttempts {
			break
		}
		if err := grokimage.WaitRetry(downloadCtx, attempt); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// downloadImageAttempt 每次沿用同一账号，只允许出口管理器重新选择资源节点。
func (a *Adapter) downloadImageAttempt(ctx context.Context, credential account.Credential, token, rawURL string) ([]byte, bool, error) {
	lease, err := a.egress.AcquireCredential(ctx, domainegress.ScopeWebAsset, credential)
	if err != nil {
		return nil, true, err
	}
	defer lease.Release()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, false, err
	}
	request.Header = buildHeaders(token, lease, "")
	request.Header.Del("Content-Type")
	response, err := lease.Do(request)
	if err != nil {
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, 0, err)
		return nil, ctx.Err() == nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, response.StatusCode, nil)
		retryable := response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusTooEarly || response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500
		return nil, retryable, fmt.Errorf("下载图片返回 %d", response.StatusCode)
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]))
	if contentType != "" && !strings.HasPrefix(contentType, "image/") {
		return nil, false, fmt.Errorf("上游图片 Content-Type 无效")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (32<<20)+1))
	if err != nil {
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, 0, err)
		return nil, ctx.Err() == nil, fmt.Errorf("读取图片内容: %w", err)
	}
	if len(raw) > 32<<20 {
		return nil, false, fmt.Errorf("图片下载超过 32 MiB")
	}
	a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, response.StatusCode, nil)
	return raw, false, nil
}

func decodeImageBlob(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(value), "data:") {
		comma := strings.IndexByte(value, ',')
		if comma < 0 || !strings.Contains(strings.ToLower(value[:comma]), ";base64") {
			return nil, fmt.Errorf("图片 blob data URI 无效")
		}
		value = value[comma+1:]
	}
	if value == "" || base64.StdEncoding.DecodedLen(len(value)) > 32<<20 {
		return nil, fmt.Errorf("图片 blob 为空或超过 32 MiB")
	}
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(value)
	}
	if err != nil || len(raw) == 0 || len(raw) > 32<<20 {
		return nil, fmt.Errorf("图片 blob Base64 无效")
	}
	return raw, nil
}
