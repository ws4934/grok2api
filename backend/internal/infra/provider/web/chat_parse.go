package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/toolemu"
)

func consumeUpstream(source io.Reader, emit func(string, string) error) (parsedChat, error) {
	return consumeUpstreamWithCitations(source, emit, true)
}

func consumeUpstreamWithCitations(source io.Reader, emit func(string, string) error, inlineCitations bool) (parsedChat, error) {
	parsed := parsedChat{DisableInlineCitations: !inlineCitations}
	err := consumeUpstreamInto(source, &parsed, emit)
	return parsed, err
}

func consumeUpstreamInto(source io.Reader, parsed *parsedChat, emit func(string, string) error) error {
	return consumeJSONObjects(source, 8<<20, func(data []byte) error {
		kind, delta, err := parseUpstreamFrame(data, parsed)
		if err != nil {
			return err
		}
		if emit == nil {
			return nil
		}
		// Always invoke emit so streaming can flush tool/citation side-channels
		// even when the frame produced no visible text delta.
		return emit(kind, delta)
	})
}

// webStreamReadError marks a failure to read the upstream stream itself, the
// only stream failure that says anything about the egress node.
type webStreamReadError struct{ error }

func (e webStreamReadError) Unwrap() error { return e.error }

func consumeJSONObjects(source io.Reader, maxObjectBytes int, consume func([]byte) error) error {
	reader := bufio.NewReaderSize(source, 64<<10)
	frame := make([]byte, 0, 64<<10)
	depth := 0
	inString := false
	escaped := false
	for {
		value, err := reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if depth != 0 {
					return webStreamReadError{io.ErrUnexpectedEOF}
				}
				return nil
			}
			return webStreamReadError{err}
		}
		if depth == 0 {
			if value != '{' {
				continue
			}
			frame = frame[:0]
			depth = 1
			inString = false
			escaped = false
			frame = append(frame, value)
			continue
		}
		frame = append(frame, value)
		if len(frame) > maxObjectBytes {
			return fmt.Errorf("Grok Web 单个响应帧超过 %d MiB", maxObjectBytes>>20)
		}
		if inString {
			if escaped {
				escaped = false
			} else if value == '\\' {
				escaped = true
			} else if value == '"' {
				inString = false
			}
			continue
		}
		switch value {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				if err := consume(frame); err != nil {
					return err
				}
			}
		}
	}
}

func parseUpstreamFrame(data []byte, parsed *parsedChat) (string, string, error) {
	var root map[string]any
	if json.Unmarshal(data, &root) != nil {
		return "", "", nil
	}
	if event, ok := root["event"].(map[string]any); ok {
		return parseGatewayEvent(event, parsed)
	}
	if errorValue, ok := root["error"].(map[string]any); ok {
		return "", "", webResponseError(errorValue)
	}
	result, _ := root["result"].(map[string]any)
	if conversation, _ := result["conversation"].(map[string]any); conversation != nil {
		parsed.ConversationID, _ = conversation["conversationId"].(string)
		return "", "", nil
	}
	response, _ := result["response"].(map[string]any)
	if response == nil {
		return "", "", nil
	}
	if errorValue, ok := response["error"].(map[string]any); ok {
		return "", "", webResponseError(errorValue)
	}
	for _, key := range []string{"cardAttachment", "cardAttachments"} {
		if rawURL := collectCardAttachment(parsed, response[key]); rawURL != "" {
			rawURL = absoluteAssetURL(rawURL)
			parsed.Images = appendUniqueString(parsed.Images, rawURL)
			return "image", rawURL, nil
		}
	}
	if userResponse, _ := response["userResponse"].(map[string]any); userResponse != nil {
		if id, _ := userResponse["responseId"].(string); id != "" {
			parsed.ParentID = id
		}
	}
	collectSearchSources(parsed, response)
	token, _ := response["token"].(string)
	thinking, _ := response["isThinking"].(bool)
	tag, _ := response["messageTag"].(string)
	if tag == "tool_usage_card" {
		collectServerTool(parsed, response)
		// tool_usage_card 的 token 是 Grok 内部 XML 协议，不属于模型 reasoning。
		return "", "", nil
	}
	if token != "" && thinking {
		parsed.Reasoning.WriteString(token)
		return "reasoning", token, nil
	}
	if token != "" && !thinking && (tag == "final" || tag == "") {
		parsed.upstreamText.WriteString(token)
		cleaned := cleanChatToken(parsed, token)
		parsed.appendText(cleaned)
		return "text", cleaned, nil
	}
	if modelResponse, _ := response["modelResponse"].(map[string]any); modelResponse != nil {
		return collectModelResponse(parsed, modelResponse)
	}
	if imageResponse, _ := response["streamingImageGenerationResponse"].(map[string]any); imageResponse != nil {
		rawURL, _ := imageResponse["imageUrl"].(string)
		if rawURL == "" {
			rawURL, _ = imageResponse["url"].(string)
		}
		if rawURL != "" {
			moderated, _ := imageResponse["moderated"].(bool)
			if moderated {
				markModeratedImage(parsed, rawURL)
				return "", "", nil
			}
			completed, _ := imageResponse["isFinal"].(bool)
			if completed || imageResponse["progress"] == float64(100) {
				rawURL = absoluteAssetURL(rawURL)
				parsed.Images = appendUniqueString(parsed.Images, rawURL)
				return "image", rawURL, nil
			}
		}
	}
	return "", "", nil
}

func collectModelResponse(parsed *parsedChat, modelResponse map[string]any) (string, string, error) {
	if err := modelResponseStreamError(modelResponse); err != nil {
		return "", "", err
	}
	if parsed.ParentID == "" {
		parsed.ParentID, _ = modelResponse["parentResponseId"].(string)
	}
	collectSearchSources(parsed, modelResponse)
	firstImage := collectModelResponseImages(parsed, modelResponse)
	message, _ := modelResponse["message"].(string)
	if delta := mergeModelResponseText(parsed, message); delta != "" {
		return "text", delta, nil
	}
	if firstImage != "" {
		return "image", firstImage, nil
	}
	return "", "", nil
}

func mergeModelResponseText(parsed *parsedChat, message string) string {
	if message == "" {
		return ""
	}
	raw := parsed.upstreamText.String()
	if raw == message || strings.HasPrefix(raw, message) {
		return ""
	}
	if raw != "" && !strings.HasPrefix(message, raw) {
		// 已输出内容与最终 envelope 不同，保留已输出结果，避免重复或回滚流式内容。
		return ""
	}
	delta := message[len(raw):]
	parsed.upstreamText.WriteString(delta)
	delta = cleanChatToken(parsed, delta)
	parsed.appendText(delta)
	return delta
}

func modelResponseStreamError(modelResponse map[string]any) error {
	values, _ := modelResponse["streamErrors"].([]any)
	for _, raw := range values {
		switch value := raw.(type) {
		case string:
			if message := strings.TrimSpace(value); message != "" {
				return errors.New(message)
			}
		case map[string]any:
			if nested, _ := value["error"].(map[string]any); nested != nil {
				return webResponseError(nested)
			}
			if message := firstString(value, "message", "error", "detail"); message != "" {
				return webResponseError(map[string]any{"message": message, "code": value["code"]})
			}
		}
	}
	return nil
}

func webResponseError(value map[string]any) error {
	message, _ := value["message"].(string)
	if message == "" {
		message = "Grok Web stream error"
	}
	code, _ := numberAsInt(value["code"])
	if code == 7 || strings.Contains(strings.ToLower(message), "anti-bot") {
		return fmt.Errorf("%w: %s", errWebAntiBot, message)
	}
	normalized := strings.ToLower(message)
	if strings.Contains(normalized, "usage limit") || strings.Contains(normalized, "usage quota") {
		return fmt.Errorf("%w: %s", errWebUsageLimit, message)
	}
	return errors.New(message)
}

func antiBotProviderResponse() *provider.Response {
	return jsonProviderResponse(http.StatusForbidden, map[string]any{"error": map[string]any{
		"message": "Grok Web 出口会话被上游反机器人规则拒绝，请检查代理、User-Agent 与 Cloudflare Cookie 是否来自同一浏览器会话",
		"type":    "upstream_error", "code": "anti_bot_rejected",
	}})
}

func collectModelResponseImages(parsed *parsedChat, modelResponse map[string]any) string {
	first := ""
	appendImage := func(value string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		value = absoluteAssetURL(value)
		if _, moderated := parsed.moderatedImages[value]; moderated {
			return
		}
		if containsString(parsed.Images, value) {
			return
		}
		parsed.Images = append(parsed.Images, value)
		if first == "" {
			first = value
		}
	}
	if urls, ok := modelResponse["generatedImageUrls"].([]any); ok {
		for _, raw := range urls {
			value, _ := raw.(string)
			appendImage(value)
		}
	}
	if cards, ok := modelResponse["cardAttachmentsJson"].([]any); ok {
		for _, raw := range cards {
			encoded, _ := raw.(string)
			var card map[string]any
			if encoded == "" || json.Unmarshal([]byte(encoded), &card) != nil {
				continue
			}
			appendImage(imageURLFromCardData(card))
		}
	}
	return first
}

func markModeratedImage(parsed *parsedChat, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	if parsed.moderatedImages == nil {
		parsed.moderatedImages = make(map[string]struct{})
	}
	parsed.moderatedImages[absoluteAssetURL(value)] = struct{}{}
}

// applyParsedToolCalls takes the call blocks out of a whole reply with the
// Sieve that streaming uses, so every block is read as it would be streamed.
func applyParsedToolCalls(parsed *parsedChat, configuration toolemu.Config) {
	if len(configuration.Functions) == 0 || configuration.Choice == "none" {
		return
	}
	sieve := toolemu.NewSieve(configuration.Available)
	first := sieve.Feed(parsed.Text.String())
	rest := sieve.Flush() // takes the remaining blocks too
	calls := append(first.Calls, rest.Calls...)
	if len(calls) == 0 {
		return
	}
	text := first.SafeText
	if first.Complete && len(first.Calls) == 0 {
		text += first.Raw
	}
	parsed.resetText(strings.TrimSpace(text + rest.SafeText))
	parsed.ToolCalls = calls
}

func (a *Adapter) archiveChatImages(ctx context.Context, credential account.Credential, parsed *parsedChat) error {
	for _, rawURL := range parsed.Images {
		item, err := a.imageDataItem(ctx, credential, imagineImageValue{URL: rawURL}, "url")
		if err != nil {
			return err
		}
		if parsed.Text.Len() > 0 {
			parsed.appendText("\n\n")
		}
		parsed.appendText(liteImageMarkdown(item))
	}
	return nil
}

func appendUniqueString(values []string, value string) []string {
	if containsString(values, value) {
		return values
	}
	return append(values, value)
}

func containsString(values []string, value string) bool {
	for _, existing := range values {
		if existing == value {
			return true
		}
	}
	return false
}

func collectCardAttachment(parsed *parsedChat, value any) string {
	if values, ok := value.([]any); ok {
		first := ""
		for _, item := range values {
			if rawURL := collectCardAttachment(parsed, item); first == "" && rawURL != "" {
				first = rawURL
			}
		}
		return first
	}
	data := cardAttachmentData(value)
	if data == nil {
		return ""
	}
	if id, _ := data["id"].(string); id != "" {
		if parsed.cardCache == nil {
			parsed.cardCache = make(map[string]map[string]any)
		}
		parsed.cardCache[id] = data
	}
	return imageURLFromCardData(data)
}

func cardAttachmentData(value any) map[string]any {
	card, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	if raw, ok := card["jsonData"].(map[string]any); ok {
		return raw
	}
	if raw, _ := card["jsonData"].(string); raw != "" {
		var data map[string]any
		if json.Unmarshal([]byte(raw), &data) == nil {
			return data
		}
	}
	if card["image_chunk"] != nil || card["imageChunk"] != nil {
		return card
	}
	return nil
}

func imageURLFromCardData(data map[string]any) string {
	chunk, _ := data["image_chunk"].(map[string]any)
	if chunk == nil {
		chunk, _ = data["imageChunk"].(map[string]any)
	}
	if chunk == nil {
		return ""
	}
	moderated, _ := chunk["moderated"].(bool)
	progress, _ := numberAsInt(chunk["progress"])
	if moderated || progress < 100 {
		return ""
	}
	imageURL, _ := chunk["imageUrl"].(string)
	if imageURL == "" {
		imageURL, _ = chunk["image_url"].(string)
	}
	return imageURL
}
