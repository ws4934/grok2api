package inference

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

// OpenAI's asynchronous image API is the Responses API: POST /v1/responses
// with the image_generation tool (optionally background: true), then
// GET /v1/responses/{id} and POST /v1/responses/{id}/cancel. Routes whose
// provider draws resumable images run it as an image job; each response
// produces one image, matching the official tool.

// imageKeepAlive spaces the SSE comments sent while an image is drawn.
var imageKeepAlive = 15 * time.Second

const maxIdempotencyKeyLength = 160

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

func strictJSON(raw []byte, value any) error {
	return decodeSingleJSON(bytes.NewReader(raw), value, true)
}

// decodeImageResponse validates the request strictly: unsupported options are
// rejected instead of being silently dropped.
func decodeImageResponse(data []byte) (imageResponseRequest, imageResponseInput, error) {
	var r imageResponseRequest
	var out imageResponseInput
	if err := strictJSON(data, &r); err != nil {
		return r, out, fmt.Errorf("unsupported or invalid parameter: %v", err)
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
	case t.Mask != nil && strings.TrimSpace(t.Mask.ImageURL) == "":
		return r, out, errors.New("input_image_mask requires image_url")
	}
	return r, out, nil
}

// handleImageResponse serves POST /v1/responses carrying the image_generation
// tool as an image job. It returns false, leaving the request untouched, when
// no image route with resumable images matches.
func (h *Handler) handleImageResponse(c *gin.Context, body []byte, key clientkeydomain.Key, requestID string) bool {
	if !bytes.Contains(body, []byte(`"image_generation"`)) {
		return false
	}
	var head struct {
		Model string `json:"model"`
		Tools []struct {
			Type  string `json:"type"`
			Model string `json:"model"`
		} `json:"tools"`
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	if json.Unmarshal(body, &head) != nil {
		return false
	}
	found, toolModel := false, ""
	for _, tool := range head.Tools {
		if tool.Type == "image_generation" {
			found, toolModel = true, strings.TrimSpace(tool.Model)
		}
	}
	if !found {
		return false
	}
	ctx := c.Request.Context()
	topModel := strings.TrimSpace(head.Model)
	model := h.gateway.ImageToolModel(ctx, toolModel, topModel)
	if model == "" {
		return false
	}
	request, in, err := decodeImageResponse(body)
	// A text model that merely offers the tool (next to other tools, in a
	// conversation, with store=false...) stays on the conversation path. An
	// image model or a forced image tool takes the image path, whose strict
	// validation then answers 400.
	if !h.gateway.IsImageJobModel(ctx, topModel) && !forcesImageTool(head.ToolChoice, len(head.Tools)) &&
		(err != nil || (request.Store != nil && !*request.Store)) {
		return false
	}
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "unsupported_parameter", err.Error())
		return true
	}
	idempotencyKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if len(idempotencyKey) > maxIdempotencyKeyLength {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "Idempotency-Key 不能超过 160 个字符")
		return true
	}
	tool := in.Tool
	mask := ""
	if tool.Mask != nil {
		mask = strings.TrimSpace(tool.Mask.ImageURL)
	}
	job, err := h.gateway.CreateImageJob(ctx, gateway.ImageJobInput{
		RequestID: requestID, ClientKey: key, PublicModel: model, Prompt: in.Prompt, ImageURLs: in.Images, MaskURL: mask,
		Size: normalizeImageOption(tool.Size), Quality: normalizeImageOption(tool.Quality),
		OutputFormat: normalizeImageOption(tool.OutputFormat), Background: normalizeImageOption(tool.Background),
		ResponseModel: request.Model, ResponseBackground: request.Background, IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		writeGatewayError(c, err)
		return true
	}
	switch {
	case request.Stream:
		h.streamImageResponse(c, key, job)
	case request.Background:
		h.writeImageResponse(c, job)
	default:
		// A disconnect cancels only unsubmitted work; the response stays readable.
		if job, err = h.gateway.WaitImageJob(ctx, key, job.ID, nil); err != nil {
			if ctx.Err() == nil {
				writeGatewayError(c, err)
			}
			return true
		}
		h.writeImageResponse(c, job)
	}
	return true
}

// forcesImageTool reports a tool_choice that leaves the model no option but
// the image_generation tool.
func forcesImageTool(choice json.RawMessage, tools int) bool {
	var name string
	if json.Unmarshal(choice, &name) == nil {
		return name == "required" && tools == 1
	}
	var forced struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(choice, &forced) == nil && forced.Type == "image_generation"
}

func requestClientKey(c *gin.Context) (clientkeydomain.Key, bool) {
	value, exists := c.Get(middleware.ClientKey)
	key, ok := value.(clientkeydomain.Key)
	return key, exists && ok
}

// getImageResponse serves GET /v1/responses/{id} for an image job.
func (h *Handler) getImageResponse(c *gin.Context, id string) {
	key, ok := requestClientKey(c)
	if !ok {
		writeOpenAIError(c, http.StatusUnauthorized, "invalid_api_key", "客户端 API Key 无效")
		return
	}
	if c.Query("stream") == "true" {
		writeOpenAIError(c, http.StatusBadRequest, "unsupported_parameter", "Streaming image responses are unsupported; poll without stream")
		return
	}
	job, err := h.gateway.GetImageJob(c.Request.Context(), key, id)
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	h.writeImageResponse(c, job)
}

// cancelResponse serves POST /v1/responses/{id}/cancel. Only image jobs can be
// cancelled, and only before their image reached the upstream; a finished
// response is returned unchanged.
func (h *Handler) cancelResponse(c *gin.Context) {
	key, ok := requestClientKey(c)
	if !ok {
		writeOpenAIError(c, http.StatusUnauthorized, "invalid_api_key", "客户端 API Key 无效")
		return
	}
	id := strings.TrimSpace(c.Param("responseId"))
	job, err := h.gateway.CancelImageJob(c.Request.Context(), key, id)
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	h.writeImageResponse(c, job)
}

func (h *Handler) writeImageResponse(c *gin.Context, job mediadomain.Job) {
	images, usage, err := h.gateway.ImageJobOutput(c.Request.Context(), job)
	if err != nil {
		writeOpenAIError(c, http.StatusServiceUnavailable, "result_unavailable", "Image result unavailable")
		return
	}
	c.JSON(http.StatusOK, imageResponseObject(job, images, usage))
}

func imageCallID(job mediadomain.Job, index int) string {
	return fmt.Sprintf("ig_%s%02d", strings.TrimPrefix(job.ID, gateway.ImageJobIDPrefix), index)
}

// imageResponseObject renders a job as an OpenAI Response with one
// image_generation_call per image.
func imageResponseObject(job mediadomain.Job, images []gateway.ImageJobImage, usage map[string]any) gin.H {
	status := "failed"
	switch job.Status {
	case mediadomain.StatusQueued:
		status = "queued"
	case mediadomain.StatusInProgress:
		status = "in_progress"
	case mediadomain.StatusCompleted:
		status = "completed"
	case mediadomain.StatusCanceled:
		status = "cancelled"
	}
	params := job.Params
	format, background, quality := params.OutputFormat, params.Background, job.Quality
	if format == "" {
		format = "png"
	}
	if background == "" {
		background = "auto"
	}
	if quality == "" {
		quality = "auto"
	}
	output := []any{}
	for index := range job.ItemCount {
		call := gin.H{"id": imageCallID(job, index), "type": "image_generation_call", "status": "failed", "result": nil}
		switch {
		case index < len(images):
			size := job.Size
			if images[index].Width > 0 && images[index].Height > 0 {
				size = fmt.Sprintf("%dx%d", images[index].Width, images[index].Height)
			}
			call["status"], call["result"] = "completed", base64.StdEncoding.EncodeToString(images[index].Data)
			call["output_format"], call["background"], call["quality"], call["size"] = format, background, quality, size
		case index > len(images) || job.Status == mediadomain.StatusQueued || job.Status == mediadomain.StatusCanceled:
			continue
		case job.Status == mediadomain.StatusInProgress:
			call["status"] = "in_progress"
			if job.Submitted {
				call["status"] = "generating"
			}
		}
		output = append(output, call)
	}
	var problem any
	if status == "failed" {
		code, message := "image_generation_failed", "Image generation failed"
		switch job.ErrorCode {
		case "invalid_request":
			code, message = "invalid_request", job.ErrorMessage
		case "submission_uncertain":
			code, message = "result_uncertain", "The image may have been submitted upstream, but its result is unknown"
		}
		problem = gin.H{"code": code, "message": message}
	}
	model := params.ResponseModel
	if model == "" {
		model = job.Model
	}
	var usageValue any
	if status == "completed" && usage != nil {
		usageValue = usage
	}
	return gin.H{
		"id": job.ID, "object": "response", "created_at": job.CreatedAt.Unix(), "status": status,
		"background": params.ResponseBackground, "error": problem, "incomplete_details": nil, "instructions": nil,
		"metadata": gin.H{}, "model": model, "output": output, "parallel_tool_calls": false,
		"tool_choice": gin.H{"type": "image_generation"}, "store": true, "usage": usageValue,
		"tools": []any{gin.H{"type": "image_generation", "model": job.Model, "size": job.Size, "quality": job.Quality, "background": params.Background, "output_format": params.OutputFormat}},
	}
}

// streamImageResponse emits the official Responses event sequence for one
// image_generation_call while the job runs. There are no partial images.
func (h *Handler) streamImageResponse(c *gin.Context, key clientkeydomain.Key, job mediadomain.Job) {
	ctx := c.Request.Context()
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Status(http.StatusOK)
	sequence, lastWrite := 0, time.Now()
	send := func(name string, body gin.H) {
		body["type"], body["sequence_number"] = name, sequence
		sequence++
		data, _ := json.Marshal(body)
		_, _ = fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", name, data)
		c.Writer.Flush()
		lastWrite = time.Now()
	}
	head := imageResponseObject(job, nil, nil)
	head["status"], head["output"] = "in_progress", []any{}
	send("response.created", gin.H{"response": head})
	send("response.in_progress", gin.H{"response": head})
	id := imageCallID(job, 0)
	send("response.output_item.added", gin.H{"output_index": 0, "item": gin.H{"id": id, "type": "image_generation_call", "status": "in_progress", "result": nil}})
	send("response.image_generation_call.in_progress", gin.H{"output_index": 0, "item_id": id})
	generating := false
	markGenerating := func() {
		if !generating {
			generating = true
			send("response.image_generation_call.generating", gin.H{"output_index": 0, "item_id": id})
		}
	}
	job, err := h.gateway.WaitImageJob(ctx, key, job.ID, func(job mediadomain.Job) {
		if job.Submitted {
			markGenerating()
		}
		if time.Since(lastWrite) >= imageKeepAlive {
			_, _ = c.Writer.WriteString(": keep-alive\n\n")
			c.Writer.Flush()
			lastWrite = time.Now()
		}
	})
	if ctx.Err() != nil {
		return
	}
	var final gin.H
	if err == nil {
		var images []gateway.ImageJobImage
		var usage map[string]any
		if images, usage, err = h.gateway.ImageJobOutput(ctx, job); err == nil {
			final = imageResponseObject(job, images, usage)
		}
	}
	if err != nil {
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
