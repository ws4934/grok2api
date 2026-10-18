package channelhttp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/channel"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
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

func DecodeChat(data []byte, responses bool) (domain.ChatRequest, bool, error) {
	var raw map[string]json.RawMessage
	if e := json.Unmarshal(data, &raw); e != nil || raw == nil {
		return domain.ChatRequest{}, false, errors.New("request must be a JSON object")
	}
	allowed := map[string]bool{"model": true, "stream": true}
	if responses {
		allowed["input"] = true
		allowed["instructions"] = true
		allowed["store"] = true
	} else {
		allowed["messages"] = true
		allowed["stream_options"] = true
	}
	for k := range raw {
		if !allowed[k] {
			return domain.ChatRequest{}, false, fmt.Errorf("unsupported parameter: %s", k)
		}
	}
	var r domain.ChatRequest
	var stream bool
	if json.Unmarshal(raw["model"], &r.Model) != nil || r.Model == "" {
		return r, false, errors.New("model is required")
	}
	if v, ok := raw["stream"]; ok {
		if json.Unmarshal(v, &stream) != nil {
			return r, false, errors.New("stream must be boolean")
		}
	}
	var messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
		Type    string          `json:"type"`
	}
	if responses {
		if v, ok := raw["store"]; ok {
			var store bool
			if json.Unmarshal(v, &store) != nil || store {
				return r, false, errors.New("stored Responses are unsupported")
			}
		}
		var instructions string
		if v, ok := raw["instructions"]; ok {
			if json.Unmarshal(v, &instructions) != nil {
				return r, false, errors.New("instructions must be text")
			}
			if instructions != "" {
				r.Messages = append(r.Messages, domain.Message{Role: "system", Content: []domain.ContentPart{{Type: "text", Text: instructions}}})
			}
		}
		var text string
		if string(raw["input"]) == "null" {
			return r, false, errors.New("input cannot be null")
		}
		if json.Unmarshal(raw["input"], &text) == nil {
			r.Messages = append(r.Messages, domain.Message{Role: "user", Content: []domain.ContentPart{{Type: "text", Text: text}}})
		} else if strictJSON(raw["input"], &messages) != nil {
			return r, false, errors.New("input must be text or messages")
		}
	} else {
		if strictJSON(raw["messages"], &messages) != nil {
			return r, false, errors.New("messages must be an array")
		}
		if v, ok := raw["stream_options"]; ok {
			var options map[string]json.RawMessage
			if json.Unmarshal(v, &options) != nil {
				return r, false, errors.New("invalid stream_options")
			}
			for k, v := range options {
				var b bool
				if k != "include_usage" || json.Unmarshal(v, &b) != nil {
					return r, false, errors.New("unsupported stream_options")
				}
			}
		}
	}
	images := 0
	for _, m := range messages {
		if m.Type != "" && m.Type != "message" {
			return r, false, errors.New("unsupported input item")
		}
		role := m.Role
		if role == "developer" {
			role = "system"
		}
		if role != "system" && role != "user" && role != "assistant" {
			return r, false, errors.New("unsupported message role")
		}
		out := domain.Message{Role: role}
		var text string
		if string(m.Content) == "null" {
			return r, false, errors.New("message content cannot be null")
		}
		if json.Unmarshal(m.Content, &text) == nil {
			out.Content = []domain.ContentPart{{Type: "text", Text: text}}
		} else {
			var parts []struct {
				Type     string          `json:"type"`
				Text     string          `json:"text"`
				ImageURL json.RawMessage `json:"image_url"`
				Detail   string          `json:"detail"`
			}
			if strictJSON(m.Content, &parts) != nil {
				return r, false, errors.New("invalid message content")
			}
			for _, p := range parts {
				switch p.Type {
				case "text", "input_text", "output_text":
					out.Content = append(out.Content, domain.ContentPart{Type: "text", Text: p.Text})
				case "image_url", "input_image":
					var u string
					if json.Unmarshal(p.ImageURL, &u) != nil {
						var image struct {
							URL    string `json:"url"`
							Detail string `json:"detail"`
						}
						if strictJSON(p.ImageURL, &image) != nil {
							return r, false, errors.New("invalid image_url")
						}
						u = image.URL
						p.Detail = image.Detail
					}
					if u == "" {
						return r, false, errors.New("image_url is required")
					}
					if p.Detail != "" && p.Detail != "auto" {
						return r, false, errors.New("image detail is unsupported")
					}
					images++
					out.Content = append(out.Content, domain.ContentPart{Type: "image_url", ImageURL: u})
				default:
					return r, false, errors.New("unsupported content part")
				}
			}
		}
		if len(out.Content) == 0 {
			return r, false, errors.New("message content is empty")
		}
		r.Messages = append(r.Messages, out)
	}
	if len(r.Messages) == 0 || len(r.Messages) > 1024 || images > 8 {
		return r, false, errors.New("invalid message or image count")
	}
	return r, stream, nil
}
func publicError(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"type": "invalid_request_error", "code": code, "message": message, "param": nil}})
}

// Dispatch is an optional compatibility bridge. Unmatched requests retain the
// existing handler and body, including all original Grok behavior.
func (h *Handler) Dispatch() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if c.Request.Method == "POST" && (path == "/v1/images/generations" || path == "/v1/images/edits") {
			if h.imageRequest(c, false, false) {
				return
			}
			c.Next()
			return
		}
		responses := path == "/v1/responses"
		if c.Request.Method != "POST" || (!responses && path != "/v1/chat/completions") {
			c.Next()
			return
		}
		data, e := io.ReadAll(io.LimitReader(c.Request.Body, 32<<20+1))
		if e != nil || len(data) > 32<<20 {
			publicError(c, 413, "request_too_large", "Request exceeds the body limit")
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(data))
		var head struct {
			Model string `json:"model"`
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
			publicError(c, 503, "channel_unavailable", "Channel lookup failed")
			return
		}
		if !matched {
			c.Next()
			return
		}
		c.Abort()
		r, stream, e := DecodeChat(data, responses)
		if e != nil {
			publicError(c, 400, "unsupported_parameter", e.Error())
			return
		}
		v, _ := c.Get(middleware.ClientKey)
		key, ok := v.(clientkey.Key)
		if !ok {
			publicError(c, 401, "invalid_api_key", "API key required")
			return
		}
		responseID := "chatcmpl-" + uuid.NewString()
		if responses {
			responseID = "resp_" + uuid.NewString()
		}
		messageID := "msg_" + uuid.NewString()
		created := time.Now().Unix()
		started := false
		send := func(name string, v any) error {
			if !started {
				c.Header("Content-Type", "text/event-stream")
				c.Header("Cache-Control", "no-cache")
				c.Header("X-Accel-Buffering", "no")
				c.Status(200)
				started = true
			}
			b, e := json.Marshal(v)
			if e != nil {
				return e
			}
			var prefix string
			if name != "" {
				prefix = "event: " + name + "\n"
			}
			if _, e = c.Writer.Write([]byte(prefix + "data: " + string(b) + "\n\n")); e != nil {
				return e
			}
			c.Writer.Flush()
			return nil
		}
		reasoningID := "rs_" + uuid.NewString()
		reasoningStarted := false
		result, e := h.service.Chat(c.Request.Context(), key, route, r, stream, func(event domain.ChatEvent) error {
			if !stream || event.Done {
				return nil
			}
			if responses {
				if !started {
					if e := send("response.created", gin.H{"type": "response.created", "response": gin.H{"id": responseID, "object": "response", "status": "in_progress", "output": []any{}}}); e != nil {
						return e
					}
					if e := send("response.output_item.added", gin.H{"type": "response.output_item.added", "output_index": 0, "item": gin.H{"id": messageID, "type": "message", "role": "assistant", "content": []any{}}}); e != nil {
						return e
					}
				}
				if event.Reasoning != "" {
					if !reasoningStarted {
						if err := send("response.output_item.added", gin.H{"type": "response.output_item.added", "output_index": 1, "item": gin.H{"id": reasoningID, "type": "reasoning", "summary": []any{}}}); err != nil {
							return err
						}
						reasoningStarted = true
					}
					if err := send("response.reasoning_summary_text.delta", gin.H{"type": "response.reasoning_summary_text.delta", "item_id": reasoningID, "output_index": 1, "summary_index": 0, "delta": event.Reasoning}); err != nil {
						return err
					}
				}
				if event.Text != "" {
					return send("response.output_text.delta", gin.H{"type": "response.output_text.delta", "response_id": responseID, "item_id": messageID, "output_index": 0, "content_index": 0, "delta": event.Text})
				}
				return nil
			}
			delta := gin.H{"content": event.Text}
			if event.Reasoning != "" {
				delta["reasoning_content"] = event.Reasoning
			}
			return send("", gin.H{"id": responseID, "object": "chat.completion.chunk", "created": created, "model": head.Model, "choices": []any{gin.H{"index": 0, "delta": delta, "finish_reason": nil}}})
		})
		if e != nil {
			if !started {
				publicError(c, 502, "channel_request_failed", "Channel request failed; check channel configuration and account state")
			} else {
				_ = send("error", gin.H{"error": gin.H{"code": "channel_request_failed", "message": "Channel request interrupted"}})
			}
			return
		}
		finish := "stop"
		if result.Truncated {
			finish = "length"
		}
		usage := gin.H{"prompt_tokens": result.InputTokens, "completion_tokens": result.OutputTokens, "total_tokens": result.InputTokens + result.OutputTokens}
		if responses {
			status := "completed"
			var incomplete any
			if result.Truncated {
				status = "incomplete"
				incomplete = gin.H{"reason": "max_output_tokens", "source": "gateway_estimate"}
			}
			out := gin.H{"id": responseID, "object": "response", "created_at": created, "model": head.Model, "status": status, "incomplete_details": incomplete, "output": []any{gin.H{"id": messageID, "type": "message", "role": "assistant", "status": "completed", "content": []any{gin.H{"type": "output_text", "text": result.Text, "annotations": []any{}}}}}, "usage": gin.H{"input_tokens": result.InputTokens, "output_tokens": result.OutputTokens, "total_tokens": result.InputTokens + result.OutputTokens}}
			if result.Reasoning != "" {
				out["output"] = append(out["output"].([]any), gin.H{"id": reasoningID, "type": "reasoning", "summary": []any{gin.H{"type": "summary_text", "text": result.Reasoning}}})
			}
			if stream {
				if result.Reasoning != "" {
					_ = send("response.reasoning_summary_text.done", gin.H{"type": "response.reasoning_summary_text.done", "item_id": reasoningID, "output_index": 1, "summary_index": 0, "text": result.Reasoning})
				}
				_ = send("response.output_text.done", gin.H{"type": "response.output_text.done", "item_id": messageID, "output_index": 0, "content_index": 0, "text": result.Text})
				_ = send("response."+status, gin.H{"type": "response." + status, "response": out})
			} else {
				c.JSON(200, out)
			}
			return
		}
		if stream {
			_ = send("", gin.H{"id": responseID, "object": "chat.completion.chunk", "created": created, "model": head.Model, "choices": []any{gin.H{"index": 0, "delta": gin.H{}, "finish_reason": finish}}, "usage": usage})
			_, _ = c.Writer.WriteString("data: [DONE]\n\n")
			c.Writer.Flush()
		} else {
			message := gin.H{"role": "assistant", "content": result.Text}
			if strings.TrimSpace(result.Reasoning) != "" {
				message["reasoning_content"] = result.Reasoning
			}
			c.JSON(200, gin.H{"id": responseID, "object": "chat.completion", "created": created, "model": head.Model, "choices": []any{gin.H{"index": 0, "message": message, "finish_reason": finish}}, "usage": usage})
		}
	}
}

var _ = http.StatusOK
