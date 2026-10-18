// Protocol portions adapted from SkyKoo/gptGrok2api at 15773099641575776025e21d1290b5209b058c5a. See LICENSE.reference.
package chatgpt

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// OpenAIChat implements the authenticated ChatGPT Web conversation endpoint
// for the OpenAI JWT account pool. It intentionally shares the Sentinel and
// browser transport with OpenAIImage.
type OpenAIChat struct {
	Image *OpenAIImage
}

func NewOpenAIChat(image *OpenAIImage) *OpenAIChat {
	return &OpenAIChat{Image: image}
}

type OpenAIChatEvent struct {
	Text     string
	Thinking string
	Done     bool
}

// OpenAIChatImage binds decoded client image bytes to their original message
// position. Uploaded file IDs are deliberately scoped to one account attempt.
type OpenAIChatImage struct {
	MessageIndex int
	PartIndex    int
	Input        OpenAIImageInput
}

func (c *OpenAIChat) Stream(ctx context.Context, account wireAccount, request wireChatRequest, onEvent func(OpenAIChatEvent) error, images ...OpenAIChatImage) error {
	if c == nil || c.Image == nil {
		return fmt.Errorf("OpenAI chat provider is not configured")
	}
	ctx = c.Image.withTimezone(ctx, account)
	inputs := make([]OpenAIImageInput, len(images))
	for index, image := range images {
		inputs[index] = image.Input
	}
	references, err := c.Image.uploadInputs(ctx, account, inputs)
	if err != nil {
		return err
	}
	payload, err := openAIChatPayloadWithImages(request, images, references)
	if err != nil {
		return err
	}
	applyOpenAITimezone(ctx, payload)
	scripts, build, err := c.Image.bootstrap(ctx, account)
	if err != nil {
		return err
	}
	requirements, err := c.Image.chatRequirements(ctx, account, scripts, build)
	if err != nil {
		return err
	}
	headers := c.Image.requirementHeaders(requirements)
	headers["Accept"] = "text/event-stream"
	markSubmitted(ctx)
	response, err := c.Image.do(ctx, "POST", "/backend-api/conversation", account, payload, headers, true)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return readOpenAIHTTPError(response, "/backend-api/conversation")
	}
	return scanOpenAIChat(response.Body, onEvent, func(v any) { notifyUpstreamModel(ctx, v) })
}

func scanOpenAIChat(reader io.Reader, onEvent func(OpenAIChatEvent) error, observers ...func(any)) error {
	state := openAIChatState{}
	ended, hasText := false, false
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "event:") {
			continue
		}
		if strings.HasPrefix(line, "data:") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
		if line == "[DONE]" {
			if !hasText {
				return fmt.Errorf("ChatGPT returned no assistant text")
			}
			return onEvent(OpenAIChatEvent{Done: true})
		}
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var value any
		if err := json.Unmarshal([]byte(line), &value); err != nil {
			return fmt.Errorf("invalid ChatGPT stream event: %w", err)
		}
		for _, observe := range observers {
			observe(value)
		}
		event, eventErr := state.event(value)
		if eventErr != nil {
			return eventErr
		}
		hasText = hasText || event.Text != ""
		if event.Text != "" {
			ended = event.Done
		} else {
			ended = ended || event.Done
		}
		if event.Text != "" || event.Thinking != "" {
			if err := onEvent(event); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if !ended || !hasText {
		return fmt.Errorf("ChatGPT stream ended before completion: %w", io.ErrUnexpectedEOF)
	}
	return onEvent(OpenAIChatEvent{Done: true})
}

func openAIChatPayloadWithImages(request wireChatRequest, images []OpenAIChatImage, references []openAIImageReference) (map[string]any, error) {
	if len(images) != len(references) {
		return nil, fmt.Errorf("chat image upload count does not match input")
	}
	refs := make(map[[2]int]openAIImageReference, len(images))
	for index, image := range images {
		refs[[2]int{image.MessageIndex, image.PartIndex}] = references[index]
	}
	payload := openAIChatPayload(request)
	messages := make([]any, 0, len(request.Messages))
	for messageIndex, message := range request.Messages {
		parts := []any{}
		attachments := []any{}
		switch content := message.Content.(type) {
		case string:
			if content != "" {
				parts = append(parts, content)
			}
		case []any:
			for partIndex, raw := range content {
				part, ok := raw.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("invalid chat content part")
				}
				switch part["type"] {
				case "text":
					parts = append(parts, stringValue(part["text"]))
				case "image_url":
					ref, ok := refs[[2]int{messageIndex, partIndex}]
					if !ok {
						return nil, fmt.Errorf("chat image has no uploaded reference")
					}
					parts = append(parts, map[string]any{"content_type": "image_asset_pointer", "asset_pointer": "file-service://" + ref.FileID, "width": ref.Width, "height": ref.Height, "size_bytes": ref.FileSize})
					attachments = append(attachments, map[string]any{"id": ref.FileID, "mimeType": ref.MIME, "name": ref.FileName, "size": ref.FileSize, "width": ref.Width, "height": ref.Height})
				default:
					return nil, fmt.Errorf("unsupported chat content part")
				}
			}
		}
		if len(parts) == 0 {
			continue
		}
		contentType := "text"
		metadata := map[string]any{}
		if len(attachments) > 0 {
			contentType = "multimodal_text"
			metadata["attachments"] = attachments
		}
		role := strings.ToLower(strings.TrimSpace(message.Role))
		if role == "developer" {
			role = "system"
		}
		messages = append(messages, map[string]any{"id": firstUUID(), "author": map[string]any{"role": role}, "create_time": float64(time.Now().UnixNano()) / 1e9, "content": map[string]any{"content_type": contentType, "parts": parts}, "metadata": metadata})
	}
	payload["messages"] = messages
	return payload, nil
}

type openAIChatState struct {
	text    string
	summary string
	path    string
	hidden  bool
}

func (s *openAIChatState) event(value any) (OpenAIChatEvent, error) {
	event := OpenAIChatEvent{}
	if object, ok := value.(map[string]any); ok {
		if detail, ok := object["detail"].(string); ok {
			return OpenAIChatEvent{}, &upstreamError{
				Status:  400,
				Message: detail,
				Body:    detail,
			}
		}
		if rawError, ok := object["error"].(map[string]any); ok {
			status := 502
			if code := strings.ToLower(firstStringValue(rawError, "code", "type")); strings.Contains(code, "rate") {
				status = 429
			}
			return OpenAIChatEvent{}, &upstreamError{
				Status:  status,
				Message: firstStringValue(rawError, "message", "error"),
				Body:    stringValue(rawError["message"]),
			}
		}
		if path, ok := object["p"].(string); ok {
			s.path = path
		}
		for _, raw := range []any{object, object["v"]} {
			envelope, _ := raw.(map[string]any)
			message, ok := envelope["message"].(map[string]any)
			if !ok {
				continue
			}
			author, _ := message["author"].(map[string]any)
			role, channel := stringValue(author["role"]), stringValue(message["channel"])
			// Only explicit display summaries are exposed. Raw analysis/thought
			// content is neither emitted nor counted as client output.
			content, _ := message["content"].(map[string]any)
			if role == "assistant" && stringValue(content["content_type"]) == "thoughts" {
				var summary strings.Builder
				for _, raw := range toAnyList(content["thoughts"]) {
					if thought, ok := raw.(map[string]any); ok {
						if text := stringValue(thought["summary"]); text != "" {
							if summary.Len() > 0 {
								summary.WriteString("\n")
							}
							summary.WriteString(text)
						}
					}
				}
				event.Thinking = appendDelta(&s.summary, summary.String())
			}
			s.hidden = (role != "" && role != "assistant") || (channel != "" && channel != "final")
			if message["status"] == "finished_successfully" && !s.hidden {
				event.Done = true
			}
		}
		if candidate := openAIAssistantText(object); candidate != "" && !s.hidden {
			event.Text = appendDelta(&s.text, candidate)
		} else if delta, ok := object["v"].(string); ok && object["o"] == "append" && !s.hidden && (s.path == "/message/content/parts/0" || s.path == "/message/content/text") {
			event.Text = delta
			s.text += delta
		}
		if object["is_complete"] == true || object["finished_successfully"] == true {
			event.Done = true
		}
	}
	return event, nil
}

func openAIChatPayload(request wireChatRequest) map[string]any {
	messages := make([]any, 0, len(request.Messages))
	for _, message := range request.Messages {
		text := openAIMessageText(message.Content)
		if text == "" {
			continue
		}
		role := strings.TrimSpace(message.Role)
		if role == "" {
			role = "user"
		}
		messages = append(messages, map[string]any{
			"id":          firstUUID(),
			"author":      map[string]any{"role": role},
			"create_time": float64(time.Now().UnixNano()) / 1e9,
			"content":     map[string]any{"content_type": "text", "parts": []any{text}},
		})
	}
	modelID := strings.TrimSpace(request.Model)
	if modelID == "auto" || modelID == "" {
		modelID = "auto"
	}
	payload := map[string]any{
		"action":                        "next",
		"messages":                      messages,
		"model":                         modelID,
		"parent_message_id":             firstUUID(),
		"conversation_mode":             map[string]any{"kind": "primary_assistant"},
		"conversation_origin":           nil,
		"force_paragen":                 false,
		"force_paragen_model_slug":      "",
		"force_rate_limit":              false,
		"force_use_sse":                 true,
		"history_and_training_disabled": true,
		"reset_rate_limits":             false,
		"suggestions":                   []any{},
		"supported_encodings":           []any{},
		"system_hints":                  []any{},
		"variant_purpose":               "comparison_implicit",
		"websocket_request_id":          firstUUID(),
		"client_contextual_info":        map[string]any{"is_dark_mode": false, "time_since_loaded": 120, "page_height": 900, "page_width": 1400, "pixel_ratio": 2, "screen_height": 1440, "screen_width": 2560},
	}
	if request.ReasoningEffort != "" && request.ReasoningEffort != "none" {
		payload["thinking_effort"] = request.ReasoningEffort
	}
	return payload
}

func openAIMessageText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, raw := range typed {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if stringValue(part["type"]) == "text" {
				parts = append(parts, stringValue(part["text"]))
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func openAIAssistantText(value map[string]any) string {
	for _, candidate := range []any{value, value["v"]} {
		object, ok := candidate.(map[string]any)
		if !ok {
			continue
		}
		message, ok := object["message"].(map[string]any)
		if !ok {
			continue
		}
		author, _ := message["author"].(map[string]any)
		if role := strings.ToLower(stringValue(author["role"])); role != "" && role != "assistant" {
			continue
		}
		if channel := stringValue(message["channel"]); channel != "" && channel != "final" {
			continue
		}
		content, _ := message["content"].(map[string]any)
		parts, _ := content["parts"].([]any)
		var text strings.Builder
		for _, part := range parts {
			if value, ok := part.(string); ok {
				text.WriteString(value)
			}
		}
		if text.Len() == 0 {
			if value, ok := content["text"].(string); ok {
				text.WriteString(value)
			}
		}
		if text.Len() > 0 {
			return text.String()
		}
	}
	return ""
}

func appendDelta(current *string, candidate string) string {
	if candidate == "" {
		return ""
	}
	if strings.HasPrefix(candidate, *current) {
		delta := strings.TrimPrefix(candidate, *current)
		*current = candidate
		return delta
	}
	if strings.HasPrefix(*current, candidate) {
		return ""
	}
	*current += candidate
	return candidate
}
