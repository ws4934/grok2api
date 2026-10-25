// Protocol portions adapted from SkyKoo/gptGrok2api at 15773099641575776025e21d1290b5209b058c5a. See LICENSE.reference.
package chatgpt

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	// Search is the web search seen so far; valid during the callback only.
	Search *chatSearch
	// Conversation and Parent, set on the final Done event, are where a
	// follow-up message continues this conversation; empty when unknown.
	Conversation, Parent string
	// Lost, set on the final Done event, is the ChatGPT tool channel (such as
	// functions.exec) the reply sent a call to; client tools never run there.
	Lost string
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
	response, err := c.Image.postConversation(ctx, account, payload)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	// ChatGPT echoes the request's messages before the answer; their IDs
	// keep an earlier assistant turn from being read as the reply.
	sent := map[string]bool{}
	for _, raw := range payload["messages"].([]any) {
		message, _ := raw.(map[string]any)
		sent[stringValue(message["id"])] = true
	}
	return scanOpenAIChat(response.Body, sent, onEvent, func(v any) { notifyUpstreamModel(ctx, v) })
}

// postConversation submits a conversation payload with its Sentinel
// requirements and returns the open event stream; the request counts as
// submitted from here on.
func (o *OpenAIImage) postConversation(ctx context.Context, account wireAccount, payload map[string]any) (*http.Response, error) {
	ctx = o.withTimezone(ctx, account)
	applyOpenAITimezone(ctx, payload)
	scripts, build, err := o.bootstrap(ctx, account)
	if err != nil {
		return nil, err
	}
	requirements, err := o.chatRequirements(ctx, account, scripts, build)
	if err != nil {
		return nil, err
	}
	headers := o.requirementHeaders(requirements)
	headers["Accept"] = "text/event-stream"
	if err := markSubmitted(ctx); err != nil {
		return nil, err
	}
	response, err := o.do(ctx, "POST", "/backend-api/conversation", account, payload, headers, true)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		return nil, readOpenAIHTTPError(response, "/backend-api/conversation")
	}
	return response, nil
}

func scanOpenAIChat(reader io.Reader, inputs map[string]bool, emit func(OpenAIChatEvent) error, observers ...func(any)) error {
	state := openAIChatState{inputs: inputs}
	onEvent := func(event OpenAIChatEvent) error {
		event.Search = &state.search
		return emit(event)
	}
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
				// Fragments only: no merged answer message was streamed.
				text := state.fragmentText()
				if text == "" {
					return fmt.Errorf("ChatGPT returned no assistant text")
				}
				if err := onEvent(OpenAIChatEvent{Text: text}); err != nil {
					return err
				}
			}
			return onEvent(state.done())
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
	return onEvent(state.done())
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
	var lastMetadata map[string]any
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
		lastMetadata = metadata
	}
	if request.Search && lastMetadata != nil {
		lastMetadata["system_hints"] = []any{"search"}
	}
	payload["messages"] = messages
	return payload, nil
}

type openAIChatState struct {
	text    strings.Builder
	summary strings.Builder
	path    string
	hidden  bool
	// fragments keeps the text of answer fragments (is_message_fragment) by
	// message ID in arrival order; they are only used when no merged answer
	// message is streamed.
	fragments     map[string]string
	fragmentOrder []string
	search        chatSearch
	// inputs holds the IDs of the request's messages, which ChatGPT echoes.
	inputs map[string]bool
	// conversation, lastToken and answer locate the end of the turn: the
	// message that ended it and, when the answer streamed as fragments, the
	// merged answer, which ChatGPT then places after the last fragment.
	conversation, lastToken, answer string
	// lost is the ChatGPT tool channel a call of the reply went to.
	lost string
}

// done is the final event, carrying where a follow-up message continues.
func (s *openAIChatState) done() OpenAIChatEvent {
	parent := s.answer
	if parent == "" {
		parent = s.lastToken
	}
	return OpenAIChatEvent{Done: true, Conversation: s.conversation, Parent: parent, Lost: s.lost}
}

// displayableAssistantMessage reports whether a message is the user-visible
// answer. ChatGPT streams answer fragments (split around reasoning) beside a
// merged message carrying the whole answer, plus hidden and non-text messages
// (UI widgets, tool output); only the merged or plain text answer is shown.
func displayableAssistantMessage(message map[string]any) bool {
	author, _ := message["author"].(map[string]any)
	if role := strings.ToLower(stringValue(author["role"])); role != "" && role != "assistant" {
		return false
	}
	if channel := stringValue(message["channel"]); channel != "" && channel != "final" {
		return false
	}
	metadata, _ := message["metadata"].(map[string]any)
	if boolValue(metadata["is_message_fragment"]) || boolValue(metadata["is_visually_hidden_from_conversation"]) {
		return false
	}
	content, _ := message["content"].(map[string]any)
	switch stringValue(content["content_type"]) {
	case "", "text", "multimodal_text":
		return true
	default:
		return false
	}
}

// messageText joins the string parts of a message (or its text field).
func messageText(message map[string]any) string {
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
	return text.String()
}

// rememberFragment records the latest snapshot of an answer fragment.
func (s *openAIChatState) rememberFragment(message map[string]any) {
	metadata, _ := message["metadata"].(map[string]any)
	if !boolValue(metadata["is_message_fragment"]) {
		return
	}
	id, text := stringValue(message["id"]), messageText(message)
	if id == "" || text == "" {
		return
	}
	if s.fragments == nil {
		s.fragments = map[string]string{}
	}
	if _, seen := s.fragments[id]; !seen {
		s.fragmentOrder = append(s.fragmentOrder, id)
	}
	s.fragments[id] = text
}

// fragmentText joins the fragments for a stream that had no merged answer.
func (s *openAIChatState) fragmentText() string {
	var text strings.Builder
	for _, id := range s.fragmentOrder {
		text.WriteString(s.fragments[id])
	}
	return text.String()
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
			if code := strings.ToLower(aiString(rawError, "code", "type")); strings.Contains(code, "rate") {
				status = 429
			}
			return OpenAIChatEvent{}, &upstreamError{
				Status:  status,
				Message: aiString(rawError, "message", "error"),
				Body:    stringValue(rawError["message"]),
			}
		}
		if id := stringValue(object["conversation_id"]); id != "" {
			s.conversation = id
		}
		switch stringValue(object["type"]) {
		case "message_marker":
			if stringValue(object["marker"]) == "last_token" {
				s.lastToken = stringValue(object["message_id"])
			}
		case "streaming_parent_patch":
			if id := stringValue(object["logical_answer_id"]); id != "" {
				s.answer = id
			}
		case "server_ste_metadata":
			metadata, _ := object["metadata"].(map[string]any)
			if count, _ := metadata["search_tool_call_count"].(float64); count > 0 {
				s.search.count = int(count)
			}
		case "url_moderation":
			result, _ := object["url_moderation_result"].(map[string]any)
			if safe, ok := result["is_safe"].(bool); ok && !safe {
				s.search.block(stringValue(result["full_url"]))
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
			if s.inputs[stringValue(message["id"])] {
				s.hidden = true
				continue
			}
			author, _ := message["author"].(map[string]any)
			role := stringValue(author["role"])
			if recipient := stringValue(message["recipient"]); role == "assistant" && strings.HasPrefix(recipient, "functions.") {
				s.lost = recipient
			}
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
			s.rememberFragment(message)
			s.hidden = !displayableAssistantMessage(message)
			s.search.note(message, !s.hidden)
			if message["status"] == "finished_successfully" && !s.hidden {
				event.Done = true
			}
		}
		if candidate := openAIAssistantText(object); candidate != "" && !s.hidden {
			event.Text = appendDelta(&s.text, candidate)
		} else if delta, ok := object["v"].(string); ok && object["o"] == "append" && !s.hidden && (s.path == "/message/content/parts/0" || s.path == "/message/content/text") {
			event.Text = delta
			s.text.WriteString(delta)
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
	if request.ConversationID != "" {
		payload["conversation_id"], payload["parent_message_id"] = request.ConversationID, request.ParentMessageID
	}
	if request.ReasoningEffort != "" && request.ReasoningEffort != "none" {
		payload["thinking_effort"] = request.ReasoningEffort
	}
	// The Web composer's search tool sends the "search" hint, as its image
	// tool sends "picture_v2" (see OpenAIImage).
	if request.Search {
		payload["system_hints"] = []any{"search"}
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
		if !ok || !displayableAssistantMessage(message) {
			continue
		}
		if text := messageText(message); text != "" {
			return text
		}
	}
	return ""
}

func appendDelta(current *strings.Builder, candidate string) string {
	if candidate == "" {
		return ""
	}
	seen := current.String()
	if strings.HasPrefix(candidate, seen) {
		delta := candidate[len(seen):]
		current.WriteString(delta)
		return delta
	}
	if strings.HasPrefix(seen, candidate) {
		return ""
	}
	current.WriteString(candidate)
	return candidate
}
