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
	Model          string          `json:"model"`
	Prompt         string          `json:"prompt"`
	N              *int            `json:"n"`
	Size           string          `json:"size"`
	Quality        string          `json:"quality"`
	ResponseFormat string          `json:"response_format"`
	OutputFormat   string          `json:"output_format"`
	Background     string          `json:"background"`
	Stream         bool            `json:"stream"`
	Images         json.RawMessage `json:"images"`
	Image          json.RawMessage `json:"image"`
	ImageURL       json.RawMessage `json:"image_url"`
	Mask           json.RawMessage `json:"mask"`
	// Accepted for client compatibility and ignored: the upstream has no
	// equivalent. output_compression is validated but never applied.
	User              string `json:"user"`
	Style             string `json:"style"`
	Moderation        string `json:"moderation"`
	OutputCompression *int   `json:"output_compression"`
	PartialImages     *int   `json:"partial_images"`
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

var (
	errFileID       = errors.New("file_id image references are not supported; use image_url instead")
	errImageRefForm = errors.New("unsupported image reference")
)

// imageRefs flattens the official and reference image forms into strings
// (data URI, http(s) URL or bare base64): a string, {"image_url": string |
// {"url"}}, {"url"}, {"b64_json"|"base64"}, or a top-level array of those.
func imageRefs(raw json.RawMessage, nested bool) ([]string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []string{s}, nil
	}
	var list []json.RawMessage
	if !nested && json.Unmarshal(raw, &list) == nil {
		var out []string
		for _, item := range list {
			refs, e := imageRefs(item, true)
			if e != nil {
				return nil, e
			}
			out = append(out, refs...)
		}
		return out, nil
	}
	var o struct {
		ImageURL json.RawMessage `json:"image_url"`
		URL      string          `json:"url"`
		B64JSON  string          `json:"b64_json"`
		Base64   string          `json:"base64"`
		FileID   string          `json:"file_id"`
	}
	if json.Unmarshal(raw, &o) != nil {
		return nil, errImageRefForm
	}
	switch {
	case o.FileID != "":
		return nil, errFileID
	case o.B64JSON != "":
		return []string{o.B64JSON}, nil
	case o.Base64 != "":
		return []string{o.Base64}, nil
	case o.URL != "":
		return []string{o.URL}, nil
	case len(o.ImageURL) > 0 && string(o.ImageURL) != "null":
		if json.Unmarshal(o.ImageURL, &s) != nil {
			var u struct {
				URL string `json:"url"`
			}
			if json.Unmarshal(o.ImageURL, &u) != nil || u.URL == "" {
				return nil, errImageRefForm
			}
			s = u.URL
		}
		return []string{s}, nil
	}
	return nil, errImageRefForm
}

// formImageRefs reads a multipart text value: one reference, or a JSON array or object.
func formImageRefs(value string) ([]string, error) {
	if t := strings.TrimSpace(value); strings.HasPrefix(t, "[") || strings.HasPrefix(t, "{") {
		return imageRefs(json.RawMessage(t), false)
	}
	return []string{value}, nil
}
func imageRefError(c *gin.Context, e error) {
	if errors.Is(e, errFileID) {
		publicError(c, 400, "unsupported_image_reference", e.Error())
		return
	}
	publicError(c, 400, "invalid_image", "Image must be a data URI, an https URL, base64, or an object with image_url, url, b64_json or base64")
}

// readImageURL resolves one reference: a base64 data URI, an http(s) URL
// (fetched with the shared SSRF guard) or bare base64.
func readImageURL(c *gin.Context, u string) ([]byte, error) {
	u = strings.TrimSpace(u)
	hasPrefix := func(p string) bool { return len(u) >= len(p) && strings.EqualFold(u[:len(p)], p) }
	switch {
	case hasPrefix("http://") || hasPrefix("https://"):
		return mediahttp.FetchRemoteImage(c.Request.Context(), u)
	case hasPrefix("data:"):
		head, data, ok := strings.Cut(u, ",")
		if !ok || !strings.HasSuffix(head, ";base64") {
			return nil, errors.New("invalid data URI")
		}
		u = data
	}
	if len(u) > 28<<20 {
		return nil, errors.New("image too large")
	}
	return base64.StdEncoding.DecodeString(u)
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

// imageWireProblem validates the options that are accepted but have no effect.
func imageWireProblem(v imageWire) (string, string) {
	switch {
	case v.N != nil && (*v.N < 1 || *v.N > 10):
		return "invalid_n", "n must be 1–10"
	case v.Moderation != "" && v.Moderation != "auto" && v.Moderation != "low":
		return "invalid_moderation", "moderation must be auto or low"
	case v.OutputCompression != nil && (*v.OutputCompression < 0 || *v.OutputCompression > 100):
		return "invalid_output_compression", "output_compression must be 0-100"
	case v.PartialImages != nil && (*v.PartialImages < 0 || *v.PartialImages > 3):
		return "invalid_partial_images", "partial_images must be 0-3"
	case v.ResponseFormat != "url" && v.ResponseFormat != "b64_json":
		return "invalid_response_format", "response_format must be url or b64_json"
	}
	return "", ""
}

// imageRequest serves the OpenAI Images API for extension routes, synchronously
// or as SSE when stream is true. It returns false, leaving the request
// untouched, when no extension route matches.
func (h *Handler) imageRequest(c *gin.Context) bool {
	var v imageWire
	var files []*multipart.FileHeader
	var form *multipart.Form
	isMultipart := strings.HasPrefix(c.GetHeader("Content-Type"), "multipart/form-data")
	if isMultipart {
		if e := c.Request.ParseMultipartForm(20 << 20); e != nil {
			publicError(c, 400, "invalid_image_input", "Invalid multipart request")
			return true
		}
		form = c.Request.MultipartForm
		v.Model = c.PostForm("model")
		v.Prompt = c.PostForm("prompt")
		v.Size = c.PostForm("size")
		v.Quality = c.PostForm("quality")
		v.ResponseFormat = c.PostForm("response_format")
		v.OutputFormat = c.PostForm("output_format")
		v.Background = c.PostForm("background")
		v.Moderation = c.PostForm("moderation")
		for _, name := range []string{"image", "image[]", "images", "images[]"} {
			files = append(files, form.File[name]...)
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
	var refs, maskRefs []string
	if isMultipart {
		defer form.RemoveAll()
		allowed := map[string]bool{"model": true, "prompt": true, "n": true, "size": true, "quality": true, "response_format": true, "output_format": true, "background": true, "stream": true, "user": true, "style": true, "moderation": true, "output_compression": true, "partial_images": true, "mask": true}
		for name, values := range form.Value {
			if name != "image_url" && name != "image_url[]" && (!allowed[name] || len(values) != 1) {
				publicError(c, 400, "unsupported_parameter", "Unsupported or repeated multipart field")
				return true
			}
		}
		for name := range form.File {
			if name != "image" && name != "image[]" && name != "images" && name != "images[]" && name != "mask" {
				publicError(c, 400, "unsupported_parameter", "Unsupported multipart file field")
				return true
			}
		}
		number := func(name string) (*int, bool) {
			raw := c.PostForm(name)
			if raw == "" {
				return nil, true
			}
			n, err := strconv.Atoi(raw)
			return &n, err == nil
		}
		var okN, okCompression, okPartial bool
		v.N, okN = number("n")
		v.OutputCompression, okCompression = number("output_compression")
		v.PartialImages, okPartial = number("partial_images")
		okStream := true
		if raw := c.PostForm("stream"); raw != "" {
			var err error
			v.Stream, err = strconv.ParseBool(raw)
			okStream = err == nil
		}
		if !okN || !okCompression || !okPartial || !okStream {
			publicError(c, 400, "invalid_parameter", "n, output_compression and partial_images must be integers; stream must be boolean")
			return true
		}
		for _, value := range append(append([]string{}, form.Value["image_url"]...), form.Value["image_url[]"]...) {
			got, err := formImageRefs(value)
			if err != nil {
				imageRefError(c, err)
				return true
			}
			refs = append(refs, got...)
		}
		for _, value := range form.Value["mask"] {
			got, err := formImageRefs(value)
			if err != nil {
				imageRefError(c, err)
				return true
			}
			maskRefs = append(maskRefs, got...)
		}
	} else {
		data, _ := io.ReadAll(c.Request.Body)
		if strictJSON(data, &v) != nil {
			publicError(c, 400, "unsupported_parameter", "Unsupported image parameter")
			return true
		}
	}
	if v.ResponseFormat == "" {
		v.ResponseFormat = "b64_json"
	}
	if code, message := imageWireProblem(v); code != "" {
		publicError(c, 400, code, message)
		return true
	}
	for _, raw := range []json.RawMessage{v.Images, v.Image, v.ImageURL} {
		got, err := imageRefs(raw, false)
		if err != nil {
			imageRefError(c, err)
			return true
		}
		refs = append(refs, got...)
	}
	jsonMask, err := imageRefs(v.Mask, false)
	if err != nil {
		imageRefError(c, err)
		return true
	}
	maskRefs = append(maskRefs, jsonMask...)
	var maskFiles []*multipart.FileHeader
	if isMultipart {
		maskFiles = form.File["mask"]
	}
	if len(maskFiles)+len(maskRefs) > 1 {
		publicError(c, 400, "invalid_mask", "Only one mask is supported")
		return true
	}
	if len(refs)+len(files) > 8 {
		publicError(c, 400, "too_many_images", "At most 8 reference images are supported")
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
	request := domain.ImageRequest{Operation: operation, Model: v.Model, Prompt: v.Prompt, Size: v.Size, Quality: v.Quality, OutputFormat: v.OutputFormat, Background: v.Background}
	if v.N != nil {
		request.N = *v.N
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
	for _, ref := range refs {
		data, e := readImageURL(c, ref)
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
	if len(maskFiles)+len(maskRefs) == 1 {
		var data []byte
		var e error
		if len(maskFiles) == 1 {
			data, e = readFile(maskFiles[0])
		} else {
			data, e = readImageURL(c, maskRefs[0])
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
	if v.Stream {
		h.streamImages(c, key.ID, job, v.ResponseFormat)
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
	r := job.Request
	size := r.Size
	for _, item := range job.Items {
		if item.Status == "succeeded" {
			size = imageSize(item, r)
			break
		}
	}
	c.JSON(200, gin.H{"created": job.CreatedAt.Unix(), "data": out, "usage": h.imageUsage(c.Request.Context(), job, job.Items), "size": size, "quality": r.Quality, "output_format": r.OutputFormat, "background": r.Background, "job_id": job.ID, "status": job.Status})
	return true
}
