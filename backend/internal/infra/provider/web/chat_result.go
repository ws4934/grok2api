package web

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/toolemu"
)

func buildOpenAIResult(operation, responseID, model string, parsed parsedChat, streaming bool, responseOptions ...conversation.ResponseOptions) map[string]any {
	created := time.Now().Unix()
	options := conversation.ResponseOptions{}
	if len(responseOptions) > 0 {
		options = responseOptions[0]
	}
	inputTokens := parsed.InputTokens
	outputTokens := estimateTokens(parsed.Text.String()) + estimateTokens(parsed.Reasoning.String()) + estimateToolCallTokens(parsed.ToolCalls)
	if operation == "chat" {
		finalizeXAIAnnotations(&parsed)
		message := map[string]any{"role": "assistant", "content": parsed.Text.String(), "reasoning_content": parsed.Reasoning.String()}
		if len(parsed.Annotations) > 0 {
			message["annotations"] = chatAnnotations(parsed.Annotations)
		}
		finishReason := "stop"
		if len(parsed.ToolCalls) > 0 {
			finishReason = "tool_calls"
			if parsed.Text.Len() == 0 {
				message["content"] = nil
			}
			message["tool_calls"] = chatToolCalls(parsed.ToolCalls)
		}
		value := map[string]any{
			"id": strings.Replace(responseID, "resp_", "chatcmpl_", 1), "object": "chat.completion", "created": created, "model": model,
			"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finishReason}},
			"usage":   map[string]any{"prompt_tokens": inputTokens, "completion_tokens": outputTokens, "total_tokens": inputTokens + outputTokens},
		}
		// xAI: top-level citations = all source URLs encountered (always when present).
		if citations := xaiCitationURLs(parsed); len(citations) > 0 {
			value["citations"] = citations
		}
		if usage := xaiServerSideToolUsage(parsed); usage != nil {
			value["server_side_tool_usage"] = usage
		}
		return value
	}
	if operation == conversation.OperationMessages {
		visibleText, stopSequence := applyWebStopSequences(parsed.Text.String(), options.StopSequences)
		emitWebSearch := shouldEmitWebMessagesSearch(parsed, options)
		// Zero initial capacity: tool/search counts come from untrusted upstream.
		content := make([]any, 0)
		if options.AnthropicThinking && parsed.Reasoning.Len() > 0 {
			content = append(content, map[string]any{"type": "thinking", "thinking": parsed.Reasoning.String()})
		}
		if emitWebSearch {
			content = append(content, webMessagesSearchBlocks(newWebID("srvtoolu"), parsed, options)...)
		}
		if visibleText != "" || len(parsed.ToolCalls) == 0 {
			content = append(content, map[string]any{"type": "text", "text": visibleText})
		}
		for _, call := range parsed.ToolCalls {
			var input any = map[string]any{}
			if json.Unmarshal([]byte(call.Arguments), &input) != nil {
				input = map[string]any{}
			}
			content = append(content, map[string]any{"type": "tool_use", "id": webAnthropicToolID(call.ID), "name": call.Name, "input": input})
		}
		stopReason := "end_turn"
		if len(parsed.ToolCalls) > 0 {
			stopReason = "tool_use"
		} else if stopSequence != "" {
			stopReason = "stop_sequence"
		}
		usage := map[string]any{"input_tokens": inputTokens, "output_tokens": outputTokens, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0}
		if emitWebSearch {
			usage["server_tool_use"] = map[string]any{"web_search_requests": webMessagesSearchRequests(parsed)}
		}
		return map[string]any{
			"id": strings.Replace(responseID, "resp_", "msg_", 1), "type": "message", "role": "assistant", "model": model,
			"content": content, "stop_reason": stopReason, "stop_sequence": nullableWebString(stopSequence),
			"usage": usage,
		}
	}
	finalizeXAIAnnotations(&parsed)
	output := parsed.ResponseOutput
	if output == nil {
		output = make([]any, 0, 2)
		if parsed.Reasoning.Len() > 0 {
			output = append(output, map[string]any{"id": newWebID("rs"), "type": "reasoning", "status": "completed", "summary": []any{map[string]any{"type": "summary_text", "text": parsed.Reasoning.String()}}})
		}
		// xAI Tool Usage Details: web_search_call / x_search_call precede the assistant message.
		output = append(output, xaiHostedSearchOutputItems(parsed)...)
		if parsed.Text.Len() > 0 || len(parsed.ToolCalls) == 0 {
			annotations := responsesAnnotations(parsed.Annotations)
			if annotations == nil {
				annotations = []any{}
			}
			message := map[string]any{"id": newWebID("msg"), "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": parsed.Text.String(), "annotations": annotations, "logprobs": []any{}}}}
			output = append(output, message)
		}
		for _, call := range parsed.ToolCalls {
			output = append(output, map[string]any{
				"id": newWebID("fc"), "type": "function_call", "status": "completed",
				"call_id": call.ID, "name": call.Name, "arguments": call.Arguments,
			})
		}
	}
	tools := parsed.Tools
	if tools == nil {
		tools = []any{}
	}
	toolChoice := parsed.ToolChoice
	if toolChoice == nil {
		toolChoice = "auto"
	}
	value := map[string]any{
		"id": responseID, "object": "response", "created_at": created, "completed_at": created, "status": "completed", "model": model,
		"output": output, "parallel_tool_calls": parsed.ParallelTools, "tools": tools, "tool_choice": toolChoice, "store": true,
		"usage": map[string]any{
			"input_tokens": inputTokens, "output_tokens": outputTokens, "total_tokens": inputTokens + outputTokens,
			"input_tokens_details":  map[string]any{"cached_tokens": 0},
			"output_tokens_details": map[string]any{"reasoning_tokens": estimateTokens(parsed.Reasoning.String())},
			"num_sources_used":      int64(len(parsed.SearchSources)), "num_server_side_tools_used": parsed.ServerTools,
		},
	}
	// xAI Citations docs: response.citations is the full URL list from tool research.
	if citations := xaiCitationURLs(parsed); len(citations) > 0 {
		value["citations"] = citations
	}
	if usage := xaiServerSideToolUsage(parsed); usage != nil {
		value["server_side_tool_usage"] = usage
	}
	return value
}

func chatToolCalls(calls []toolemu.Call) []any {
	values := make([]any, 0, len(calls))
	for _, call := range calls {
		values = append(values, map[string]any{
			"id": call.ID, "type": "function",
			"function": map[string]any{"name": call.Name, "arguments": call.Arguments},
		})
	}
	return values
}

func estimateToolCallTokens(calls []toolemu.Call) int64 {
	var total int64
	for _, call := range calls {
		total += estimateTokens(call.Name) + estimateTokens(call.Arguments)
	}
	return total
}
