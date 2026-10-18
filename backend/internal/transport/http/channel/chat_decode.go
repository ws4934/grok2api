package channelhttp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	domain "github.com/chenyme/grok2api/backend/internal/domain/channel"
)

// chatInput is a decoded Chat Completions, Responses or Messages request.
type chatInput struct {
	Request      domain.ChatRequest
	Stream       bool
	IncludeUsage bool
	// N is validated per path: text replies have one choice, images allow 1–10.
	N int
	// Image is set when modalities asks for image output.
	Image bool
	// Tools, ToolChoice and ParallelToolCalls are echoed by Responses objects.
	Tools             []json.RawMessage
	ToolChoice        json.RawMessage
	ParallelToolCalls bool
}

// ignoredChatParams are accepted for client compatibility. They do not change
// the reply: the upstream exposes no sampling, stop or caching controls.
var ignoredChatParams = map[string]bool{
	"temperature": true, "top_p": true, "frequency_penalty": true, "presence_penalty": true,
	"seed": true, "user": true, "metadata": true, "stop": true, "include": true, "truncation": true,
	"prompt_cache_key": true, "service_tier": true, "safety_identifier": true,
}

// toolsUnavailable is prepended when a client declares tools: they cannot run
// here, so the model must answer in plain text.
const toolsUnavailable = "Tools are unavailable in this session: you cannot call functions, run commands or read files. Answer directly in plain text and never claim to have used a tool."

func decodeChat(data []byte, responses bool) (chatInput, error) {
	in := chatInput{N: 1, ParallelToolCalls: true}
	r := &in.Request
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil || raw == nil {
		return in, errors.New("request must be a JSON object")
	}
	structural := map[string]bool{"messages": !responses, "input": responses, "instructions": responses}
	for k, v := range raw {
		if ignoredChatParams[k] || structural[k] || string(v) == "null" {
			continue
		}
		if e := in.option(k, v); e != nil {
			return in, e
		}
	}
	if r.Model == "" {
		return in, errors.New("model is required")
	}
	effort, e := reasoningEffort(raw)
	if e != nil {
		return in, e
	}
	r.ReasoningEffort = effort
	var items []json.RawMessage
	if responses {
		var instructions string
		if v, ok := raw["instructions"]; ok && string(v) != "null" {
			if json.Unmarshal(v, &instructions) != nil {
				return in, errors.New("instructions must be text")
			}
			if instructions != "" {
				r.Messages = append(r.Messages, textMessage("system", instructions))
			}
		}
		var text string
		if string(raw["input"]) == "null" {
			return in, errors.New("input cannot be null")
		}
		if json.Unmarshal(raw["input"], &text) == nil {
			r.Messages = append(r.Messages, textMessage("user", text))
		} else if json.Unmarshal(raw["input"], &items) != nil {
			return in, errors.New("input must be text or messages")
		}
	} else if json.Unmarshal(raw["messages"], &items) != nil {
		return in, errors.New("messages must be an array")
	}
	images := 0
	for _, item := range items {
		m, ok, e := decodeItem(item)
		if e != nil {
			return in, e
		}
		if ok {
			images += countImages(m)
			r.Messages = append(r.Messages, m)
		}
	}
	if len(in.Tools) > 0 {
		r.Messages = append([]domain.Message{textMessage("system", toolsUnavailable)}, r.Messages...)
	}
	if len(r.Messages) == 0 || len(r.Messages) > 1024 || images > 8 {
		return in, errors.New("invalid message or image count")
	}
	return in, nil
}

// option validates one top-level parameter that is not structural or ignored.
func (in *chatInput) option(k string, v json.RawMessage) error {
	switch k {
	case "model":
		if json.Unmarshal(v, &in.Request.Model) != nil {
			return errors.New("model must be text")
		}
	case "stream":
		if json.Unmarshal(v, &in.Stream) != nil {
			return errors.New("stream must be boolean")
		}
	case "stream_options":
		var o struct {
			IncludeUsage       bool `json:"include_usage"`
			IncludeObfuscation bool `json:"include_obfuscation"`
		}
		if strictJSON(v, &o) != nil {
			return errors.New("unsupported stream_options")
		}
		in.IncludeUsage = o.IncludeUsage
	case "n":
		if json.Unmarshal(v, &in.N) != nil || in.N < 1 {
			return errors.New("n must be a positive integer")
		}
	case "store", "background", "logprobs":
		var b bool
		if json.Unmarshal(v, &b) != nil || b {
			return fmt.Errorf("%s must be false", k)
		}
	case "top_logprobs":
		var n int
		if json.Unmarshal(v, &n) != nil || n != 0 {
			return errors.New("top_logprobs is unsupported")
		}
	case "previous_response_id":
		return errors.New("previous_response_id is unsupported; send the full conversation")
	case "max_tokens", "max_completion_tokens", "max_output_tokens":
		var n int
		if json.Unmarshal(v, &n) != nil || n < 1 {
			return fmt.Errorf("%s must be a positive integer", k)
		}
		if in.Request.MaxOutputTokens == 0 || n < in.Request.MaxOutputTokens {
			in.Request.MaxOutputTokens = n
		}
	case "reasoning_effort", "thinking_effort", "reasoning":
		// Decoded by reasoningEffort, which applies the precedence.
	case "response_format":
		var o struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(v, &o) != nil || o.Type != "text" {
			return errors.New("only text response_format is supported")
		}
	case "text":
		var o struct {
			Format *struct {
				Type string `json:"type"`
			} `json:"format"`
		}
		if json.Unmarshal(v, &o) != nil || (o.Format != nil && o.Format.Type != "text") {
			return errors.New("only text output format is supported")
		}
	case "modalities":
		var m []string
		if json.Unmarshal(v, &m) != nil {
			return errors.New("modalities must be an array")
		}
		for _, x := range m {
			if x != "text" && x != "image" {
				return errors.New("only text and image modalities are supported")
			}
			in.Image = in.Image || x == "image"
		}
	case "tools":
		if json.Unmarshal(v, &in.Tools) != nil {
			return errors.New("tools must be an array")
		}
		for _, t := range in.Tools {
			var tool struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(t, &tool) != nil || tool.Type != "function" {
				return errors.New("only function tools are supported")
			}
		}
	case "tool_choice":
		var s string
		if json.Unmarshal(v, &s) != nil || (s != "auto" && s != "none") {
			return errors.New("tool_choice must be auto or none; tools cannot be forced")
		}
		in.ToolChoice = v
	case "parallel_tool_calls":
		if json.Unmarshal(v, &in.ParallelToolCalls) != nil {
			return errors.New("parallel_tool_calls must be boolean")
		}
	default:
		return fmt.Errorf("unsupported parameter: %s", k)
	}
	return nil
}

// reasoningEffort reads thinking_effort, then reasoning_effort, then
// reasoning.effort, and normalizes the value for the upstream.
func reasoningEffort(raw map[string]json.RawMessage) (string, error) {
	value, ok := raw["thinking_effort"]
	if !ok {
		value, ok = raw["reasoning_effort"]
	}
	if !ok {
		var reasoning struct {
			Effort json.RawMessage `json:"effort"`
		}
		if v, found := raw["reasoning"]; found && json.Unmarshal(v, &reasoning) != nil {
			return "", errors.New("reasoning must be an object")
		}
		value = reasoning.Effort
	}
	if len(value) == 0 || string(value) == "null" {
		return "", nil
	}
	var s string
	if json.Unmarshal(value, &s) != nil {
		return "", errors.New("reasoning effort must be text")
	}
	switch v := strings.ToLower(strings.TrimSpace(s)); v {
	case "", "none", "minimal":
		return "", nil
	case "low", "medium", "high":
		return v, nil
	case "xhigh", "extended":
		return "extended", nil
	}
	return "", fmt.Errorf("unsupported reasoning effort: %s", s)
}

// decodeItem converts one chat message or Responses input item. Tool history
// is flattened to text because tools never run here; reasoning items are
// dropped (ok is false).
func decodeItem(raw json.RawMessage) (domain.Message, bool, error) {
	var v struct {
		Type       string          `json:"type"`
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		ToolCallID string          `json:"tool_call_id"`
		ToolCalls  []struct {
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
		CallID    string          `json:"call_id"`
		Name      string          `json:"name"`
		Arguments string          `json:"arguments"`
		Output    json.RawMessage `json:"output"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return domain.Message{}, false, errors.New("invalid message")
	}
	switch v.Type {
	case "reasoning":
		return domain.Message{}, false, nil
	case "function_call":
		return textMessage("assistant", toolCall(v.Name, v.CallID, v.Arguments)), true, nil
	case "function_call_output":
		parts, e := decodeContent(v.Output)
		if e != nil {
			return domain.Message{}, false, e
		}
		return domain.Message{Role: "user", Content: append([]domain.ContentPart{toolResult(v.CallID)}, parts...)}, true, nil
	case "", "message":
	default:
		return domain.Message{}, false, errors.New("unsupported input item")
	}
	out := domain.Message{Role: v.Role}
	switch v.Role {
	case "developer":
		out.Role = "system"
	case "tool":
		out.Role = "user"
		out.Content = []domain.ContentPart{toolResult(v.ToolCallID)}
	case "system", "user", "assistant":
	default:
		return out, false, errors.New("unsupported message role")
	}
	if v.Role != "assistant" || len(v.ToolCalls) == 0 {
		v.ToolCalls = nil
	}
	// An assistant turn that only calls tools may omit its content.
	if c := string(v.Content); len(v.ToolCalls) == 0 || (c != "" && c != "null" && c != `""`) {
		parts, e := decodeContent(v.Content)
		if e != nil {
			return out, false, e
		}
		out.Content = append(out.Content, parts...)
	}
	for _, call := range v.ToolCalls {
		out.Content = append(out.Content, domain.ContentPart{Type: "text", Text: toolCall(call.Function.Name, call.ID, call.Function.Arguments)})
	}
	return out, true, nil
}

// decodeContent reads string content or OpenAI text and image parts.
func decodeContent(raw json.RawMessage) ([]domain.ContentPart, error) {
	var text string
	if len(raw) == 0 || string(raw) == "null" {
		return nil, errors.New("message content cannot be null")
	}
	if json.Unmarshal(raw, &text) == nil {
		return []domain.ContentPart{{Type: "text", Text: text}}, nil
	}
	var parts []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		ImageURL json.RawMessage `json:"image_url"`
		Detail   string          `json:"detail"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return nil, errors.New("invalid message content")
	}
	out := []domain.ContentPart{}
	for _, p := range parts {
		switch p.Type {
		case "text", "input_text", "output_text":
			out = append(out, domain.ContentPart{Type: "text", Text: p.Text})
		case "image_url", "input_image":
			var u string
			if json.Unmarshal(p.ImageURL, &u) != nil {
				var image struct {
					URL    string `json:"url"`
					Detail string `json:"detail"`
				}
				if json.Unmarshal(p.ImageURL, &image) != nil {
					return nil, errors.New("invalid image_url")
				}
				u, p.Detail = image.URL, image.Detail
			}
			if u == "" {
				return nil, errors.New("image_url is required")
			}
			if p.Detail != "" && p.Detail != "auto" && p.Detail != "low" && p.Detail != "high" {
				return nil, errors.New("image detail must be auto, low or high")
			}
			out = append(out, domain.ContentPart{Type: "image_url", ImageURL: u})
		default:
			return nil, errors.New("unsupported content part")
		}
	}
	if len(out) == 0 {
		return nil, errors.New("message content is empty")
	}
	return out, nil
}

func textMessage(role, text string) domain.Message {
	return domain.Message{Role: role, Content: []domain.ContentPart{{Type: "text", Text: text}}}
}
func toolCall(name, id, arguments string) string {
	return fmt.Sprintf("Tool call %s (%s): %s", name, id, arguments)
}
func toolResult(id string) domain.ContentPart {
	return domain.ContentPart{Type: "text", Text: fmt.Sprintf("Tool result (%s):", id)}
}
func countImages(m domain.Message) int {
	n := 0
	for _, p := range m.Content {
		if p.Type == "image_url" {
			n++
		}
	}
	return n
}
