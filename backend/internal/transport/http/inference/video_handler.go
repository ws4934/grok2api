package inference

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/pkg/mediafile"
	"github.com/gin-gonic/gin"
)

type videoGenerationImage struct {
	URL    string `json:"url"`
	FileID string `json:"file_id"`
}

// UnmarshalJSON also reads a bare URL and the {"image_url": …} form other
// video APIs use.
func (v *videoGenerationImage) UnmarshalJSON(data []byte) error {
	if json.Unmarshal(data, &v.URL) == nil {
		return nil
	}
	var raw struct {
		URL      string          `json:"url"`
		FileID   string          `json:"file_id"`
		ImageURL json.RawMessage `json:"image_url"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	v.URL, v.FileID = raw.URL, raw.FileID
	if v.URL == "" && len(raw.ImageURL) > 0 {
		var nested videoGenerationImage
		if err := json.Unmarshal(raw.ImageURL, &nested); err == nil {
			v.URL = nested.URL
		}
	}
	return nil
}

type videoGenerationAudio struct {
	VoiceID string `json:"voice_id"`
}

type videoGenerationRequest struct {
	Model           string                 `json:"model"`
	Prompt          string                 `json:"prompt"`
	User            *string                `json:"user"`
	Duration        json.RawMessage        `json:"duration"`
	AspectRatio     string                 `json:"aspect_ratio"`
	Resolution      string                 `json:"resolution"`
	Image           *videoGenerationImage  `json:"image"`
	LastFrame       *videoGenerationImage  `json:"last_frame"`
	ReferenceImages []videoGenerationImage `json:"reference_images"`
	ReferenceAudios []videoGenerationAudio `json:"reference_audios"`
	ReferenceVideos []videoGenerationImage `json:"reference_videos"`
	Video           *videoGenerationImage  `json:"video"`
	// Other video APIs' names, mapped onto the xAI fields: seconds (duration),
	// size (aspect ratio and resolution), quality (resolution) and
	// input_reference (first frame).
	Seconds        json.RawMessage       `json:"seconds"`
	Size           string                `json:"size"`
	Quality        string                `json:"quality"`
	InputReference *videoGenerationImage `json:"input_reference"`
}

// adapted maps other video APIs' field names onto the xAI ones the client
// left empty.
func (r videoGenerationRequest) adapted() videoGenerationRequest {
	if !hasJSONValue(r.Duration) {
		r.Duration = r.Seconds
	}
	if strings.TrimSpace(r.AspectRatio) == "" {
		r.AspectRatio = r.Size
	}
	if strings.TrimSpace(r.Resolution) == "" {
		if w, h := provider.ImagePair(r.Size); w > 0 && strings.ContainsAny(strings.ToLower(r.Size), "x×*") {
			r.Resolution = fmt.Sprintf("%dp", int(min(w, h)))
		} else {
			r.Resolution = r.Quality
		}
	}
	if r.Image == nil {
		r.Image = r.InputReference
	}
	return r
}

func (h *Handler) generateVideo(c *gin.Context) {
	h.handleVideoCreate(c, gatewayVideoOperationGenerate, "视频生成")
}

func (h *Handler) editVideo(c *gin.Context) {
	h.handleVideoCreate(c, gatewayVideoOperationEdit, "视频编辑")
}

func (h *Handler) extendVideo(c *gin.Context) {
	h.handleVideoCreate(c, gatewayVideoOperationExtend, "视频延长")
}

const (
	gatewayVideoOperationGenerate = "generate"
	gatewayVideoOperationEdit     = "edit"
	gatewayVideoOperationExtend   = "extend"
)

func (h *Handler) handleVideoCreate(c *gin.Context, operation, label string) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxBodyBytes)
	if !isJSONRequest(c) {
		writeOpenAIError(c, http.StatusUnsupportedMediaType, "invalid_request", label+"仅支持 application/json")
		return
	}
	// Fields the gateway does not use (output.upload_url, storage_options,
	// seeds, watermarks…) are ignored, not refused.
	var request videoGenerationRequest
	if err := decodeSingleJSON(c.Request.Body, &request, false); err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", label+" JSON 请求无效: "+err.Error())
		return
	}
	request = request.adapted()
	model := strings.TrimSpace(request.Model)
	prompt := strings.TrimSpace(request.Prompt)
	if model == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", label+"缺少有效 model")
		return
	}
	parseVideoImage := func(input videoGenerationImage, field string) (string, bool) {
		urlValue := strings.TrimSpace(input.URL)
		fileID := strings.TrimSpace(input.FileID)
		if (urlValue == "") == (fileID == "") {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", field+" 必须且只能提供 url 或 file_id")
			return "", false
		}
		if fileID != "" {
			if !mediadomain.IsInputAssetID(fileID) {
				writeOpenAIError(c, http.StatusBadRequest, "invalid_request", field+".file_id 无效")
				return "", false
			}
			return gateway.VideoInputFileReference(fileID), true
		}
		return urlValue, true
	}

	imageURL := ""
	lastFrameURL := ""
	referenceURLs := []string{}
	referenceAudios := []string{}
	referenceVideoURLs := []string{}
	videoURL := ""

	// 写法认不出的时长、比例、分辨率按没传处理：取值范围和默认值由路由的 Provider
	// 按模型和操作决定，Provider 把其他值对齐到最接近的可用值。
	duration, err := parseVideoDuration(request.Duration)
	if err != nil {
		duration = 0
	}
	aspectRatio := strings.TrimSpace(request.AspectRatio)
	if strings.EqualFold(aspectRatio, "auto") {
		aspectRatio = "auto"
	} else if w, _ := provider.ImagePair(aspectRatio); w == 0 {
		aspectRatio = ""
	}
	resolution := strings.ToLower(strings.TrimSpace(request.Resolution))
	if _, err := strconv.Atoi(resolution); err == nil {
		resolution += "p" // a bare "720" means 720p
	}
	if provider.ImageTierPixels(resolution) == 0 {
		resolution = ""
	}

	if operation == gatewayVideoOperationGenerate {
		if request.Image != nil {
			value, ok := parseVideoImage(*request.Image, "image")
			if !ok {
				return
			}
			imageURL = value
		}
		if request.LastFrame != nil {
			if request.Image == nil {
				writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "last_frame（尾帧）必须同时提供 image（首帧）")
				return
			}
			value, ok := parseVideoImage(*request.LastFrame, "last_frame")
			if !ok {
				return
			}
			lastFrameURL = value
		}
		// Inputs beyond the gateway's limits are dropped, the first ones kept;
		// the route's provider then keeps what its upstream takes.
		request.ReferenceImages = request.ReferenceImages[:min(len(request.ReferenceImages), mediadomain.MaxInputImages)]
		request.ReferenceAudios = request.ReferenceAudios[:min(len(request.ReferenceAudios), maxVideoReferenceAudios)]
		request.ReferenceVideos = request.ReferenceVideos[:min(len(request.ReferenceVideos), mediadomain.MaxReferenceVideos)]
		referenceURLs = make([]string, 0, len(request.ReferenceImages))
		for _, input := range request.ReferenceImages {
			value, ok := parseVideoImage(input, "reference_images")
			if !ok {
				return
			}
			referenceURLs = append(referenceURLs, value)
		}
		referenceAudios = make([]string, 0, len(request.ReferenceAudios))
		for i, input := range request.ReferenceAudios {
			voiceID := strings.TrimSpace(input.VoiceID)
			if voiceID == "" {
				writeOpenAIError(c, http.StatusBadRequest, "invalid_request", fmt.Sprintf("reference_audios[%d].voice_id 不能为空", i))
				return
			}
			referenceAudios = append(referenceAudios, voiceID)
		}
		for _, input := range request.ReferenceVideos {
			value, ok := parseVideoImage(input, "reference_videos")
			if !ok {
				return
			}
			referenceVideoURLs = append(referenceVideoURLs, value)
		}
		if len(referenceVideoURLs) > 0 && prompt == "" {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "参考视频生成必须提供 prompt")
			return
		}
		hasReferenceMode := len(referenceURLs) > 0 || len(referenceAudios) > 0
		if hasReferenceMode && prompt == "" {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "参考图/参考音频视频必须提供 prompt")
			return
		}
		if prompt == "" && imageURL == "" && !hasReferenceMode {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "文本生视频必须提供 prompt；图片生视频可以省略 prompt")
			return
		}
		if request.Video != nil {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "视频生成不支持 video 输入")
			return
		}
	} else {
		if prompt == "" {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", label+"必须提供 prompt")
			return
		}
		if request.Video == nil {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", label+"必须提供 video")
			return
		}
		value, ok := parseVideoImage(*request.Video, "video")
		if !ok {
			return
		}
		videoURL = value
		if request.Image != nil || request.LastFrame != nil || len(request.ReferenceImages) > 0 || len(request.ReferenceAudios) > 0 || len(request.ReferenceVideos) > 0 {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", label+"不支持 image、last_frame、reference_images、reference_videos 或 reference_audios")
			return
		}
	}

	clientKey, requestID, ok := requestIdentity(c)
	if !ok {
		return
	}
	var op provider.VideoOperation
	switch operation {
	case gatewayVideoOperationEdit:
		op = provider.VideoOperationEdit
	case gatewayVideoOperationExtend:
		op = provider.VideoOperationExtend
	default:
		op = provider.VideoOperationGenerate
	}
	job, err := h.gateway.CreateVideo(c.Request.Context(), gateway.VideoInput{
		RequestID: requestID, ClientKey: clientKey, PublicModel: model,
		Operation: op,
		Prompt:    prompt, Duration: duration, AspectRatio: aspectRatio, Resolution: resolution,
		ImageURL: imageURL, LastFrameURL: lastFrameURL, ReferenceURLs: referenceURLs, ReferenceVideoURLs: referenceVideoURLs,
		ReferenceAudios: referenceAudios, VideoURL: videoURL,
	})
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"request_id": job.ID})
}

func (h *Handler) getVideo(c *gin.Context) {
	clientKey, _, ok := requestIdentity(c)
	if !ok {
		return
	}
	job, err := h.gateway.GetVideo(c.Request.Context(), strings.TrimSpace(c.Param("requestId")), clientKey)
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	c.JSON(http.StatusOK, videoGenerationResponse(job, h.videoPlaybackURL(job)))
}

func (h *Handler) publicURL(path string) string {
	baseURL := h.publicAPIBaseURL
	if h.publicBaseURL != nil {
		baseURL = strings.TrimRight(strings.TrimSpace(h.publicBaseURL()), "/")
	}
	if baseURL == "" {
		return path
	}
	return baseURL + path
}

func (h *Handler) videoContentURL(jobID string) string {
	return h.publicURL("/v1/videos/" + url.PathEscape(jobID) + "/content")
}

// videoPlaybackURL prefers the stored asset served by the public media route, so the
// returned link opens directly in browsers and players. /v1/videos/{id}/content needs
// the client API key, which makes the URL unusable outside an authenticated client.
// Images already return their public media URL; this keeps video consistent. Jobs
// without a stored asset keep the protected content endpoint.
func (h *Handler) videoPlaybackURL(job mediadomain.Job) string {
	if assetID := strings.TrimSpace(job.ResultAssetID); assetID != "" {
		return h.publicURL("/v1/media/videos/" + url.PathEscape(assetID))
	}
	return h.videoContentURL(job.ID)
}

func (h *Handler) getVideoContent(c *gin.Context) {
	clientKey, _, ok := requestIdentity(c)
	if !ok {
		return
	}
	body, contentType, size, err := h.gateway.OpenVideoContent(c.Request.Context(), strings.TrimSpace(c.Param("requestId")), clientKey)
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	defer func() { _ = body.Close() }()
	writeVideoContent(c, body, contentType, size, strings.TrimSpace(c.Param("requestId")))
}

func writeVideoContent(c *gin.Context, body io.Reader, contentType string, size int64, downloadName string) {
	if size > maxMediaResponseTransferBytes {
		writeOpenAIError(c, http.StatusBadGateway, "media_too_large", "上游媒体超过 2 GiB 安全上限")
		return
	}
	contentType, ok := normalizeVideoResponseContentType(contentType)
	if !ok {
		writeOpenAIError(c, http.StatusBadGateway, "invalid_media_type", "上游视频服务返回了不受支持的内容类型")
		return
	}
	// Clients that save the response need an extension to get a playable file.
	c.Header("Content-Disposition", mediafile.VideoContentDisposition(downloadName, contentType))
	c.Header("Cache-Control", "private, no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Security-Policy", "default-src 'none'; sandbox")
	c.Header("Referrer-Policy", "no-referrer")
	if size >= 0 {
		c.Header("Content-Length", strconv.FormatInt(size, 10))
	} else {
		c.Header("Trailer", mediaTransferErrorTrailer)
	}
	if err := writeMediaBody(c, body, contentType, http.StatusOK, maxMediaResponseTransferBytes); err != nil && size < 0 {
		errorCode := "stream_interrupted"
		if errors.Is(err, errResponseTransferLimit) {
			errorCode = "response_too_large"
		}
		c.Header(mediaTransferErrorTrailer, errorCode)
	}
}

func normalizeVideoResponseContentType(value string) (string, bool) {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(value))
	if err != nil {
		return "", false
	}
	switch strings.ToLower(mediaType) {
	case "video/mp4", "video/quicktime", "video/webm":
		return strings.ToLower(mediaType), true
	default:
		return "", false
	}
}

// maxVideoReferenceAudios is the most reference voices the xAI video API takes.
const maxVideoReferenceAudios = 3

// parseVideoDuration 解析可选的 duration；没传时返回 0，由路由的 Provider 取模型默认值。
func parseVideoDuration(durationRaw json.RawMessage) (int, error) {
	duration, hasDuration, err := parseOptionalVideoInteger(durationRaw)
	if err != nil {
		return 0, fmt.Errorf("duration 必须是整数或整数字符串")
	}
	if hasDuration && duration < 1 {
		return 0, fmt.Errorf("duration 必须是正整数")
	}
	return duration, nil
}

func parseOptionalVideoInteger(raw json.RawMessage) (int, bool, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, false, nil
	}
	var number int
	if json.Unmarshal(raw, &number) != nil {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return 0, true, errors.New("必须是整数或整数字符串")
		}
		parsed, err := strconv.Atoi(strings.TrimSpace(text))
		if err != nil {
			return 0, true, errors.New("必须是整数或整数字符串")
		}
		number = parsed
	}
	return number, true, nil
}

func hasJSONValue(value json.RawMessage) bool {
	trimmed := bytes.TrimSpace(value)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

func videoGenerationResponse(job mediadomain.Job, contentURLs ...string) gin.H {
	switch job.Status {
	case mediadomain.StatusCompleted:
		videoURL := job.UpstreamURL
		if len(contentURLs) > 0 && contentURLs[0] != "" {
			videoURL = contentURLs[0]
		}
		video := gin.H{"url": videoURL, "respect_moderation": true}
		operation := job.Operation
		if operation == "" {
			operation = mediadomain.VideoOperationGenerate
		}
		if operation == mediadomain.VideoOperationGenerate && job.Seconds > 0 {
			video["duration"] = job.Seconds
		}
		// video_url repeats video.url for clients that read a top-level link,
		// such as the canvas xAI Video plugin.
		return gin.H{
			"status": "done", "model": job.Model, "progress": 100,
			"video": video, "video_url": videoURL,
		}
	case mediadomain.StatusFailed, mediadomain.StatusUncertain, mediadomain.StatusCanceled:
		return gin.H{
			"status": "failed",
			"error":  gin.H{"code": officialVideoErrorCode(job.ErrorCode), "message": job.ErrorMessage},
		}
	default:
		return gin.H{"status": "pending", "model": job.Model, "progress": min(99, max(0, job.Progress))}
	}
}

func officialVideoErrorCode(value string) string {
	switch value {
	case "account_unavailable", "provider_unavailable":
		return "service_unavailable"
	case "model_not_found":
		return "invalid_argument"
	default:
		return "internal_error"
	}
}
