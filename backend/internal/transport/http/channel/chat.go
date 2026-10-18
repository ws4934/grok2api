package channelhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	app "github.com/chenyme/grok2api/backend/internal/application/channel"
	keyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	domain "github.com/chenyme/grok2api/backend/internal/domain/channel"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func strictJSON(raw []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if e := decoder.Decode(value); e != nil {
		return e
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("expected one JSON value")
	}
	return nil
}

func publicError(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, gin.H{"error": openAIError(status, code, message)})
}
func openAIError(status int, code, message string) gin.H {
	t := "invalid_request_error"
	switch {
	case code == "insufficient_quota":
		t = code
	case status >= 500:
		t = "server_error"
	case status == 401:
		t = "authentication_error"
	case status == 403:
		t = "permission_error"
	case status == 429:
		t = "rate_limit_error"
	}
	return gin.H{"type": t, "code": code, "message": message, "param": nil}
}

type failure struct {
	status        int
	code, message string
}

// chatFailure maps a Service.Chat error to a client-facing status and code.
func (h *Handler) chatFailure(route model.Route, e error) failure {
	switch {
	case errors.Is(e, domain.ErrNoAccount):
		return failure{503, "no_available_account", "No channel account is available; retry later"}
	case errors.Is(e, domain.ErrQuotaExhausted), errors.Is(e, keyapp.ErrBillingLimit):
		return failure{429, "insufficient_quota", "Quota exhausted"}
	case errors.Is(e, domain.ErrDisabled), errors.Is(e, domain.ErrNotFound):
		return failure{404, "model_not_found", "Model unavailable"}
	case errors.Is(e, context.DeadlineExceeded):
		return failure{504, "upstream_timeout", "Channel request timed out"}
	}
	switch h.service.Classify(route.Provider, e).Kind {
	case domain.FailureQuota:
		return failure{429, "rate_limit_exceeded", "Upstream rate limit reached; retry later"}
	case domain.FailureAuth:
		return failure{503, "upstream_unavailable", "Upstream account unavailable; retry later"}
	case domain.FailureRequest:
		return failure{400, "upstream_rejected_request", "The upstream rejected the request"}
	}
	return failure{502, "channel_request_failed", "Channel request failed; check channel configuration and account state"}
}

// sse writes Server-Sent Events and commits the 200 status on the first write.
type sse struct {
	c       *gin.Context
	started bool
}

func (s *sse) write(frame string) error {
	if !s.started {
		s.c.Header("Content-Type", "text/event-stream")
		s.c.Header("Cache-Control", "no-cache")
		s.c.Header("X-Accel-Buffering", "no")
		s.c.Status(200)
		s.started = true
	}
	if _, e := s.c.Writer.WriteString(frame); e != nil {
		return e
	}
	s.c.Writer.Flush()
	return nil
}
func (s *sse) send(event string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	if event != "" {
		event = "event: " + event + "\n"
	}
	return s.write(event + "data: " + string(b) + "\n\n")
}

// Dispatch is an optional compatibility bridge. Unmatched requests retain the
// existing handler and body, including all original Grok behavior.
func (h *Handler) Dispatch() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if c.Request.Method == "GET" && strings.HasPrefix(path, "/v1/responses/"+app.ResponseIDPrefix) {
			if !h.getImageResponse(c) {
				c.Next()
			}
			return
		}
		if c.Request.Method == "POST" && (path == "/v1/images/generations" || path == "/v1/images/edits") {
			if !h.imageRequest(c) {
				c.Next()
			}
			return
		}
		responses, messages := path == "/v1/responses", path == "/v1/messages"
		if c.Request.Method != "POST" || (!responses && !messages && path != "/v1/chat/completions") {
			c.Next()
			return
		}
		reject := func(status int, code, message string) {
			if messages {
				anthropicError(c, status, message)
			} else {
				publicError(c, status, code, message)
			}
		}
		data, e := io.ReadAll(io.LimitReader(c.Request.Body, 32<<20+1))
		if e != nil || len(data) > 32<<20 {
			reject(413, "request_too_large", "Request exceeds the body limit")
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(data))
		if responses && h.imageResponse(c, data) {
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(data))
		var head struct {
			Model      string          `json:"model"`
			Modalities json.RawMessage `json:"modalities"`
		}
		if json.Unmarshal(data, &head) != nil {
			c.Next()
			return
		}
		operation := "chat"
		if responses {
			operation = "responses"
		}
		route, matched, e := h.service.Resolve(c.Request.Context(), head.Model, operation)
		if e != nil {
			reject(503, "channel_unavailable", "Channel lookup failed")
			return
		}
		if !responses && !messages && h.chatImage(c, data, head.Model, matched, bytes.Contains(head.Modalities, []byte(`"image"`))) {
			return
		}
		if !matched {
			c.Next()
			return
		}
		c.Abort()
		if messages {
			h.messages(c, route, data)
			return
		}
		in, e := decodeChat(data, responses)
		switch {
		case e != nil:
		case in.N > 1:
			e = errors.New("n greater than 1 is unsupported")
		case in.Image:
			e = errors.New("image output needs the image_generation tool")
		}
		if e != nil {
			publicError(c, 400, "unsupported_parameter", e.Error())
			return
		}
		key, ok := keyID(c)
		if !ok {
			publicError(c, 401, "invalid_api_key", "API key required")
			return
		}
		if responses {
			h.respond(c, key, route, in)
		} else {
			h.complete(c, key, route, in)
		}
	}
}

// completion renders Chat Completions objects and chunks.
type completion struct {
	out          *sse
	id, model    string
	created      int64
	includeUsage bool
}

func newCompletion(c *gin.Context, name string, includeUsage bool) *completion {
	return &completion{out: &sse{c: c}, id: "chatcmpl-" + uuid.NewString(), model: name, created: time.Now().Unix(), includeUsage: includeUsage}
}

// chunk sends one delta; the first chunk of a stream carries the role.
func (w *completion) chunk(delta gin.H, finish any) error {
	if !w.out.started {
		delta["role"] = "assistant"
	}
	v := gin.H{"id": w.id, "object": "chat.completion.chunk", "created": w.created, "model": w.model, "choices": []any{gin.H{"index": 0, "delta": delta, "finish_reason": finish}}}
	if w.includeUsage {
		v["usage"] = nil
	}
	return w.out.send("", v)
}

// end finishes a stream; usage goes in a final chunk with no choices when
// stream_options.include_usage is set, as in the OpenAI API.
func (w *completion) end(finish string, usage gin.H) {
	if !w.out.started {
		_ = w.chunk(gin.H{"content": ""}, nil)
	}
	_ = w.chunk(gin.H{}, finish)
	if w.includeUsage {
		_ = w.out.send("", gin.H{"id": w.id, "object": "chat.completion.chunk", "created": w.created, "model": w.model, "choices": []any{}, "usage": usage})
	}
	_ = w.out.write("data: [DONE]\n\n")
}
func (w *completion) reply(message gin.H, finish string, usage gin.H) {
	w.out.c.JSON(200, gin.H{"id": w.id, "object": "chat.completion", "created": w.created, "model": w.model, "choices": []any{gin.H{"index": 0, "message": message, "finish_reason": finish}}, "usage": usage})
}
func (w *completion) fail(f failure) {
	if !w.out.started {
		publicError(w.out.c, f.status, f.code, f.message)
		return
	}
	_ = w.out.send("error", gin.H{"error": openAIError(f.status, f.code, f.message)})
}
func chatUsage(prompt, completion, reasoning int64) gin.H {
	return gin.H{"prompt_tokens": prompt, "completion_tokens": completion, "total_tokens": prompt + completion,
		"prompt_tokens_details": gin.H{"cached_tokens": 0}, "completion_tokens_details": gin.H{"reasoning_tokens": reasoning}}
}

func (h *Handler) complete(c *gin.Context, key clientkey.Key, route model.Route, in chatInput) {
	w := newCompletion(c, in.Request.Model, in.IncludeUsage)
	result, e := h.service.Chat(c.Request.Context(), key, route, in.Request, in.Stream, func(ev domain.ChatEvent) error {
		if !in.Stream || ev.Done {
			return nil
		}
		delta := gin.H{"content": ev.Text}
		if ev.Reasoning != "" {
			delta["reasoning_content"] = ev.Reasoning
		}
		return w.chunk(delta, nil)
	})
	if e != nil {
		w.fail(h.chatFailure(route, e))
		return
	}
	finish := "stop"
	if result.Truncated {
		finish = "length"
	}
	usage := chatUsage(result.InputTokens, result.OutputTokens, h.service.EstimateTextTokens(result.Reasoning))
	if in.Stream {
		w.end(finish, usage)
		return
	}
	message := gin.H{"role": "assistant", "content": result.Text}
	if strings.TrimSpace(result.Reasoning) != "" {
		message["reasoning_content"] = result.Reasoning
	}
	w.reply(message, finish, usage)
}
