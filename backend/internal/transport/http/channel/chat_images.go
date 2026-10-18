package channelhttp

import (
	"errors"
	"fmt"
	"strings"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/channel"
	"github.com/gin-gonic/gin"
)

// chatImage generates images requested through Chat Completions: the model is
// an image route, or modalities asks for an image. The prompt and reference
// images come from the last user message; the reply is markdown linking our
// public image URLs. It returns false, leaving the request untouched, when no
// image route applies.
func (h *Handler) chatImage(c *gin.Context, data []byte, model string, chatMatched, wantsImage bool) bool {
	if chatMatched && !wantsImage {
		return false
	}
	// Only the image route is looked up here, so unmatched (legacy) chat
	// requests pay one extra lookup; edits resolve image_edit below.
	ctx := c.Request.Context()
	route, matched, e := h.service.Resolve(ctx, model, "image")
	if e != nil {
		publicError(c, 503, "channel_unavailable", "Channel lookup failed")
		return true
	}
	if !matched {
		if chatMatched {
			publicError(c, 400, "unsupported_parameter", "Image output needs an image model")
			return true
		}
		return false
	}
	c.Abort()
	in, e := decodeChat(data, false)
	if e == nil && in.N > 10 {
		e = errors.New("n must be 1-10")
	}
	if e != nil {
		publicError(c, 400, "unsupported_parameter", e.Error())
		return true
	}
	var last domain.Message
	for _, m := range in.Request.Messages {
		if m.Role == "user" {
			last = m
		}
	}
	var texts, urls []string
	for _, p := range last.Content {
		if p.Type == "text" {
			texts = append(texts, p.Text)
		} else {
			urls = append(urls, p.ImageURL)
		}
	}
	prompt := strings.TrimSpace(strings.Join(texts, "\n"))
	if prompt == "" {
		publicError(c, 400, "invalid_prompt", "The last user message must contain the image prompt")
		return true
	}
	operation := "image"
	if len(urls) > 0 {
		operation = "image_edit"
		if route, matched, e = h.service.Resolve(ctx, model, operation); e != nil {
			publicError(c, 503, "channel_unavailable", "Channel lookup failed")
			return true
		}
	}
	if !matched {
		publicError(c, 404, "model_not_found", "Model does not support image editing")
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
	request := domain.ImageRequest{Operation: operation, Model: in.Request.Model, PublicModel: in.Request.Model, Prompt: prompt, N: in.N}
	for i, u := range urls {
		raw, e := readImageURL(c, u)
		if e != nil {
			publicError(c, 400, "invalid_image", "Could not read input image")
			return true
		}
		input, e := h.saveImage(c, raw, fmt.Sprintf("image-%d", i+1))
		if e != nil {
			publicError(c, 400, "invalid_image", "Could not store input image")
			return true
		}
		request.Inputs = append(request.Inputs, input)
	}
	job, e := h.service.SubmitImages(ctx, key, route, request, c.GetHeader("Idempotency-Key"))
	if e != nil {
		imageRejected(c, e)
		return true
	}
	// The job stays readable as a Responses object after a client disconnect.
	c.Header("X-Image-Job-ID", job.ID)
	c.Header("Location", "/v1/responses/"+job.ID)
	w := newCompletion(c, in.Request.Model, in.IncludeUsage)
	if in.Stream {
		_ = w.chunk(gin.H{"content": ""}, nil)
	}
	type waited struct {
		job domain.ImageJob
		err error
	}
	done := make(chan waited, 1)
	go func() {
		j, e := h.service.WaitImageJob(ctx, key.ID, job.ID)
		done <- waited{j, e}
	}()
	// Comments keep proxies from closing an idle stream while the image renders.
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	var result waited
	for waiting := true; waiting; {
		select {
		case result = <-done:
			waiting = false
		case <-tick.C:
			if in.Stream {
				_ = w.out.write(": keep-alive\n\n")
			}
		}
	}
	if result.err != nil {
		w.fail(failure{503, "jobs_unavailable", "Image job lookup failed; inspect X-Image-Job-ID"})
		return true
	}
	var images []string
	if result.job.Status == "succeeded" || result.job.Status == "partially_succeeded" {
		out, _ := h.service.ImageResults(ctx, key.ID, job.ID, "url")
		for _, r := range out {
			images = append(images, "![image]("+r.URL+")")
		}
	}
	if len(images) == 0 {
		w.fail(failure{502, "image_generation_failed", "Image task did not deliver a result; inspect X-Image-Job-ID"})
		return true
	}
	content := strings.Join(images, "\n\n")
	usage := chatUsage(h.service.EstimateTextTokens(prompt), 0, 0)
	if in.Stream {
		_ = w.chunk(gin.H{"content": content}, nil)
		w.end("stop", usage)
	} else {
		w.reply(gin.H{"role": "assistant", "content": content}, "stop", usage)
	}
	return true
}
