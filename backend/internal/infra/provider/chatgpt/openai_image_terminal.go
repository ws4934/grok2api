// Protocol portions adapted from SkyKoo/gptGrok2api at 15773099641575776025e21d1290b5209b058c5a. See LICENSE.reference.
package chatgpt

import "strings"

// openAIImageTextOnlyError inspects a complete conversation snapshot, never an
// SSE fragment. A final assistant reply can be a request for input, not an image.
// Be conservative: a tool invocation in the current turn may still be rendering.
func openAIImageTextOnlyError(value any, secrets ...string) error {
	root, _ := value.(map[string]any)
	if generating, _ := root["is_generating"].(bool); generating {
		return nil
	}
	mapping, _ := root["mapping"].(map[string]any)
	id := stringValue(root["current_node"])
	node, _ := mapping[id].(map[string]any)
	message, _ := node["message"].(map[string]any)
	author, _ := message["author"].(map[string]any)
	content, _ := message["content"].(map[string]any)
	ended, _ := message["end_turn"].(bool)
	if id == "" || author["role"] != "assistant" || !ended || message["status"] != "finished_successfully" || content["content_type"] != "text" {
		return nil
	}
	if channel := stringValue(message["channel"]); channel != "" && channel != "final" {
		return nil
	}
	// Follow only the current branch, stopping at the input for this turn. Old
	// replies on other branches must not determine the current generation state.
	seen := make(map[string]bool)
	for id != "" && !seen[id] {
		seen[id] = true
		n, ok := mapping[id].(map[string]any)
		if !ok {
			return nil
		}
		m, _ := n["message"].(map[string]any)
		a, _ := m["author"].(map[string]any)
		if a["role"] == "user" {
			reason := openAIImageMessageText(message)
			if reason == "" {
				reason = "上游已结束回复，但未生成图片，请补充输入或调整描述后重试。"
			} else {
				reason = "上游未生成图片：" + reason
			}
			return openAIImageTerminalError(map[string]any{"error": reason}, secrets...)
		}
		if a["role"] == "tool" {
			return nil
		}
		if recipient := stringValue(m["recipient"]); recipient != "" && recipient != "all" {
			return nil
		}
		metadata, _ := m["metadata"].(map[string]any)
		// These structured markers are evidence of background work; do not infer
		// completion from friendly assistant text while that work is outstanding.
		for _, key := range []string{"async_task_id", "image_gen_async", "image_gen_task_id", "tool_calls"} {
			if marker, exists := metadata[key]; exists && marker != nil && marker != false && marker != "" {
				return nil
			}
		}
		if calls, ok := m["tool_calls"].([]any); ok && len(calls) > 0 {
			return nil
		}
		id = stringValue(n["parent"])
	}
	return nil // Incomplete/cyclic ancestry is not proof that generation ended.
}

func openAIImageMessageText(message map[string]any) string {
	content, _ := message["content"].(map[string]any)
	if kind := stringValue(content["content_type"]); kind != "text" && kind != "refusal" {
		return ""
	}
	var text []string
	if parts, ok := content["parts"].([]any); ok {
		for _, part := range parts {
			if value, ok := part.(string); ok && strings.TrimSpace(value) != "" {
				text = append(text, value)
			}
		}
	}
	for _, key := range []string{"text", "refusal"} {
		if value, ok := content[key].(string); ok && strings.TrimSpace(value) != "" {
			text = append(text, value)
		}
	}
	return strings.Join(text, "\n")
}

func openAIImageFailureMarker(value any) bool {
	switch strings.ToLower(strings.TrimSpace(stringValue(value))) {
	case "failed", "error", "cancelled", "canceled", "blocked", "moderated", "refused", "rejected", "finished_error", "refusal", "content_filter", "safety", "moderation":
		return true
	}
	return false
}
