package channelhttp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	domain "github.com/chenyme/grok2api/backend/internal/domain/channel"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ignoredMessagesParams do not change the reply; the upstream has no
// sampling, stop sequence or thinking budget controls.
var ignoredMessagesParams = map[string]bool{
	"metadata": true, "temperature": true, "top_p": true, "top_k": true, "stop_sequences": true,
	"thinking": true, "service_tier": true, "context_management": true,
}

func anthropicError(c *gin.Context, status int, message string) {
	c.AbortWithStatusJSON(status, anthropicErrorBody(status, message))
}
func anthropicErrorBody(status int, message string) gin.H {
	t := "invalid_request_error"
	switch {
	case status >= 500:
		t = "api_error"
	case status == 401:
		t = "authentication_error"
	case status == 403:
		t = "permission_error"
	case status == 404:
		t = "not_found_error"
	case status == 413:
		t = "request_too_large"
	case status == 429:
		t = "rate_limit_error"
	}
	return gin.H{"type": "error", "error": gin.H{"type": t, "message": message}}
}

// decodeMessages converts a Messages API request. max_tokens is required and
// caps the output; tools get the same notice as Chat Completions.
func decodeMessages(data []byte) (chatInput, error) {
	in := chatInput{N: 1}
	r := &in.Request
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil || raw == nil {
		return in, errors.New("request must be a JSON object")
	}
	for k, v := range raw {
		switch {
		case ignoredMessagesParams[k] || string(v) == "null":
		case k == "model", k == "messages", k == "max_tokens", k == "system", k == "stream", k == "tools", k == "tool_choice":
		default:
			return in, fmt.Errorf("unsupported parameter: %s", k)
		}
	}
	var v struct {
		Model     string          `json:"model"`
		MaxTokens int             `json:"max_tokens"`
		Stream    bool            `json:"stream"`
		System    json.RawMessage `json:"system"`
		Messages  []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Type string `json:"type"`
		} `json:"tools"`
		ToolChoice *struct {
			Type string `json:"type"`
		} `json:"tool_choice"`
	}
	if e := json.Unmarshal(data, &v); e != nil {
		return in, fmt.Errorf("invalid request: %v", e)
	}
	if v.Model == "" {
		return in, errors.New("model is required")
	}
	if v.MaxTokens < 1 {
		return in, errors.New("max_tokens must be a positive integer")
	}
	r.Model, r.MaxOutputTokens, in.Stream = v.Model, v.MaxTokens, v.Stream
	for _, t := range v.Tools {
		if t.Type != "" && t.Type != "custom" {
			return in, errors.New("only client tools are supported")
		}
	}
	if v.ToolChoice != nil && v.ToolChoice.Type != "auto" && v.ToolChoice.Type != "none" {
		return in, errors.New("tool_choice must be auto or none; tools cannot be forced")
	}
	if len(v.Tools) > 0 {
		r.Messages = append(r.Messages, textMessage("system", toolsUnavailable))
	}
	if len(v.System) > 0 && string(v.System) != "null" {
		parts, e := decodeBlocks(v.System)
		if e != nil || countImages(domain.Message{Content: parts}) > 0 {
			return in, errors.New("system must be text")
		}
		if len(parts) > 0 {
			r.Messages = append(r.Messages, domain.Message{Role: "system", Content: parts})
		}
	}
	images := 0
	for _, m := range v.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			return in, errors.New("unsupported message role")
		}
		parts, e := decodeBlocks(m.Content)
		if e != nil {
			return in, e
		}
		if len(parts) > 0 {
			images += countImages(domain.Message{Content: parts})
			r.Messages = append(r.Messages, domain.Message{Role: m.Role, Content: parts})
		}
	}
	if len(v.Messages) == 0 || len(r.Messages) > 1024 || images > 8 {
		return in, errors.New("invalid message or image count")
	}
	return in, nil
}

// decodeBlocks reads string content or Messages API content blocks. Tool use
// history is flattened to text and thinking blocks are dropped.
func decodeBlocks(raw json.RawMessage) ([]domain.ContentPart, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []domain.ContentPart{{Type: "text", Text: text}}, nil
	}
	var blocks []struct {
		Type   string `json:"type"`
		Text   string `json:"text"`
		Source *struct {
			Type      string `json:"type"`
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
			URL       string `json:"url"`
		} `json:"source"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return nil, errors.New("content must be text or content blocks")
	}
	out := []domain.ContentPart{}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			out = append(out, domain.ContentPart{Type: "text", Text: b.Text})
		case "image":
			switch {
			case b.Source != nil && b.Source.Type == "base64" && b.Source.Data != "" && strings.HasPrefix(b.Source.MediaType, "image/"):
				out = append(out, domain.ContentPart{Type: "image_url", ImageURL: "data:" + b.Source.MediaType + ";base64," + b.Source.Data})
			case b.Source != nil && b.Source.Type == "url" && b.Source.URL != "":
				out = append(out, domain.ContentPart{Type: "image_url", ImageURL: b.Source.URL})
			default:
				return nil, errors.New("image source must be base64 or url")
			}
		case "tool_use":
			out = append(out, domain.ContentPart{Type: "text", Text: toolCall(b.Name, b.ID, string(b.Input))})
		case "tool_result":
			out = append(out, toolResult(b.ToolUseID))
			if len(b.Content) > 0 && string(b.Content) != "null" {
				parts, e := decodeBlocks(b.Content)
				if e != nil {
					return nil, e
				}
				out = append(out, parts...)
			}
		case "thinking", "redacted_thinking":
		default:
			return nil, fmt.Errorf("unsupported content block: %s", b.Type)
		}
	}
	return out, nil
}

// messages serves POST /v1/messages for channel chat routes.
func (h *Handler) messages(c *gin.Context, route model.Route, data []byte) {
	in, e := decodeMessages(data)
	if e != nil {
		anthropicError(c, 400, e.Error())
		return
	}
	key, ok := keyID(c)
	if !ok {
		anthropicError(c, 401, "API key required")
		return
	}
	ctx := c.Request.Context()
	out := &sse{c: c}
	id := "msg_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	message := func(content []any, stop any, usage gin.H) gin.H {
		return gin.H{"id": id, "type": "message", "role": "assistant", "model": in.Request.Model, "content": content, "stop_reason": stop, "stop_sequence": nil, "usage": usage}
	}
	emit := func(name string, v gin.H) error {
		if !in.Stream {
			return nil
		}
		v["type"] = name
		return out.send(name, v)
	}
	begun := false
	begin := func() error {
		if begun || !in.Stream {
			return nil
		}
		begun = true
		return emit("message_start", gin.H{"message": message([]any{}, nil, gin.H{"input_tokens": h.service.EstimateChatInput(ctx, route, in.Request), "output_tokens": 0})})
	}
	content := []any{}
	parts := blocks{
		open: func(b *block) error {
			if e := begin(); e != nil {
				return e
			}
			start := gin.H{"type": "text", "text": ""}
			if b.Reasoning {
				start = gin.H{"type": "thinking", "thinking": "", "signature": ""}
			}
			return emit("content_block_start", gin.H{"index": b.Index, "content_block": start})
		},
		delta: func(b *block, text string) error {
			d := gin.H{"type": "text_delta", "text": text}
			if b.Reasoning {
				d = gin.H{"type": "thinking_delta", "thinking": text}
			}
			return emit("content_block_delta", gin.H{"index": b.Index, "delta": d})
		},
		close: func(b *block) error {
			if b.Reasoning {
				content = append(content, gin.H{"type": "thinking", "thinking": b.Text, "signature": ""})
			} else {
				content = append(content, gin.H{"type": "text", "text": b.Text})
			}
			return emit("content_block_stop", gin.H{"index": b.Index})
		},
	}
	result, e := h.service.Chat(ctx, key, route, in.Request, in.Stream, func(ev domain.ChatEvent) error {
		if ev.Done {
			return nil
		}
		if e := parts.add(true, ev.Reasoning); e != nil {
			return e
		}
		return parts.add(false, ev.Text)
	})
	if e != nil {
		f := h.chatFailure(route, e)
		if !out.started {
			anthropicError(c, f.status, f.message)
			return
		}
		_ = out.send("error", anthropicErrorBody(f.status, f.message))
		return
	}
	if begin() != nil || parts.finish() != nil {
		return
	}
	stop := "end_turn"
	if result.Truncated {
		stop = "max_tokens"
	}
	usage := gin.H{"input_tokens": result.InputTokens, "output_tokens": result.OutputTokens}
	if !in.Stream {
		c.JSON(200, message(content, stop, usage))
		return
	}
	if emit("message_delta", gin.H{"delta": gin.H{"stop_reason": stop, "stop_sequence": nil}, "usage": usage}) == nil {
		_ = emit("message_stop", gin.H{})
	}
}
