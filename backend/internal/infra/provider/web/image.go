package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

const (
	maxGeneratedImages            = 10
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
	ratio := resolveImageAspectRatio(request.AspectRatio, request.Size)
	if spec.ImaginePro {
		ratio = resolveImage20AspectRatio(request.AspectRatio, request.Size)
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

// webImageRatios are the aspect ratios Grok Imagine draws.
var webImageRatios = []string{"1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3", "2:1", "1:2", "19.5:9", "9:19.5", "20:9", "9:20"}

// Current Web Quality 2.0 ratios, observed in the official v2 toolbar. Other
// Web products retain their established ratio mapping.
var webImage20Ratios = []string{"1:1", "16:9", "9:16", "4:3", "3:2", "2:3", "21:9", "5:2"}

// resolveImageAspectRatio reads aspect_ratio, else size, as the closest ratio
// Grok Imagine draws; "auto" without a shape. The OpenAI sizes keep their
// established ratios.
func resolveImageAspectRatio(aspectRatio, size string) string {
	return resolveImageRatio(aspectRatio, size, webImageRatios)
}

func resolveImage20AspectRatio(aspectRatio, size string) string {
	return resolveImageRatio(aspectRatio, size, webImage20Ratios)
}

func resolveImageRatio(aspectRatio, size string, ratios []string) string {
	value := strings.ToLower(strings.TrimSpace(aspectRatio))
	if value == "" {
		value = strings.ToLower(strings.TrimSpace(size))
	}
	aliases := map[string]string{"1792x1024": "3:2", "1024x1792": "2:3"}
	if ratio := aliases[value]; ratio != "" {
		return ratio
	}
	if ratio := provider.ClosestImageRatio(value, ratios); ratio != "" {
		return ratio
	}
	return "auto"
}

func resolveAspectRatio(size string) string {
	if value := resolveImageAspectRatio("", size); value != "auto" {
		return value
	}
	return "1:1"
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
