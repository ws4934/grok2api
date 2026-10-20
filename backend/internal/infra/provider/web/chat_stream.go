package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/toolemu"
)

func (a *Adapter) streamOpenAIResponse(ctx context.Context, source io.ReadCloser, lease *infraegress.Lease, credential account.Credential, responseID, model, operation, prompt string, previous *inferencedomain.WebResponseState, tools toolemu.Config, parallelTools bool, options conversation.ResponseOptions) io.ReadCloser {
	reader, writer := io.Pipe()
	stream := provider.NewStreamPipeReadCloser(reader, source)
	go func() {
		defer stream.CloseSource()
		defer lease.Release()
		parsed := &parsedChat{
			ResponseID: responseID, InputTokens: estimateTokens(prompt), Tools: tools.ResponseTools,
			ToolChoice: tools.ResponseChoice, ParallelTools: parallelTools,
			DisableInlineCitations: !options.InlineCitationsEnabled(),
		}
		if previous != nil {
			parsed.ConversationID = previous.ConversationID
		}
		var clientText strings.Builder
		archivedImages := make(map[string]struct{})
		var sieve *toolemu.Sieve
		if len(tools.Functions) > 0 && tools.Choice != "none" {
			sieve = toolemu.NewSieve(tools.Available)
		}
		messagesStream := newWebMessagesStream(writer, responseID, model, parsed.InputTokens, options)
		visiblePhase := webVisibleStreamPhase{}
		annotationCursor := 0
		hostedSearchEmitted := make(map[string]struct{})
		var responsesStream *webResponsesStream
		if operation == conversation.OperationResponses {
			responsesStream = newWebResponsesStream(writer, responseID)
		}
		writeDelta := func(kind, delta string) error {
			if !visiblePhase.Allow(kind, delta) {
				return nil
			}
			if responsesStream != nil {
				return responsesStream.Delta(kind, delta)
			}
			return writeWebStreamDelta(writer, messagesStream, operation, responseID, model, kind, delta)
		}
		writeToolCalls := func(calls []toolemu.Call) error {
			if responsesStream != nil {
				return responsesStream.ToolCalls(calls)
			}
			return writeWebStreamToolCalls(writer, messagesStream, operation, responseID, model, calls)
		}
		flushAnnotations := func() error {
			if annotationCursor >= len(parsed.Annotations) {
				return nil
			}
			newOnes := parsed.Annotations[annotationCursor:]
			annotationCursor = len(parsed.Annotations)
			if responsesStream != nil {
				return responsesStream.Annotations(newOnes, annotationCursor-len(newOnes))
			}
			return writeStreamAnnotations(writer, operation, responseID, model, newOnes, annotationCursor-len(newOnes))
		}
		flushHostedSearch := func() error {
			if operation != conversation.OperationResponses {
				return nil
			}
			for _, call := range parsed.HostedSearchCalls {
				// Wait until the tool_result arrives (completed / has sources).
				if call.Status != "completed" && len(call.Sources) == 0 {
					continue
				}
				if _, emitted := hostedSearchEmitted[call.ID]; emitted {
					continue
				}
				if err := responsesStream.HostedSearch(call); err != nil {
					return err
				}
				hostedSearchEmitted[call.ID] = struct{}{}
			}
			return nil
		}
		flushSideChannel := func() error {
			if err := flushHostedSearch(); err != nil {
				return err
			}
			return flushAnnotations()
		}
		if operation != conversation.OperationMessages {
			writeStreamStart(writer, operation, responseID, model, parsed.InputTokens)
		}
		err := consumeUpstreamInto(source, parsed, func(kind, delta string) error {
			if len(parsed.ToolCalls) > 0 && kind != "reasoning" {
				return flushSideChannel()
			}
			if kind == "image" {
				rawURL := delta
				item, imageErr := a.imageDataItem(ctx, credential, imagineImageValue{URL: delta}, "url")
				if imageErr != nil {
					return imageErr
				}
				delta = liteImageMarkdown(item)
				if parsed.Text.Len() > 0 {
					delta = "\n\n" + delta
				}
				parsed.appendText(delta)
				archivedImages[rawURL] = struct{}{}
				kind = "text"
			}
			if kind == "text" && sieve != nil {
				result := sieve.Feed(delta)
				if result.SafeText != "" {
					clientText.WriteString(result.SafeText)
					if err := writeDelta(kind, result.SafeText); err != nil {
						return err
					}
				}
				if result.Complete {
					if len(result.Calls) == 0 {
						clientText.WriteString(result.Raw)
						if err := writeDelta(kind, result.Raw); err != nil {
							return err
						}
						return flushSideChannel()
					}
					parsed.ToolCalls = result.Calls
					return writeToolCalls(result.Calls)
				}
				return flushSideChannel()
			}
			if kind == "text" {
				if delta != "" {
					clientText.WriteString(delta)
				}
			}
			if delta != "" {
				if err := writeDelta(kind, delta); err != nil {
					return err
				}
			}
			return flushSideChannel()
		})
		if err != nil {
			a.feedbackStreamError(ctx, lease, err)
			_ = writer.CloseWithError(err)
			return
		}
		if sieve != nil && len(parsed.ToolCalls) == 0 {
			result := sieve.Flush()
			if result.SafeText != "" {
				clientText.WriteString(result.SafeText)
				if err := writeDelta("text", result.SafeText); err != nil {
					_ = writer.CloseWithError(err)
					return
				}
			}
			if len(result.Calls) > 0 {
				parsed.ToolCalls = result.Calls
				if err := writeToolCalls(result.Calls); err != nil {
					_ = writer.CloseWithError(err)
					return
				}
			}
		}
		if len(parsed.ToolCalls) == 0 {
			for _, rawURL := range parsed.Images {
				if _, exists := archivedImages[rawURL]; exists {
					continue
				}
				item, imageErr := a.imageDataItem(ctx, credential, imagineImageValue{URL: rawURL}, "url")
				if imageErr != nil {
					_ = writer.CloseWithError(imageErr)
					return
				}
				delta := liteImageMarkdown(item)
				if clientText.Len() > 0 {
					delta = "\n\n" + delta
				}
				clientText.WriteString(delta)
				if err := writeDelta("text", delta); err != nil {
					_ = writer.CloseWithError(err)
					return
				}
			}
		}
		parsed.resetText(clientText.String())
		if operation == conversation.OperationResponses {
			finalizeXAIAnnotations(parsed)
			if finishErr := responsesStream.Finish(parsed); finishErr != nil {
				_ = writer.CloseWithError(finishErr)
				return
			}
		}
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, http.StatusOK, nil)
		payload := buildOpenAIResult(operation, responseID, model, *parsed, false, options)
		data, _ := json.Marshal(payload)
		if operation == conversation.OperationResponses {
			a.saveResponseState(context.WithoutCancel(ctx), credential.ID, responseID, *parsed, data)
		}
		if operation == conversation.OperationMessages {
			if finishErr := messagesStream.Finish(*parsed, payload); finishErr != nil {
				_ = writer.CloseWithError(finishErr)
				return
			}
		} else {
			writeStreamDone(writer, operation, responseID, model, *parsed, payload)
		}
		_ = writer.Close()
	}()
	return stream
}

func writeWebStreamDelta(writer io.Writer, stream *webMessagesStream, operation, responseID, model, kind, delta string) error {
	if operation == conversation.OperationMessages {
		return stream.Delta(kind, delta)
	}
	return writeStreamDelta(writer, operation, responseID, model, kind, delta)
}

func writeWebStreamToolCalls(writer io.Writer, stream *webMessagesStream, operation, responseID, model string, calls []toolemu.Call) error {
	if operation == conversation.OperationMessages {
		return stream.Tools(calls)
	}
	return writeStreamToolCalls(writer, operation, responseID, model, calls)
}

func writeStreamStart(writer io.Writer, operation, responseID, model string, inputTokens int64) {
	if operation == "chat" {
		chunk := map[string]any{"id": strings.Replace(responseID, "resp_", "chatcmpl_", 1), "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}}}
		writeSSE(writer, "", chunk)
		return
	}
	if operation == conversation.OperationMessages {
		writeSSE(writer, "message_start", map[string]any{
			"type": "message_start", "message": map[string]any{
				"id": strings.Replace(responseID, "resp_", "msg_", 1), "type": "message", "role": "assistant", "model": model,
				"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
				"usage": map[string]any{"input_tokens": inputTokens, "output_tokens": 0, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0},
			},
		})
		writeSSE(writer, "content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		return
	}
	writeSSE(writer, "response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": responseID, "object": "response", "status": "in_progress", "model": model, "output": []any{}}})
}

type webVisibleStreamPhase struct {
	textStarted bool
}

// Allow keeps client-visible output monotonic when Grok Web emits additional
// reasoning after final text has already started. The complete reasoning is
// still retained in parsedChat for non-streaming output and usage accounting.
func (p *webVisibleStreamPhase) Allow(kind, delta string) bool {
	if delta == "" {
		return false
	}
	if kind == "reasoning" {
		return !p.textStarted
	}
	if kind == "text" {
		p.textStarted = true
	}
	return true
}

func writeStreamDelta(writer io.Writer, operation, responseID, model, kind, delta string) error {
	if operation == "chat" {
		field := "content"
		if kind == "reasoning" {
			field = "reasoning_content"
		}
		chunk := map[string]any{"id": strings.Replace(responseID, "resp_", "chatcmpl_", 1), "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{field: delta}, "finish_reason": nil}}}
		return writeSSE(writer, "", chunk)
	}
	if operation == conversation.OperationMessages {
		if kind == "reasoning" {
			return nil
		}
		return writeSSE(writer, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": delta}})
	}
	return errors.New("Responses 流式 delta 必须通过统一 output 状态机发送")
}

func writeStreamToolCalls(writer io.Writer, operation, responseID, model string, calls []toolemu.Call) error {
	if operation == "chat" {
		for index, call := range calls {
			chunk := map[string]any{
				"id": strings.Replace(responseID, "resp_", "chatcmpl_", 1), "object": "chat.completion.chunk",
				"created": time.Now().Unix(), "model": model,
				"choices": []any{map[string]any{"index": 0, "finish_reason": nil, "delta": map[string]any{
					"tool_calls": []any{map[string]any{
						"index": index, "id": call.ID, "type": "function",
						"function": map[string]any{"name": call.Name, "arguments": call.Arguments},
					}},
				}}},
			}
			if err := writeSSE(writer, "", chunk); err != nil {
				return err
			}
		}
		return nil
	}
	if operation == conversation.OperationMessages {
		for index, call := range calls {
			contentIndex := index + 1
			if err := writeSSE(writer, "content_block_start", map[string]any{
				"type": "content_block_start", "index": contentIndex,
				"content_block": map[string]any{"type": "tool_use", "id": call.ID, "name": call.Name, "input": map[string]any{}},
			}); err != nil {
				return err
			}
			if err := writeSSE(writer, "content_block_delta", map[string]any{
				"type": "content_block_delta", "index": contentIndex,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": call.Arguments},
			}); err != nil {
				return err
			}
			if err := writeSSE(writer, "content_block_stop", map[string]any{"type": "content_block_stop", "index": contentIndex}); err != nil {
				return err
			}
		}
		return nil
	}
	return errors.New("Responses 流式 tool call 必须通过统一 output 状态机发送")
}

func writeStreamDone(writer io.Writer, operation, responseID, model string, parsed parsedChat, payload map[string]any) {
	if operation == "chat" {
		finishReason := "stop"
		if len(parsed.ToolCalls) > 0 {
			finishReason = "tool_calls"
		}
		// Progressive annotations were already emitted as deltas. Repeating all of
		// them here makes clients that accumulate deltas display duplicates.
		// Top-level citations + server_side_tool_usage match non-stream chat (xAI).
		chunk := map[string]any{"id": strings.Replace(responseID, "resp_", "chatcmpl_", 1), "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finishReason}}, "usage": payload["usage"]}
		if citations := payload["citations"]; citations != nil {
			chunk["citations"] = citations
		}
		if toolUsage := payload["server_side_tool_usage"]; toolUsage != nil {
			chunk["server_side_tool_usage"] = toolUsage
		}
		writeSSE(writer, "", chunk)
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
		return
	}
	if operation == conversation.OperationMessages {
		writeSSE(writer, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		stopReason := "end_turn"
		if len(parsed.ToolCalls) > 0 {
			stopReason = "tool_use"
		}
		usage, _ := payload["usage"].(map[string]any)
		writeSSE(writer, "message_delta", map[string]any{
			"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": usage["output_tokens"]},
		})
		writeSSE(writer, "message_stop", map[string]any{"type": "message_stop"})
		return
	}
	writeSSE(writer, "response.completed", map[string]any{"type": "response.completed", "response": payload})
}

// writeStreamAnnotations emits Chat Completions delta.annotations. Responses
// annotations are owned by webResponsesStream so their item coordinates cannot drift.
func writeStreamAnnotations(writer io.Writer, operation, responseID, model string, annotations []map[string]any, _ int) error {
	if len(annotations) == 0 || operation == conversation.OperationMessages {
		return nil
	}
	if operation == "chat" {
		chunk := map[string]any{
			"id": strings.Replace(responseID, "resp_", "chatcmpl_", 1), "object": "chat.completion.chunk",
			"created": time.Now().Unix(), "model": model,
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"annotations": chatAnnotations(annotations)}, "finish_reason": nil,
			}},
		}
		return writeSSE(writer, "", chunk)
	}
	return nil
}

func writeSSE(writer io.Writer, event string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if event != "" {
		if _, err := fmt.Fprintf(writer, "event: %s\n", event); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(writer, "data: %s\n\n", data)
	return err
}
