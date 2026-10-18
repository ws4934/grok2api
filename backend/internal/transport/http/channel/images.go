package channelhttp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"strconv"
	"strings"

	domain "github.com/chenyme/grok2api/backend/internal/domain/channel"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	mediahttp "github.com/chenyme/grok2api/backend/internal/transport/http/media"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

type imageWire struct {
	Model          string            `json:"model"`
	Prompt         string            `json:"prompt"`
	N              int               `json:"n"`
	Size           string            `json:"size"`
	Quality        string            `json:"quality"`
	ResponseFormat string            `json:"response_format"`
	OutputFormat   string            `json:"output_format"`
	Background     string            `json:"background"`
	Images         []json.RawMessage `json:"images"`
	Image          json.RawMessage   `json:"image"`
	Mask           json.RawMessage   `json:"mask"`
}

// RegisterPublic adds the one Responses route the legacy router lacks.
// GET /v1/responses/{id} is served through Dispatch because the legacy route owns it.
func (h *Handler) RegisterPublic(r *gin.RouterGroup) {
	r.POST("/responses/:responseId/cancel", h.cancelImageResponse)
}
func keyID(c *gin.Context) (clientkey.Key, bool) {
	v, _ := c.Get(middleware.ClientKey)
	key, ok := v.(clientkey.Key)
	return key, ok && key.ID > 0
}
func (h *Handler) adminImageJobs(c *gin.Context) {
	offset, _ := strconv.Atoi(c.Query("offset"))
	limit, _ := strconv.Atoi(c.Query("limit"))
	v, total, e := h.service.ImageJobs(c.Request.Context(), 0, provider(c), offset, limit)
	reply(c, gin.H{"items": v, "total": total}, e)
}

// readImageRef accepts the OpenAI image reference forms: a data URI, an https
// URL (fetched with the shared SSRF guard) or {"url": ...}.
func readImageRef(c *gin.Context, raw json.RawMessage) ([]byte, error) {
	var u string
	if json.Unmarshal(raw, &u) != nil {
		var o struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(raw, &o) != nil || o.URL == "" {
			return nil, errors.New("expected image URL or data URI")
		}
		u = o.URL
	}
	return readImageURL(c, u)
}
func readImageURL(c *gin.Context, u string) ([]byte, error) {
	if strings.HasPrefix(u, "data:") {
		head, data, ok := strings.Cut(u, ",")
		if !ok || !strings.HasSuffix(head, ";base64") || len(data) > 28<<20 {
			return nil, errors.New("invalid data URI")
		}
		return base64.StdEncoding.DecodeString(data)
	}
	return mediahttp.FetchRemoteImage(c.Request.Context(), u)
}
func (h *Handler) saveImage(c *gin.Context, data []byte, name string) (domain.ImageInput, error) {
	if len(data) > 20<<20 {
		return domain.ImageInput{}, errors.New("image too large")
	}
	return h.service.SaveImageInput(c.Request.Context(), domain.Blob{Name: name, Data: data})
}
func imageRejected(c *gin.Context, e error) {
	status := 400
	if errors.Is(e, domain.ErrConflict) {
		status = 409
	}
	publicError(c, status, "image_job_rejected", "Image request rejected; check permissions, model, price and input requirements")
}

// imageRequest serves the synchronous OpenAI Images API for extension routes.
// It returns false, leaving the request untouched, when no extension route matches.
func (h *Handler) imageRequest(c *gin.Context) bool {
	var v imageWire
	var files []*multipart.FileHeader
	var maskFile *multipart.FileHeader
	isMultipart := strings.HasPrefix(c.GetHeader("Content-Type"), "multipart/form-data")
	if isMultipart {
		if e := c.Request.ParseMultipartForm(20 << 20); e != nil {
			publicError(c, 400, "invalid_image_input", "Invalid multipart request")
			return true
		}
		v.Model = c.PostForm("model")
		v.Prompt = c.PostForm("prompt")
		v.Size = c.PostForm("size")
		v.Quality = c.PostForm("quality")
		v.ResponseFormat = c.PostForm("response_format")
		v.OutputFormat = c.PostForm("output_format")
		v.Background = c.PostForm("background")
		files = append(files, c.Request.MultipartForm.File["image"]...)
		files = append(files, c.Request.MultipartForm.File["image[]"]...)
		files = append(files, c.Request.MultipartForm.File["images"]...)
		masks := c.Request.MultipartForm.File["mask"]
		if len(masks) > 1 {
			publicError(c, 400, "invalid_mask", "Only one mask is supported")
			return true
		}
		if len(masks) == 1 {
			maskFile = masks[0]
		}
	} else {
		data, e := io.ReadAll(io.LimitReader(c.Request.Body, 32<<20+1))
		if e != nil || len(data) > 32<<20 {
			publicError(c, 413, "request_too_large", "Request exceeds body limit")
			return true
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(data))
		if json.Unmarshal(data, &v) != nil {
			return false
		}
	}
	operation := "image"
	if strings.HasSuffix(c.Request.URL.Path, "/edits") {
		operation = "image_edit"
	}
	route, matched, e := h.service.Resolve(c.Request.Context(), v.Model, operation)
	if e != nil {
		publicError(c, 503, "channel_unavailable", "Channel lookup failed")
		return true
	}
	if !matched {
		return false
	}
	c.Abort()
	if isMultipart {
		defer c.Request.MultipartForm.RemoveAll()
		allowed := map[string]bool{"model": true, "prompt": true, "n": true, "size": true, "quality": true, "response_format": true, "output_format": true, "background": true}
		for name, values := range c.Request.MultipartForm.Value {
			if !allowed[name] || len(values) != 1 {
				publicError(c, 400, "unsupported_parameter", "Unsupported or repeated multipart field")
				return true
			}
		}
		for name := range c.Request.MultipartForm.File {
			if name != "image" && name != "image[]" && name != "images" && name != "mask" {
				publicError(c, 400, "unsupported_parameter", "Unsupported multipart file field")
				return true
			}
		}
		if raw := c.PostForm("n"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 || n > 10 {
				publicError(c, 400, "invalid_n", "n must be 1–10")
				return true
			}
			v.N = n
		}
	} else {
		data, _ := io.ReadAll(c.Request.Body)
		if strictJSON(data, &v) != nil {
			publicError(c, 400, "unsupported_parameter", "Unsupported image parameter")
			return true
		}
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(data, &fields)
		if _, exists := fields["n"]; exists && (v.N < 1 || v.N > 10) {
			publicError(c, 400, "invalid_n", "n must be 1–10")
			return true
		}
	}
	if v.ResponseFormat == "" {
		v.ResponseFormat = "url"
	}
	if v.ResponseFormat != "url" && v.ResponseFormat != "b64_json" {
		publicError(c, 400, "invalid_response_format", "response_format must be url or b64_json")
		return true
	}
	key, ok := keyID(c)
	if !ok {
		publicError(c, 401, "invalid_api_key", "API key required")
		return true
	}
	if e := h.service.CheckAccess(c.Request.Context(), key, route); e != nil {
		publicError(c, 404, "model_not_found", "Model unavailable")
		return true
	}
	request := domain.ImageRequest{Operation: operation, Model: v.Model, Prompt: v.Prompt, N: v.N, Size: v.Size, Quality: v.Quality, OutputFormat: v.OutputFormat, Background: v.Background}
	if len(v.Image) > 0 && string(v.Image) != "null" {
		v.Images = append(v.Images, v.Image)
	}
	if len(v.Images)+len(files) > 8 {
		publicError(c, 400, "too_many_images", "At most 8 reference images are supported")
		return true
	}
	readFile := func(f *multipart.FileHeader) ([]byte, error) {
		if f.Size > 20<<20 {
			return nil, errors.New("image too large")
		}
		r, e := f.Open()
		if e != nil {
			return nil, e
		}
		defer r.Close()
		return io.ReadAll(io.LimitReader(r, 20<<20+1))
	}
	for _, raw := range v.Images {
		data, e := readImageRef(c, raw)
		if e != nil {
			publicError(c, 400, "invalid_image", "Could not read reference image")
			return true
		}
		input, e := h.saveImage(c, data, "image")
		if e != nil {
			publicError(c, 400, "invalid_image", "Could not store reference image")
			return true
		}
		request.Inputs = append(request.Inputs, input)
	}
	for _, file := range files {
		data, e := readFile(file)
		if e != nil {
			publicError(c, 400, "invalid_image", "Could not read uploaded image")
			return true
		}
		input, e := h.saveImage(c, data, file.Filename)
		if e != nil {
			publicError(c, 400, "invalid_image", "Could not store uploaded image")
			return true
		}
		request.Inputs = append(request.Inputs, input)
	}
	if len(v.Mask) > 0 || maskFile != nil {
		var data []byte
		var e error
		if maskFile != nil {
			data, e = readFile(maskFile)
		} else {
			data, e = readImageRef(c, v.Mask)
		}
		if e != nil {
			publicError(c, 400, "invalid_mask", "Could not read mask")
			return true
		}
		input, e := h.saveImage(c, data, "mask.png")
		if e != nil {
			publicError(c, 400, "invalid_mask", "Could not store mask")
			return true
		}
		request.Mask = &input
	}
	job, e := h.service.SubmitImages(c.Request.Context(), key, route, request, c.GetHeader("Idempotency-Key"))
	if e != nil {
		imageRejected(c, e)
		return true
	}
	// The job stays readable as a Responses object after a client disconnect.
	c.Header("X-Image-Job-ID", job.ID)
	c.Header("Location", "/v1/responses/"+job.ID)
	job, e = h.service.WaitImageJob(c.Request.Context(), key.ID, job.ID)
	if e != nil {
		return true
	}
	out, e := h.service.ImageResults(c.Request.Context(), key.ID, job.ID, v.ResponseFormat)
	if e != nil || len(out) == 0 {
		publicError(c, 502, "image_generation_failed", "Image task did not deliver a result; inspect X-Image-Job-ID")
		return true
	}
	c.JSON(200, gin.H{"created": job.CreatedAt.Unix(), "data": out, "job_id": job.ID, "status": job.Status})
	return true
}
