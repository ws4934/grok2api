package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// EditImage retries the complete browser media flow once after a challenge
// response. Reacquiring the lease is required because the failed lease keeps
// the immutable browser-session cookies that were rejected upstream.
func (a *Adapter) EditImage(ctx context.Context, request provider.ImageEditRequest) (*provider.Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		response, err := a.editImageAttempt(ctx, request)
		if err == nil {
			return response, nil
		}
		var upstreamErr *webMediaUpstreamError
		if !errors.As(err, &upstreamErr) || !isClearanceRefreshableMediaError(upstreamErr) || attempt > 0 {
			if errors.As(err, &upstreamErr) {
				return upstreamErr.providerResponse(), nil
			}
			return nil, err
		}
		a.log().Warn("web_image_clearance_retry", "operation", "edit", "status", upstreamErr.status, "body_kind", upstreamErr.bodyKind)
	}
	return nil, fmt.Errorf("图片编辑 Clearance 重试耗尽")
}

func (a *Adapter) editImageAttempt(ctx context.Context, request provider.ImageEditRequest) (*provider.Response, error) {
	if strings.TrimSpace(request.Quality) != "" {
		return invalidImageRequest("Grok Web 图片模型不支持 quality")
	}
	if len(request.ImageURLs) == 0 || len(request.ImageURLs) > 8 {
		return jsonProviderResponse(http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "image 数量必须在 1 到 8 之间", "type": "invalid_request_error"}}), nil
	}
	count := request.Count
	if count <= 0 {
		count = 1
	}
	if count != 1 {
		return invalidImageRequest("Grok Web 图片编辑当前仅支持 n=1")
	}
	if request.PartialImages < 0 || request.PartialImages > 3 {
		return invalidImageRequest("partial_images 必须在 0 到 3 之间")
	}
	if request.PartialImages > 0 && !request.Streaming {
		return invalidImageRequest("partial_images 仅可在 stream=true 时使用")
	}
	resolution := strings.ToLower(strings.TrimSpace(request.Resolution))
	if resolution == "" {
		resolution = "1k"
	}
	if resolution != "1k" {
		return invalidImageRequest("Grok Web 图片编辑当前仅支持 resolution=1k")
	}
	format := strings.ToLower(strings.TrimSpace(request.ResponseFormat))
	if format == "" {
		format = "url"
	}
	if format != "url" && format != "b64_json" {
		return invalidImageRequest("response_format 必须是 url 或 b64_json")
	}
	ratio, err := resolveImageEditAspectRatio(request.AspectRatio, request.Size)
	if err != nil {
		return invalidImageRequest(err.Error())
	}
	cfg := a.config()
	token, err := a.cipher.Decrypt(request.Credential.EncryptedAccessToken)
	if err != nil {
		return nil, err
	}
	lease, err := a.egress.AcquireCredential(ctx, domainegress.ScopeWeb, request.Credential)
	if err != nil {
		return nil, err
	}
	leaseOwned := true
	defer func() {
		if leaseOwned {
			lease.Release()
		}
	}()
	images := make([]provider.ImageInput, 0, len(request.ImageURLs))
	for _, rawURL := range request.ImageURLs {
		image, loadErr := a.loadChatImage(ctx, lease, rawURL, cfg.MaxInputImageBytes)
		if loadErr != nil {
			return invalidImageRequest(loadErr.Error())
		}
		images = append(images, image)
	}
	assets := make([]string, 0, len(images))
	for _, image := range images {
		uploaded, uploadErr := a.uploadFileV2Direct(ctx, cfg, lease, token, image, cfg.BaseURL+"/imagine", imagineSelfUploadSource, "image_edit_upload")
		if uploadErr != nil {
			return nil, uploadErr
		}
		if uploaded.MetadataID == "" {
			return nil, fmt.Errorf("上传图片成功但上游未返回 fileMetadataId")
		}
		assets = append(assets, uploaded.MetadataID)
	}
	payload := buildImageEditPayload(request.Prompt, assets, ratio)
	response, err := a.postJSONWithReferer(ctx, cfg, lease, token, cfg.BaseURL+"/rest/app-chat/conversations/new", payload, time.Duration(cfg.ImageTimeoutSeconds)*time.Second, cfg.BaseURL+"/imagine")
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, webMediaDiagnosticBodyLimit+1))
		_ = response.Body.Close()
		truncated := len(body) > webMediaDiagnosticBodyLimit
		if truncated {
			body = body[:webMediaDiagnosticBodyLimit]
		}
		upstreamErr := newWebMediaUpstreamError(response.StatusCode, body, truncated)
		a.logWebMediaUpstreamRejection("image_edit_generate", response, upstreamErr)
		return nil, upstreamErr
	}
	if request.Streaming {
		reader, writer := io.Pipe()
		streamCtx, cancel := context.WithCancel(ctx)
		leaseOwned = false
		go a.streamImageEdit(streamCtx, writer, response.Body, lease, request.Credential, request.PartialImages, request.Size, ratio)
		return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: streamHeaders(), Body: &cancelBody{ReadCloser: reader, cancel: cancel}, QuotaUnits: 1}, nil
	}
	defer response.Body.Close()
	capture := &boundedCapture{limit: 8 << 20}
	parsed, consumeErr := consumeUpstream(io.TeeReader(response.Body, capture), nil)
	if consumeErr != nil {
		return nil, consumeErr
	}
	urls := imageEditResultURLs(&parsed, capture.Bytes())
	if len(urls) == 0 {
		return jsonProviderResponse(http.StatusBadGateway, map[string]any{"error": map[string]any{
			"message": "上游未返回可用的编辑图片",
			"type":    "server_error", "code": "image_edit_incomplete",
		}}), nil
	}
	result, err := a.imageResponse(ctx, request.Credential, urls, nil, 1, format)
	if result != nil {
		result.QuotaUnits = 1
	}
	return result, err
}

func buildImageEditPayload(prompt string, assets []string, aspectRatio string) map[string]any {
	imageToImage := map[string]any{
		"prompt":      prompt,
		"inputAssets": assets,
	}
	if aspectRatio != "" {
		imageToImage["aspectRatio"] = aspectRatio
	}
	return map[string]any{
		"modelName": "imagine-image-edit", "message": prompt,
		"enableImageStreaming": true, "enableSideBySide": true, "sendFinalMetadata": true,
		"mediaGenInput": map[string]any{"imageToImage": imageToImage},
	}
}

func resolveImageEditAspectRatio(aspectRatio, size string) (string, error) {
	if strings.TrimSpace(aspectRatio) == "" && strings.TrimSpace(size) == "" {
		return "", nil
	}
	return resolveImageAspectRatio(aspectRatio, size)
}

type imageEditStreamFrame struct {
	URL       string
	Progress  int
	Moderated bool
}

func parseImageEditStreamFrame(data []byte) (imageEditStreamFrame, bool) {
	var root map[string]any
	if json.Unmarshal(data, &root) != nil {
		return imageEditStreamFrame{}, false
	}
	result, _ := root["result"].(map[string]any)
	response, _ := result["response"].(map[string]any)
	imageResponse, _ := response["streamingImageGenerationResponse"].(map[string]any)
	if imageResponse == nil {
		return imageEditStreamFrame{}, false
	}
	rawURL := firstString(imageResponse, "imageUrl", "url")
	if rawURL == "" {
		return imageEditStreamFrame{}, false
	}
	progress, hasProgress := numberAsInt(imageResponse["progress"])
	if !hasProgress {
		if final, _ := imageResponse["isFinal"].(bool); !final {
			return imageEditStreamFrame{}, false
		}
		progress = 100
	}
	moderated, _ := imageResponse["moderated"].(bool)
	return imageEditStreamFrame{URL: absoluteAssetURL(rawURL), Progress: progress, Moderated: moderated}, true
}

func (a *Adapter) streamImageEdit(
	ctx context.Context,
	writer *io.PipeWriter,
	source io.ReadCloser,
	lease *egress.Lease,
	credential account.Credential,
	partialImages int,
	size string,
	aspectRatio string,
) {
	defer lease.Release()
	defer source.Close()
	createdAt := time.Now().Unix()
	parsed := parsedChat{}
	capture := &boundedCapture{limit: 8 << 20}
	seenPartials := make(map[string]struct{}, partialImages)
	partialIndex := 0
	consumeErr := consumeJSONObjects(io.TeeReader(source, capture), 8<<20, func(data []byte) error {
		if _, _, err := parseUpstreamFrame(data, &parsed); err != nil {
			return err
		}
		frame, ok := parseImageEditStreamFrame(data)
		if !ok || frame.Moderated || frame.Progress >= 100 || partialIndex >= partialImages {
			return nil
		}
		if _, exists := seenPartials[frame.URL]; exists {
			return nil
		}
		raw, err := a.imageBytes(ctx, credential, imagineImageValue{URL: frame.URL})
		if err != nil {
			// partial_images 是尽力而为；预览下载失败不应阻断最终编辑结果。
			return nil
		}
		if err := writeSSE(writer, "image_edit.partial_image", openAIImageEditStreamEvent(
			"image_edit.partial_image", raw, createdAt, imageEditEventSize(size, aspectRatio), partialIndex,
		)); err != nil {
			return err
		}
		seenPartials[frame.URL] = struct{}{}
		partialIndex++
		return nil
	})
	if consumeErr != nil {
		a.feedbackStreamError(ctx, lease, consumeErr)
		_ = writer.CloseWithError(consumeErr)
		return
	}
	urls := imageEditResultURLs(&parsed, capture.Bytes())
	if len(urls) == 0 {
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, http.StatusOK, nil)
		_ = writer.CloseWithError(fmt.Errorf("上游未返回可用的编辑图片"))
		return
	}
	raw, err := a.imageBytes(ctx, credential, imagineImageValue{URL: urls[0]})
	if err != nil {
		_ = writer.CloseWithError(provider.NewMediaPostProcessingError(provider.MediaPostProcessingDownload, err))
		return
	}
	if err := a.saveStreamImage(ctx, raw); err != nil {
		_ = writer.CloseWithError(err)
		return
	}
	if err := writeSSE(writer, "image_edit.completed", openAIImageEditStreamEvent(
		"image_edit.completed", raw, createdAt, imageEditEventSize(size, aspectRatio), 0,
	)); err != nil {
		_ = writer.CloseWithError(err)
		return
	}
	a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, http.StatusOK, nil)
	_ = writer.Close()
}

func openAIImageEditStreamEvent(eventType string, raw []byte, createdAt int64, size string, partialIndex int) map[string]any {
	value := map[string]any{
		"type": eventType, "b64_json": base64.StdEncoding.EncodeToString(raw),
		"created_at": createdAt, "size": size, "quality": "auto",
		"background": "auto", "output_format": imageOutputFormat(raw),
	}
	if eventType == "image_edit.partial_image" {
		value["partial_image_index"] = partialIndex
	} else {
		value["usage"] = map[string]any{
			"total_tokens": 0, "input_tokens": 0, "output_tokens": 0,
			"input_tokens_details": map[string]any{"text_tokens": 0, "image_tokens": 0},
		}
	}
	return value
}

func imageEditEventSize(size, aspectRatio string) string {
	switch value := strings.ToLower(strings.TrimSpace(size)); value {
	case "1024x1024", "1024x1536", "1536x1024", "auto":
		return value
	}
	switch strings.ToLower(strings.TrimSpace(aspectRatio)) {
	case "1:1":
		return "1024x1024"
	case "2:3":
		return "1024x1536"
	case "3:2":
		return "1536x1024"
	default:
		return "auto"
	}
}

func imageEditResultURLs(parsed *parsedChat, captured []byte) []string {
	values := append([]string(nil), parsed.Images...)
	if len(values) == 0 {
		values = extractCapturedImageURLs(captured)
	}
	if len(values) == 0 {
		values = extractMarkdownImages(parsed.Text.String())
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = absoluteAssetURL(value)
		if _, moderated := parsed.moderatedImages[value]; moderated || containsString(result, value) {
			continue
		}
		result = append(result, value)
	}
	return result
}
