package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/grokimage"
)

const (
	maxGeneratedImages            = 10
	mediaOutputAttempts           = 3
	imageDownloadTimeout          = 60 * time.Second
	imagineSelfUploadSource       = "IMAGINE_SELF_UPLOAD_FILE_SOURCE"
	directFileUploadResponseLimit = 2 << 20
)

func invalidImageRequest(message string) (*provider.Response, error) {
	return jsonProviderResponse(http.StatusBadRequest, map[string]any{"error": map[string]any{
		"message": message, "type": "invalid_request_error",
	}}), nil
}

func imageGenerationUserError(message, param, code string) (*provider.Response, error) {
	return jsonProviderResponse(http.StatusBadRequest, map[string]any{"error": map[string]any{
		"message": message, "type": "image_generation_user_error", "param": param, "code": code,
	}}), nil
}

func firstString(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if result, _ := value[key].(string); result != "" {
			return result
		}
	}
	return ""
}

func firstInt(value map[string]any, keys ...string) (int, bool) {
	for _, key := range keys {
		if result, ok := numberAsInt(value[key]); ok {
			return result, true
		}
	}
	return 0, false
}

func numberAsInt(value any) (int, bool) {
	switch number := value.(type) {
	case float64:
		return int(number), true
	case int:
		return number, true
	case json.Number:
		parsed, err := number.Int64()
		return int(parsed), err == nil
	default:
		return 0, false
	}
}

var _ provider.ImageRequestValidator = (*Adapter)(nil)

func (a *Adapter) ValidateImageRequest(options provider.ImageRequestOptions) error {
	return grokimage.Validate(options)
}

func (a *Adapter) GenerateImage(ctx context.Context, request provider.ImageGenerationRequest) (*provider.Response, error) {
	count := request.Count
	if count <= 0 {
		count = 1
	}
	if request.Streaming && count != 1 {
		return imageGenerationUserError("Streaming is only supported with n=1.", "input", "unsupported_parameter")
	}
	if request.PartialImages < 0 || request.PartialImages > 3 {
		return invalidImageRequest("partial_images 必须在 0 到 3 之间")
	}
	if request.PartialImages > 0 && !request.Streaming {
		return invalidImageRequest("partial_images 仅可在 stream=true 时使用")
	}
	format := strings.ToLower(strings.TrimSpace(request.ResponseFormat))
	if format == "" {
		format = "url"
	}
	if format != "url" && format != "b64_json" {
		return jsonProviderResponse(http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "response_format 必须是 url 或 b64_json", "type": "invalid_request_error"}}), nil
	}
	spec, modelKnown := Resolve(request.Model)
	if !modelKnown || spec.Capability != "image" {
		return invalidImageRequest("模型不支持图片生成")
	}
	protocolModel := spec.ProtocolModel
	if protocolModel == "" {
		protocolModel = spec.UpstreamModel
	}
	if protocolModel == "imagine-lite" {
		if request.Streaming {
			return invalidImageRequest("grok-imagine-image-lite 不支持 stream")
		}
		if count > maxGeneratedImages {
			return invalidImageRequest("n 不能超过 10")
		}
		return a.generateLiteImage(ctx, request, count, format)
	}
	ratio, err := resolveImageAspectRatio(request.AspectRatio, request.Size)
	if err != nil {
		return invalidImageRequest(err.Error())
	}
	modelConfig, ok := resolveImagineModel(protocolModel, spec.ImaginePro, count)
	if !ok {
		return invalidImageRequest("模型不支持图片生成")
	}
	if count > modelConfig.MaxReturnCount {
		return invalidImageRequest(fmt.Sprintf("n 不能超过 %d", modelConfig.MaxReturnCount))
	}
	return a.generateWSImage(ctx, request, count, format, ratio, modelConfig)
}

func resolveImageAspectRatio(aspectRatio, size string) (string, error) {
	values := map[string]string{
		"auto": "auto", "1:1": "1:1", "16:9": "16:9", "9:16": "9:16", "4:3": "4:3", "3:4": "3:4",
		"3:2": "3:2", "2:3": "2:3", "2:1": "2:1", "1:2": "1:2", "19.5:9": "19.5:9", "9:19.5": "9:19.5", "20:9": "20:9", "9:20": "9:20",
		"1280x720": "16:9", "720x1280": "9:16", "1792x1024": "3:2", "1536x1024": "3:2", "1024x1792": "2:3", "1024x1536": "2:3", "1024x1024": "1:1",
	}
	value := strings.ToLower(strings.TrimSpace(aspectRatio))
	if value == "" {
		value = strings.ToLower(strings.TrimSpace(size))
	}
	if value == "" {
		return "auto", nil
	}
	if resolved := values[value]; resolved != "" {
		return resolved, nil
	}
	return "", fmt.Errorf("aspect_ratio 不受支持")
}

func resolveAspectRatio(size string) string {
	if strings.TrimSpace(size) == "" {
		return "1:1"
	}
	value, err := resolveImageAspectRatio("", size)
	if err != nil {
		return "1:1"
	}
	return value
}

func absoluteAssetURL(value string) string {
	if strings.HasPrefix(value, "https://") {
		return value
	}
	return "https://assets.grok.com/" + strings.TrimPrefix(value, "/")
}

func extractMarkdownImages(value string) []string {
	results := make([]string, 0, 2)
	for {
		start := strings.Index(value, "![image](")
		if start < 0 {
			break
		}
		value = value[start+len("![image]("):]
		end := strings.IndexByte(value, ')')
		if end < 0 {
			break
		}
		results = append(results, value[:end])
		value = value[end+1:]
	}
	return results
}
