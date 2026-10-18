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
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/channel"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	mediahttp "github.com/chenyme/grok2api/backend/internal/transport/http/media"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

type imageWire struct {
	Operation      string            `json:"operation"`
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

func (h *Handler) RegisterPublic(r *gin.RouterGroup) {
	r.POST("/images/jobs", h.createImageJob)
	r.GET("/images/jobs", h.listImageJobs)
	r.GET("/images/jobs/:job", h.getImageJob)
	r.GET("/images/jobs/:job/result", h.getImageResult)
	r.POST("/images/jobs/:job/cancel", h.cancelImageJob)
}
func keyID(c *gin.Context) (clientkey.Key, bool) {
	v, _ := c.Get(middleware.ClientKey)
	key, ok := v.(clientkey.Key)
	return key, ok && key.ID > 0
}
func (h *Handler) listImageJobs(c *gin.Context) {
	key, ok := keyID(c)
	if !ok {
		publicError(c, 401, "invalid_api_key", "API key required")
		return
	}
	offset, _ := strconv.Atoi(c.Query("offset"))
	limit, _ := strconv.Atoi(c.Query("limit"))
	v, total, e := h.service.ImageJobs(c.Request.Context(), key.ID, "", offset, limit)
	if e != nil {
		publicError(c, 503, "jobs_unavailable", "Job lookup failed")
		return
	}
	c.JSON(200, gin.H{"object": "list", "data": v, "total": total})
}
func (h *Handler) getImageJob(c *gin.Context) {
	key, ok := keyID(c)
	if !ok {
		return
	}
	v, e := h.service.GetImageJob(c.Request.Context(), key.ID, c.Param("job"))
	if e != nil {
		publicError(c, 404, "job_not_found", "Job not found")
		return
	}
	c.JSON(200, v)
}
func (h *Handler) getImageResult(c *gin.Context) {
	key, ok := keyID(c)
	if !ok {
		return
	}
	v, e := h.service.ImageResults(c.Request.Context(), key.ID, c.Param("job"), c.DefaultQuery("response_format", "url"))
	if e != nil {
		status := 404
		if errors.Is(e, domain.ErrConflict) {
			status = 409
		}
		publicError(c, status, "job_result_unavailable", "Job result unavailable")
		return
	}
	c.JSON(200, gin.H{"created": time.Now().Unix(), "data": v})
}
func (h *Handler) cancelImageJob(c *gin.Context) {
	key, ok := keyID(c)
	if !ok {
		return
	}
	e := h.service.CancelImageJob(c.Request.Context(), key.ID, c.Param("job"))
	if e != nil {
		publicError(c, 409, "job_cannot_cancel", "Only unsubmitted jobs can be canceled")
		return
	}
	c.JSON(200, gin.H{"id": c.Param("job"), "status": "canceled"})
}
func (h *Handler) adminImageJobs(c *gin.Context) {
	offset, _ := strconv.Atoi(c.Query("offset"))
	limit, _ := strconv.Atoi(c.Query("limit"))
	v, total, e := h.service.ImageJobs(c.Request.Context(), 0, provider(c), offset, limit)
	reply(c, gin.H{"items": v, "total": total}, e)
}
func (h *Handler) createImageJob(c *gin.Context) { h.imageRequest(c, true, true) }
func (h *Handler) imageRequest(c *gin.Context, async, required bool) bool {
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
		v.Operation = c.PostForm("operation")
		v.N, _ = strconv.Atoi(c.PostForm("n"))
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
		if e := json.Unmarshal(data, &v); e != nil {
			if !required {
				return false
			}
			publicError(c, 400, "invalid_image_input", "Invalid image JSON")
			return true
		}
	}
	if !async {
		if strings.HasSuffix(c.Request.URL.Path, "/edits") {
			v.Operation = "image_edit"
		} else {
			v.Operation = "image"
		}
	} else {
		switch v.Operation {
		case "generate", "generation":
			v.Operation = "image"
		case "edit":
			v.Operation = "image_edit"
		}
	}
	if v.Operation == "" {
		v.Operation = "image"
	}
	route, matched, e := h.service.Resolve(c.Request.Context(), v.Model, v.Operation)
	if e != nil {
		publicError(c, 503, "channel_unavailable", "Channel lookup failed")
		return true
	}
	if !matched {
		if !required {
			return false
		}
		publicError(c, 404, "model_not_found", "No available extension image route")
		return true
	}
	c.Abort()
	if isMultipart {
		defer c.Request.MultipartForm.RemoveAll()
		allowed := map[string]bool{"operation": true, "model": true, "prompt": true, "n": true, "size": true, "quality": true, "response_format": true, "output_format": true, "background": true}
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
		}
	}
	if !isMultipart {
		data, _ := io.ReadAll(c.Request.Body)
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if e := decoder.Decode(&v); e != nil {
			publicError(c, 400, "unsupported_parameter", "Unsupported image parameter")
			return true
		}
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(data, &fields)
		if _, exists := fields["n"]; exists && (v.N < 1 || v.N > 10) {
			publicError(c, 400, "invalid_n", "n must be 1–10")
			return true
		}
		if !async {
			if strings.HasSuffix(c.Request.URL.Path, "/edits") {
				v.Operation = "image_edit"
			} else {
				v.Operation = "image"
			}
		} else {
			switch v.Operation {
			case "generate", "generation":
				v.Operation = "image"
			case "edit":
				v.Operation = "image_edit"
			case "":
				v.Operation = "image"
			}
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
		return true
	}
	if e := h.service.CheckAccess(c.Request.Context(), key, route); e != nil {
		publicError(c, 404, "model_not_found", "Model unavailable")
		return true
	}
	request := domain.ImageRequest{Operation: v.Operation, Model: v.Model, Prompt: v.Prompt, N: v.N, Size: v.Size, Quality: v.Quality, OutputFormat: v.OutputFormat, Background: v.Background}
	if len(v.Image) > 0 && string(v.Image) != "null" {
		v.Images = append(v.Images, v.Image)
	}
	if len(v.Images)+len(files) > 8 {
		publicError(c, 400, "too_many_images", "At most 8 reference images are supported")
		return true
	}
	save := func(data []byte, name string) (domain.ImageInput, error) {
		if len(data) > 20<<20 {
			return domain.ImageInput{}, errors.New("image too large")
		}
		return h.service.SaveImageInput(c.Request.Context(), domain.Blob{Name: name, Data: data})
	}
	rawImage := func(raw json.RawMessage) ([]byte, error) {
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
		if strings.HasPrefix(u, "data:") {
			head, data, ok := strings.Cut(u, ",")
			if !ok || !strings.HasSuffix(head, ";base64") || len(data) > 28<<20 {
				return nil, errors.New("invalid data URI")
			}
			return base64.StdEncoding.DecodeString(data)
		}
		return mediahttp.FetchRemoteImage(c.Request.Context(), u)
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
		data, e := rawImage(raw)
		if e != nil {
			publicError(c, 400, "invalid_image", "Could not read reference image")
			return true
		}
		input, e := save(data, "image")
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
		input, e := save(data, file.Filename)
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
			data, e = rawImage(v.Mask)
		}
		if e != nil {
			publicError(c, 400, "invalid_mask", "Could not read mask")
			return true
		}
		input, e := save(data, "mask.png")
		if e != nil {
			publicError(c, 400, "invalid_mask", "Could not store mask")
			return true
		}
		request.Mask = &input
	}
	job, e := h.service.SubmitImages(c.Request.Context(), key, route, request, c.GetHeader("Idempotency-Key"))
	if e != nil {
		status := 400
		if errors.Is(e, domain.ErrConflict) {
			status = 409
		}
		publicError(c, status, "image_job_rejected", "Image job rejected; check permissions, model, price and input requirements")
		return true
	}
	c.Header("X-Image-Job-ID", job.ID)
	c.Header("Location", "/v1/images/jobs/"+job.ID)
	if async {
		c.JSON(202, job)
		return true
	}
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
