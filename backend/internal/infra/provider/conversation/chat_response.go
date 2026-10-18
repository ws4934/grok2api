package conversation

import "strings"

func chatResponse(value parsedResponse, openAIUsage bool) map[string]any {
	message := map[string]any{"role": "assistant", "content": value.Text}
	if value.Reasoning != "" {
		message["reasoning_content"] = value.Reasoning
	}
	finishReason := "stop"
	if len(value.Calls) > 0 {
		finishReason = "tool_calls"
		if value.Text == "" {
			message["content"] = nil
		}
		calls := make([]any, 0, len(value.Calls))
		for _, call := range value.Calls {
			calls = append(calls, map[string]any{
				"id": call.CallID, "type": "function",
				"function": map[string]any{"name": call.Name, "arguments": call.Arguments},
			})
		}
		message["tool_calls"] = calls
	} else if value.Refusal != "" {
		finishReason = "content_filter"
	} else if value.Status == "incomplete" {
		finishReason = "length"
	}
	if value.Refusal != "" {
		message["refusal"] = value.Refusal
	}
	if len(value.Annotations) > 0 {
		annotations := make([]any, 0, len(value.Annotations))
		for _, annotation := range value.Annotations {
			annotations = append(annotations, chatAnnotation(annotation))
		}
		message["annotations"] = annotations
	}
	id := strings.Replace(value.ID, "resp_", "chatcmpl_", 1)
	return map[string]any{
		"id": id, "object": "chat.completion", "created": value.CreatedAt, "model": value.Model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finishReason}},
		"usage":   chatUsage(value.Usage, openAIUsage),
	}
}

// chatAnnotation nests a flat Responses url_citation the way Chat Completions
// expects: {"type":"url_citation","url_citation":{url,title,start_index,end_index}}.
// Already nested citations and other annotation types pass through unchanged.
func chatAnnotation(value any) any {
	annotation, ok := value.(map[string]any)
	if !ok || annotation["type"] != "url_citation" || annotation["url_citation"] != nil {
		return value
	}
	citation := make(map[string]any, 4)
	for _, key := range []string{"url", "title", "start_index", "end_index"} {
		if field, exists := annotation[key]; exists {
			citation[key] = field
		}
	}
	return map[string]any{"type": "url_citation", "url_citation": citation}
}

func chatUsage(value responseUsage, openAIUsage bool) map[string]any {
	total := value.TotalTokens
	if total == 0 {
		total = value.InputTokens + value.OutputTokens
	}
	return withoutXAIUsage(map[string]any{
		"prompt_tokens": value.InputTokens, "completion_tokens": value.OutputTokens,
		"total_tokens":               total,
		"prompt_tokens_details":      map[string]any{"cached_tokens": value.InputTokensDetails.CachedTokens},
		"completion_tokens_details":  map[string]any{"reasoning_tokens": value.OutputTokensDetails.ReasoningTokens},
		"cost_in_usd_ticks":          value.CostInUSDTicks,
		"num_sources_used":           value.NumSourcesUsed,
		"num_server_side_tools_used": value.NumServerSideToolsUsed,
		"context_details": map[string]any{
			"input_tokens": value.ContextDetails.InputTokens, "output_tokens": value.ContextDetails.OutputTokens,
		},
	}, openAIUsage)
}

// withoutXAIUsage removes the xAI usage extensions when openAIUsage is set.
func withoutXAIUsage(usage map[string]any, openAIUsage bool) map[string]any {
	if openAIUsage {
		for _, key := range []string{"cost_in_usd_ticks", "num_sources_used", "num_server_side_tools_used", "context_details"} {
			delete(usage, key)
		}
	}
	return usage
}
