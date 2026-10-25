package inference

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/gin-gonic/gin"
)

type imageGenerationRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	Count          *int   `json:"n"`
	PartialImages  *int   `json:"partial_images"`
	Size           string `json:"size"`
	AspectRatio    string `json:"aspect_ratio"`
	Resolution     string `json:"resolution"`
	Quality        string `json:"quality"`
	ResponseFormat string `json:"response_format"`
	OutputFormat   string `json:"output_format"`
	Background     string `json:"background"`
	Stream         bool   `json:"stream"`
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
	count, partialImages := imageCounts(request.Count, request.PartialImages, request.Stream)
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
	model := strings.TrimSpace(request.Model)
	prompt := strings.TrimSpace(request.Prompt)
	if len(imageURLs) == 0 {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "图片编辑需要至少一张 image")
		return
	}
	// Images beyond the 8 an edit takes, and masks after the first, are dropped.
	imageURLs, maskURLs = imageURLs[:min(len(imageURLs), maxEditImages)], maskURLs[:min(len(maskURLs), 1)]
	if model == "" || prompt == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "图片编辑缺少有效 model 或 prompt")
		return
	}
	count, partialImages := imageCounts(request.Count, request.PartialImages, request.Stream)
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

// maxEditImages is the most source images an edit takes.
const maxEditImages = 8

// imageCounts maps n and partial_images into the ranges the routes take
// instead of refusing them: n to 1–10 (1 when streaming), partial images to
// 0–3 and only when streaming.
func imageCounts(n, partial *int, streaming bool) (int, int) {
	count, partialImages := 1, 0
	if n != nil {
		count = min(max(*n, 1), 10)
	}
	if streaming {
		count = 1
		if partial != nil {
			partialImages = min(max(*partial, 0), 3)
		}
	}
	return count, partialImages
}

// normalizeImageOption trims and lowercases an enum-like image parameter;
// providers validate the values.
func normalizeImageOption(value string) string { return strings.ToLower(strings.TrimSpace(value)) }
