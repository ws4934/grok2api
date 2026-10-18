package channelhttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	app "github.com/chenyme/grok2api/backend/internal/application/channel"
	domain "github.com/chenyme/grok2api/backend/internal/domain/channel"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/gin-gonic/gin"
)

// OpenAI's asynchronous image API is the Responses API: POST /v1/responses with
// the image_generation tool (optionally background: true), then
// GET /v1/responses/{id} and POST /v1/responses/{id}/cancel. Each response
// produces one image, matching the official tool.

type imageGenerationTool struct {
	Type          string `json:"type"`
	Model         string `json:"model"`
	Size          string `json:"size"`
	Quality       string `json:"quality"`
	Background    string `json:"background"`
	OutputFormat  string `json:"output_format"`
	PartialImages *int   `json:"partial_images"`
	Moderation    string `json:"moderation"`
	Action        string `json:"action"`
	Mask          *struct {
		ImageURL string `json:"image_url"`
	} `json:"input_image_mask"`
}
type imageResponseRequest struct {
	Model        string            `json:"model"`
	Input        json.RawMessage   `json:"input"`
	Instructions string            `json:"instructions"`
	Tools        []json.RawMessage `json:"tools"`
	ToolChoice   json.RawMessage   `json:"tool_choice"`
	Background   bool              `json:"background"`
	Store        *bool             `json:"store"`
	Stream       bool              `json:"stream"`
}
type imageResponseInput struct {
	Prompt string
	Images []string
	Tool   imageGenerationTool
}

// decodeImageResponse validates the request strictly: unsupported options are
// rejected instead of being silently dropped.
func decodeImageResponse(data []byte) (imageResponseRequest, imageResponseInput, error) {
	var r imageResponseRequest
	var out imageResponseInput
	if e := strictJSON(data, &r); e != nil {
		return r, out, fmt.Errorf("unsupported or invalid parameter: %v", e)
	}
	if r.Stream && r.Background {
		return r, out, errors.New("background image responses cannot be streamed; poll GET /v1/responses/{id}")
	}
	if len(r.Tools) != 1 || strictJSON(r.Tools[0], &out.Tool) != nil || out.Tool.Type != "image_generation" {
		return r, out, errors.New("exactly one image_generation tool with supported options is required")
	}
	t := out.Tool
	if t.PartialImages != nil && (*t.PartialImages < 0 || *t.PartialImages > 3) {
		return r, out, errors.New("partial_images must be 0-3")
	}
	if t.Moderation != "" && t.Moderation != "auto" {
		return r, out, errors.New("only moderation=auto is supported")
	}
	if len(r.ToolChoice) > 0 && string(r.ToolChoice) != "null" {
		var choice string
		var forced struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(r.ToolChoice, &choice) == nil {
			if choice != "auto" && choice != "required" {
				return r, out, errors.New("tool_choice must be auto, required or image_generation")
			}
		} else if strictJSON(r.ToolChoice, &forced) != nil || forced.Type != "image_generation" {
			return r, out, errors.New("tool_choice must be auto, required or image_generation")
		}
	}
	texts := []string{}
	if strings.TrimSpace(r.Instructions) != "" {
		texts = append(texts, r.Instructions)
	}
	var text string
	var items []struct {
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(r.Input, &text) == nil {
		texts = append(texts, text)
	} else if strictJSON(r.Input, &items) != nil {
		return r, out, errors.New("input must be text or messages")
	}
	for _, item := range items {
		if item.Type != "" && item.Type != "message" {
			return r, out, errors.New("only message input items are supported")
		}
		if item.Role != "user" && item.Role != "system" && item.Role != "developer" {
			return r, out, errors.New("only user, system and developer messages are supported")
		}
		if json.Unmarshal(item.Content, &text) == nil {
			texts = append(texts, text)
			continue
		}
		var parts []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			ImageURL string `json:"image_url"`
			Detail   string `json:"detail"`
		}
		if strictJSON(item.Content, &parts) != nil {
			return r, out, errors.New("invalid message content")
		}
		for _, p := range parts {
			switch {
			case p.Type == "input_text":
				texts = append(texts, p.Text)
			case p.Type == "input_image" && p.ImageURL != "" && (p.Detail == "" || p.Detail == "auto"):
				out.Images = append(out.Images, p.ImageURL)
			default:
				return r, out, errors.New("unsupported content part; use input_text or input_image with image_url")
			}
		}
	}
	out.Prompt = strings.TrimSpace(strings.Join(texts, "\n\n"))
	switch {
	case t.Action == "generate" && len(out.Images) > 0:
		return r, out, errors.New("action=generate does not take input images")
	case t.Action == "edit" && len(out.Images) == 0:
		return r, out, errors.New("action=edit requires an input image")
	case t.Action != "" && t.Action != "auto" && t.Action != "generate" && t.Action != "edit":
		return r, out, errors.New("action must be auto, generate or edit")
	case len(out.Images) > 8:
		return r, out, errors.New("at most 8 input images are supported")
	}
	return r, out, nil
}

// imageResponse handles POST /v1/responses carrying the image_generation tool
// for an extension route. It returns false for every other request.
func (h *Handler) imageResponse(c *gin.Context, data []byte) bool {
	var head struct {
		Model string `json:"model"`
		Tools []struct {
			Type  string `json:"type"`
			Model string `json:"model"`
		} `json:"tools"`
	}
	if json.Unmarshal(data, &head) != nil {
		return false
	}
	toolModel, found := "", false
	for _, t := range head.Tools {
		if t.Type == "image_generation" {
			found, toolModel = true, t.Model
		}
	}
	if !found {
		return false
	}
	name := toolModel
	if name == "" {
		name = head.Model
	}
	ctx := c.Request.Context()
	var generate, edit model.Route
	var generateOK, editOK bool
	resolve := func(public string) (e error) {
		if generate, generateOK, e = h.service.Resolve(ctx, public, "image"); e != nil {
			return e
		}
		edit, editOK, e = h.service.Resolve(ctx, public, "image_edit")
		return e
	}
	e := resolve(name)
	if e == nil && !generateOK && !editOK && toolModel == "" {
		// Official semantics: the top-level model is the mainline (text) model.
		if fallback := h.defaultImageModel(ctx, head.Model); fallback != "" {
			name = fallback
			e = resolve(name)
		}
	}
	if e != nil {
		publicError(c, 503, "channel_unavailable", "Channel lookup failed")
		return true
	}
	if !generateOK && !editOK {
		return false
	}
	c.Abort()
	r, in, e := decodeImageResponse(data)
	if e != nil {
		publicError(c, 400, "unsupported_parameter", e.Error())
		return true
	}
	operation, route, matched := "image", generate, generateOK
	if len(in.Images) > 0 {
		operation, route, matched = "image_edit", edit, editOK
	}
	if !matched {
		publicError(c, 404, "model_not_found", "Model does not support this image operation")
		return true
	}
	key, ok := keyID(c)
	if !ok {
		publicError(c, 401, "invalid_api_key", "API key required")
		return true
	}
	if e := h.service.CheckAccess(ctx, key, route); e != nil {
		publicError(c, 404, "model_not_found", "Model unavailable")
		return true
	}
	t := in.Tool
	request := domain.ImageRequest{Operation: operation, Model: name, PublicModel: r.Model, Async: r.Background, Prompt: in.Prompt, N: 1, Size: t.Size, Quality: t.Quality, OutputFormat: t.OutputFormat, Background: t.Background}
	for i, u := range in.Images {
		data, e := readImageURL(c, u)
		if e != nil {
			publicError(c, 400, "invalid_image", "Could not read input image")
			return true
		}
		input, e := h.saveImage(c, data, fmt.Sprintf("image-%d", i+1))
		if e != nil {
			publicError(c, 400, "invalid_image", "Could not store input image")
			return true
		}
		request.Inputs = append(request.Inputs, input)
	}
	if t.Mask != nil {
		data, e := readImageURL(c, t.Mask.ImageURL)
		if e != nil {
			publicError(c, 400, "invalid_mask", "Could not read input_image_mask")
			return true
		}
		input, e := h.saveImage(c, data, "mask.png")
		if e != nil {
			publicError(c, 400, "invalid_mask", "Could not store input_image_mask")
			return true
		}
		request.Mask = &input
	}
	job, e := h.service.SubmitImages(ctx, key, route, request, c.GetHeader("Idempotency-Key"))
	if e != nil {
		imageRejected(c, e)
		return true
	}
	if r.Stream {
		h.streamImageResponse(c, key.ID, job)
		return true
	}
	if !r.Background {
		// A disconnect cancels only unsubmitted work; the response stays readable.
		if job, e = h.service.WaitImageJob(ctx, key.ID, job.ID); e != nil {
			return true
		}
	}
	h.writeImageResponse(c, key.ID, job)
	return true
}

// defaultImageModel returns the channel's default public image route when the
// top-level model of an image_generation request is a channel text model.
func (h *Handler) defaultImageModel(ctx context.Context, name string) string {
	for _, operation := range []string{"responses", "chat"} {
		route, ok, e := h.service.Resolve(ctx, name, operation)
		if e != nil || !ok {
			continue
		}
		cfg, e := h.service.Settings(ctx, route.Provider)
		if e != nil {
			return ""
		}
		return cfg.DefaultImageModel
	}
	return ""
}

// getImageResponse serves GET /v1/responses/{id} for image jobs and returns
// false for any other ID so the legacy handler can answer it.
func (h *Handler) getImageResponse(c *gin.Context) bool {
	id := strings.TrimPrefix(c.Request.URL.Path, "/v1/responses/")
	key, ok := keyID(c)
	if !ok || strings.Contains(id, "/") {
		return false
	}
	job, e := h.service.GetImageJob(c.Request.Context(), key.ID, id)
	if errors.Is(e, domain.ErrNotFound) {
		return false
	}
	c.Abort()
	if e != nil {
		publicError(c, 503, "jobs_unavailable", "Response lookup failed")
		return true
	}
	if c.Query("stream") == "true" {
		publicError(c, 400, "unsupported_parameter", "Streaming image responses are unsupported")
		return true
	}
	h.writeImageResponse(c, key.ID, job)
	return true
}

// cancelImageResponse cancels work that has not reached the upstream. Once a
// generation is submitted it cannot be recalled, so cancellation is refused.
func (h *Handler) cancelImageResponse(c *gin.Context) {
	key, ok := keyID(c)
	if !ok {
		publicError(c, 401, "invalid_api_key", "API key required")
		return
	}
	ctx, id := c.Request.Context(), c.Param("responseId")
	job, e := h.service.GetImageJob(ctx, key.ID, id)
	if errors.Is(e, domain.ErrNotFound) {
		publicError(c, 404, "response_not_found", "No response found with id "+id)
		return
	}
	if e != nil {
		publicError(c, 503, "jobs_unavailable", "Response lookup failed")
		return
	}
	if !terminal(job.Status) {
		e = h.service.CancelImageJob(ctx, key.ID, id)
		if e != nil && !errors.Is(e, domain.ErrConflict) {
			publicError(c, 503, "jobs_unavailable", "Response cancellation failed")
			return
		}
		if job, e = h.service.GetImageJob(ctx, key.ID, id); e != nil {
			publicError(c, 503, "jobs_unavailable", "Response lookup failed")
			return
		}
		if !terminal(job.Status) {
			publicError(c, 409, "response_not_cancellable", "The image was already submitted upstream and can no longer be cancelled")
			return
		}
	}
	h.writeImageResponse(c, key.ID, job)
}
func terminal(status string) bool { return status != "queued" && status != "running" }

func (h *Handler) writeImageResponse(c *gin.Context, key uint64, job domain.ImageJob) {
	body, e := h.imageResponseBody(c.Request.Context(), key, job)
	if e != nil {
		publicError(c, 503, "result_unavailable", "Image result unavailable")
		return
	}
	c.JSON(200, body)
}

// imageResponseBody renders a job with its images and, once completed, usage.
func (h *Handler) imageResponseBody(ctx context.Context, key uint64, job domain.ImageJob) (gin.H, error) {
	var results []app.ImageResult
	var usage any
	if job.Status == "succeeded" || job.Status == "partially_succeeded" {
		var e error
		if results, e = h.service.ImageResults(ctx, key, job.ID, "b64_json"); e != nil {
			return nil, e
		}
		usage = h.imageUsage(ctx, job, job.Items)
	}
	return imageResponseObject(job, results, usage), nil
}
func imageCallID(item domain.ImageItem) string { return "ig_" + strings.ReplaceAll(item.ID, "-", "") }

// imageResponseObject renders a job as an OpenAI Response with one
// image_generation_call per generated image.
func imageResponseObject(job domain.ImageJob, results []app.ImageResult, usage any) gin.H {
	r := job.Request
	status := "failed"
	switch job.Status {
	case "queued":
		status = "queued"
	case "running":
		status = "in_progress"
	case "succeeded", "partially_succeeded":
		status = "completed"
	case "canceled":
		status = "cancelled"
	}
	output := []any{}
	next := 0
	for _, item := range job.Items {
		call := gin.H{"id": imageCallID(item), "type": "image_generation_call", "status": "failed", "result": nil}
		switch item.Status {
		case "queued":
			if job.Status == "queued" {
				continue
			}
			call["status"] = "in_progress"
		case "running":
			call["status"] = "in_progress"
			if item.Stage == "polling" || item.Stage == "downloading" {
				call["status"] = "generating"
			}
		case "succeeded":
			call["status"] = "completed"
			if len(item.AssetIDs) > 0 && next < len(results) {
				call["result"] = results[next].Base64
				next++
			}
			format := r.OutputFormat
			if format == "" {
				format = "png"
			}
			call["output_format"], call["background"], call["quality"], call["size"] = format, r.Background, r.Quality, imageSize(item, r)
		}
		output = append(output, call)
	}
	var problem any
	if status == "failed" {
		code, message := jobFailure(job)
		problem = gin.H{"code": code, "message": message}
	}
	model := r.PublicModel
	if model == "" {
		model = r.Model
	}
	return gin.H{
		"id": job.ID, "object": "response", "created_at": job.CreatedAt.Unix(), "status": status,
		"background": r.Async, "error": problem, "incomplete_details": nil, "instructions": nil,
		"metadata": gin.H{}, "model": model, "output": output, "parallel_tool_calls": false,
		"tool_choice": gin.H{"type": "image_generation"}, "store": true, "usage": usage,
		"tools": []any{gin.H{"type": "image_generation", "model": r.Model, "size": r.Size, "quality": r.Quality, "background": r.Background, "output_format": r.OutputFormat}},
	}
}

// streamImageResponse emits the official Responses event sequence for one
// image_generation_call while the job runs. There are no partial images.
func (h *Handler) streamImageResponse(c *gin.Context, key uint64, job domain.ImageJob) {
	ctx := c.Request.Context()
	sequence := 0
	send := func(name string, body gin.H) {
		body["type"], body["sequence_number"] = name, sequence
		sequence++
		sseEvent(c, name, body)
	}
	startSSE(c)
	head := imageResponseObject(job, nil, nil)
	head["status"], head["output"] = "in_progress", []any{}
	send("response.created", gin.H{"response": head})
	send("response.in_progress", gin.H{"response": head})
	id := imageCallID(job.Items[0])
	send("response.output_item.added", gin.H{"output_index": 0, "item": gin.H{"id": id, "type": "image_generation_call", "status": "in_progress", "result": nil}})
	send("response.image_generation_call.in_progress", gin.H{"output_index": 0, "item_id": id})
	generating := false
	markGenerating := func() {
		if !generating {
			generating = true
			send("response.image_generation_call.generating", gin.H{"output_index": 0, "item_id": id})
		}
	}
	job, e := h.watchImageJob(c, key, job, func(job domain.ImageJob) error {
		if stage := job.Items[0].Stage; stage == "polling" || stage == "downloading" {
			markGenerating()
		}
		return nil
	})
	if ctx.Err() != nil {
		return
	}
	final, err := h.imageResponseBody(ctx, key, job)
	if e != nil || err != nil {
		send("error", gin.H{"code": "result_unavailable", "message": "Image result unavailable", "param": nil})
		return
	}
	output, _ := final["output"].([]any)
	if final["status"] != "completed" || len(output) == 0 {
		send("response.failed", gin.H{"response": final})
		return
	}
	markGenerating()
	send("response.image_generation_call.completed", gin.H{"output_index": 0, "item_id": id})
	send("response.output_item.done", gin.H{"output_index": 0, "item": output[0]})
	send("response.completed", gin.H{"response": final})
}
