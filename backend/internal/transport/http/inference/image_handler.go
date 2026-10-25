package inference

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

type imageGenerationRequest struct {
	Model          string          `json:"model"`
	Prompt         string          `json:"prompt"`
	Count          *int            `json:"n"`
	PartialImages  *int            `json:"partial_images"`
	Size           string          `json:"size"`
	AspectRatio    string          `json:"aspect_ratio"`
	Resolution     string          `json:"resolution"`
	Quality        string          `json:"quality"`
	ResponseFormat string          `json:"response_format"`
	OutputFormat   string          `json:"output_format"`
	Background     string          `json:"background"`
	StorageOptions json.RawMessage `json:"storage_options"`
	Stream         bool            `json:"stream"`
}

// imageEditJSONRequest is also filled from multipart fields; image, images,
// image_url and mask accept every form imageReferences understands.
type imageEditJSONRequest struct {
	Model          string          `json:"model"`
	Prompt         string          `json:"prompt"`
	Image          json.RawMessage `json:"image"`
	Images         json.RawMessage `json:"images"`
	ImageURL       json.RawMessage `json:"image_url"`
	Mask           json.RawMessage `json:"mask"`
	Count          *int            `json:"n"`
	Size           string          `json:"size"`
	AspectRatio    string          `json:"aspect_ratio"`
	Resolution     string          `json:"resolution"`
	Quality        string          `json:"quality"`
	ResponseFormat string          `json:"response_format"`
	OutputFormat   string          `json:"output_format"`
	Background     string          `json:"background"`
	StorageOptions json.RawMessage `json:"storage_options"`
	Stream         bool            `json:"stream"`
	PartialImages  *int            `json:"partial_images"`
}

func (h *Handler) generateImage(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxBodyBytes)
	if !isJSONRequest(c) {
		writeOpenAIError(c, http.StatusUnsupportedMediaType, "invalid_request", "图片生成仅支持 application/json")
		return
	}
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		writeOpenAIError(c, http.StatusRequestEntityTooLarge, "request_too_large", "请求体超过限制")
		return
	}
	var request imageGenerationRequest
	if decodeSingleJSON(bytes.NewReader(body), &request, false) != nil || strings.TrimSpace(request.Model) == "" || strings.TrimSpace(request.Prompt) == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "图片请求缺少有效 model 或 prompt")
		return
	}
	if value := bytes.TrimSpace(request.StorageOptions); len(value) > 0 && !bytes.Equal(value, []byte("null")) {
		writeOpenAIError(c, http.StatusBadRequest, "unsupported_parameter", "当前兼容层暂不支持 storage_options")
		return
	}
	count := 1
	if request.Count != nil {
		if *request.Count < 1 || *request.Count > 10 {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", "n 必须在 1 到 10 之间")
			return
		}
		count = *request.Count
	}
	if request.Stream && count != 1 {
		writeImageGenerationUserError(c, "unsupported_parameter", "input", "Streaming is only supported with n=1.")
		return
	}
	partialImages := 0
	if request.PartialImages != nil {
		if *request.PartialImages < 0 || *request.PartialImages > 3 {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", "partial_images 必须在 0 到 3 之间")
			return
		}
		partialImages = *request.PartialImages
		if partialImages > 0 && !request.Stream {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", "partial_images 仅可在 stream=true 时使用")
			return
		}
	}
	clientKey, requestID, ok := requestIdentity(c)
	if !ok {
		return
	}
	result, err := h.gateway.GenerateImage(c.Request.Context(), gateway.ImageGenerationInput{
		RequestID: requestID, ClientKey: clientKey, PublicModel: request.Model, Prompt: request.Prompt,
		Count: count, Size: request.Size, AspectRatio: request.AspectRatio,
		Resolution: request.Resolution, Quality: normalizeImageOption(request.Quality), ResponseFormat: request.ResponseFormat,
		OutputFormat: normalizeImageOption(request.OutputFormat), Background: normalizeImageOption(request.Background),
		Streaming: request.Stream, PartialImages: partialImages,
		Method: c.Request.Method, Path: c.Request.URL.Path, Headers: c.Request.Header.Clone(),
	})
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	h.writeResult(c, result, request.Stream, streamProtocolImage)
}

func (h *Handler) editImage(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxBodyBytes)
	request, imageURLs, maskURLs, ok := readImageEditRequest(c)
	if !ok {
		return
	}
	if value := bytes.TrimSpace(request.StorageOptions); len(value) > 0 && !bytes.Equal(value, []byte("null")) {
		writeOpenAIError(c, http.StatusBadRequest, "unsupported_parameter", "当前兼容层暂不支持 storage_options")
		return
	}
	model := strings.TrimSpace(request.Model)
	prompt := strings.TrimSpace(request.Prompt)
	count := 1
	if request.Count != nil {
		count = *request.Count
	}
	if len(imageURLs) == 0 || len(imageURLs) > 8 {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "image 或 images 数量必须在 1 到 8 之间")
		return
	}
	if len(maskURLs) > 1 {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "mask 最多只能提供一张")
		return
	}
	if model == "" || prompt == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "图片编辑缺少有效 model 或 prompt")
		return
	}
	if count < 1 || count > 10 {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", "n 必须在 1 到 10 之间")
		return
	}
	partialImages := 0
	if request.PartialImages != nil {
		if *request.PartialImages < 0 || *request.PartialImages > 3 {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", "partial_images 必须在 0 到 3 之间")
			return
		}
		partialImages = *request.PartialImages
		if partialImages > 0 && !request.Stream {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", "partial_images 仅可在 stream=true 时使用")
			return
		}
	}
	maskURL := ""
	if len(maskURLs) == 1 {
		maskURL = maskURLs[0]
	}
	clientKey, requestID, ok := requestIdentity(c)
	if !ok {
		return
	}
	result, err := h.gateway.EditImage(c.Request.Context(), gateway.ImageEditInput{
		RequestID: requestID, ClientKey: clientKey, PublicModel: model, Prompt: prompt,
		ImageURLs: imageURLs, MaskURL: maskURL, Count: count,
		Size: normalizeImageOption(request.Size), AspectRatio: normalizeImageOption(request.AspectRatio),
		Resolution: normalizeImageOption(request.Resolution), Quality: normalizeImageOption(request.Quality), ResponseFormat: request.ResponseFormat,
		OutputFormat: normalizeImageOption(request.OutputFormat), Background: normalizeImageOption(request.Background),
		Streaming: request.Stream, PartialImages: partialImages,
		Method: c.Request.Method, Path: c.Request.URL.Path, Headers: c.Request.Header.Clone(),
	})
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	h.writeResult(c, result, request.Stream, streamProtocolImage)
}

// normalizeImageOption trims and lowercases an enum-like image parameter;
// providers validate the values.
func normalizeImageOption(value string) string { return strings.ToLower(strings.TrimSpace(value)) }

func writeImageGenerationUserError(c *gin.Context, code, param, message string) {
	middleware.NoteError(c, code, message)
	c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{
		"message": message, "type": "image_generation_user_error", "param": param, "code": code,
	}})
}
