package inference

import (
	"bytes"
	"encoding/json"

	"github.com/chenyme/grok2api/backend/internal/pkg/imagejson"
	"unicode/utf8"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
)

type responseInspector struct {
	protocol        streamProtocol
	pending         []byte
	imageProjection imagejson.Projection
	droppingLine    bool
	metadata        responseMetadata
	onFirstToken    func()
	firstTokenSeen  bool
	firstTokenReady bool
	terminalSuccess bool
	terminalFailure bool
}

const (
	reasoningStartSSEComment    = ": grok2api-reasoning-start"
	reasoningEvidenceSSEComment = ": grok2api-reasoning-evidence"
)

var internalSSEMarkers = [][]byte{
	[]byte(reasoningStartSSEComment + "\n\n"),
	[]byte(reasoningEvidenceSSEComment + "\n\n"),
}

func (i *responseInspector) Inspect(chunk []byte) {
	for len(chunk) > 0 {
		index := bytes.IndexByte(chunk, '\n')
		fragment := chunk
		if index >= 0 {
			fragment = chunk[:index]
		}
		if !i.droppingLine {
			if i.protocol == streamProtocolImage {
				i.pending = i.imageProjection.Append(i.pending, fragment)
				if i.imageProjection.PayloadObserved() && bytes.HasPrefix(bytes.TrimSpace(i.pending), []byte("data:")) {
					i.metadata.Usage.OutputObserved = true
				}
			} else {
				i.pending = append(i.pending, fragment...)
			}
			if len(i.pending) > maxStreamEventInspectionBytes {
				// The line has already been forwarded by copyStream. Treat an
				// oversized SSE data line as observed output conservatively so a
				// later idle timeout cannot misclassify a non-empty response and
				// apply the long empty-stream cooldown.
				if bytes.HasPrefix(bytes.TrimSpace(i.pending), []byte("data:")) {
					i.metadata.Usage.OutputObserved = true
				}
				i.pending = nil
				i.droppingLine = true
			}
		}
		if index < 0 {
			return
		}
		if !i.droppingLine && !i.imageProjection.Invalid() {
			i.inspectLine(i.pending)
		}
		i.pending = i.pending[:0]
		i.droppingLine = false
		i.imageProjection = imagejson.Projection{}
		chunk = chunk[index+1:]
	}
}

func (i *responseInspector) inspectLine(raw []byte) {
	line := bytes.TrimSpace(raw)
	if i.protocol == streamProtocolChat && bytes.Equal(line, []byte(reasoningStartSSEComment)) {
		i.observeReasoningStart()
		return
	}
	if bytes.HasPrefix(line, []byte("data:")) {
		value := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		event := inspectStreamEvent(value, i.protocol)
		if event.generated {
			i.metadata.Usage.OutputObserved = true
		}
		i.observeFirstToken(event.generated)
		i.observeTerminal(value, event)
		if !bytes.Equal(value, []byte("[DONE]")) {
			metadata := event.metadata
			if hasUsageMetadata(metadata.Usage) {
				if metadata.Usage.ResponseModel == "" {
					metadata.Usage.ResponseModel = i.metadata.Model
				}
				i.metadata.Usage = mergeGatewayUsage(i.metadata.Usage, metadata.Usage)
			}
			if metadata.ResponseID != "" {
				i.metadata.ResponseID = metadata.ResponseID
			}
			if metadata.SequenceNumber > i.metadata.SequenceNumber {
				i.metadata.SequenceNumber = metadata.SequenceNumber
			}
			if metadata.Model != "" {
				i.metadata.Model = metadata.Model
				i.metadata.Usage.ResponseModel = metadata.Model
			}
			if metadata.cacheCreationInputTokens > 0 {
				i.metadata.cacheCreationInputTokens = metadata.cacheCreationInputTokens
			}
		}
	}
}

func (i *responseInspector) Metadata() responseMetadata {
	return normalizeMetadataUsage(i.metadata, i.protocol)
}

func (i *responseInspector) observeReasoningStart() {
	if i.firstTokenSeen || i.firstTokenReady || i.onFirstToken == nil {
		return
	}
	i.firstTokenReady = true
}

func (i *responseInspector) observeFirstToken(generated bool) {
	if i.firstTokenSeen || i.firstTokenReady || i.onFirstToken == nil || !generated {
		return
	}
	i.firstTokenReady = true
}

func (i *responseInspector) markFirstTokenForwarded() {
	if i.firstTokenSeen || !i.firstTokenReady || i.onFirstToken == nil {
		return
	}
	i.firstTokenReady = false
	i.firstTokenSeen = true
	i.onFirstToken()
	i.onFirstToken = nil
}

// streamEvent is what the inspector reads from one SSE data line.
type streamEvent struct {
	generated bool
	eventType string
	typed     bool // eventType was decoded
	metadata  responseMetadata
}

// streamLineDTO holds the fields observeTerminal and extractMetadata read.
type streamLineDTO struct {
	Type string `json:"type"`
	responsePayloadDTO
}

// inspectStreamEvent decodes a data line once for everything the inspector
// reads. When that fails (invalid JSON, or a field of an unexpected type) it
// falls back to the narrower decodes, each of which still reads the fields it
// understands.
func inspectStreamEvent(data []byte, protocol streamProtocol) streamEvent {
	var line streamLineDTO
	var generated bool
	var err error
	switch protocol {
	case streamProtocolResponses:
		var event struct {
			streamLineDTO
			responsesDeltaDTO
		}
		err = json.Unmarshal(data, &event)
		line, generated = event.streamLineDTO, event.generated(event.Type)
	case streamProtocolChat:
		var event struct {
			streamLineDTO
			chatDeltaDTO
		}
		err = json.Unmarshal(data, &event)
		line, generated = event.streamLineDTO, event.generated()
	case streamProtocolAnthropic:
		var event struct {
			streamLineDTO
			anthropicDeltaDTO
		}
		err = json.Unmarshal(data, &event)
		line, generated = event.streamLineDTO, event.generated(event.Type)
	default:
		err = json.Unmarshal(data, &line)
	}
	if err == nil {
		return streamEvent{generated: generated, eventType: line.Type, typed: true, metadata: line.toMetadata()}
	}
	var payload struct {
		Type string `json:"type"`
	}
	typed := json.Unmarshal(data, &payload) == nil
	return streamEvent{generated: containsGeneratedDelta(data, protocol), eventType: payload.Type, typed: typed, metadata: extractMetadata(data)}
}

func containsGeneratedDelta(data []byte, protocol streamProtocol) bool {
	switch protocol {
	case streamProtocolResponses:
		var event struct {
			Type string `json:"type"`
			responsesDeltaDTO
		}
		return json.Unmarshal(data, &event) == nil && event.generated(event.Type)
	case streamProtocolChat:
		var event chatDeltaDTO
		return json.Unmarshal(data, &event) == nil && event.generated()
	case streamProtocolAnthropic:
		var event struct {
			Type string `json:"type"`
			anthropicDeltaDTO
		}
		return json.Unmarshal(data, &event) == nil && event.generated(event.Type)
	}
	return false
}

type responsesDeltaDTO struct {
	Delta string `json:"delta"`
	Item  struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	} `json:"item"`
}

func (event responsesDeltaDTO) generated(eventType string) bool {
	switch eventType {
	case "response.output_text.delta", "response.reasoning_summary_text.delta", "response.reasoning_text.delta", "response.refusal.delta", "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		return event.Delta != ""
	case "response.output_item.added":
		// Native Responses can stream an identified reasoning item with no
		// text delta when only encrypted_content is requested. That item is
		// still generation start; waiting for output_text kicks thinking
		// time out of the TPS denominator.
		return event.Item.Type == "reasoning" && event.Item.ID != ""
	}
	return false
}

type chatDeltaDTO struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			Reasoning        string `json:"reasoning"`
			ReasoningContent string `json:"reasoning_content"`
			ThinkingContent  string `json:"thinking_content"`
			Refusal          string `json:"refusal"`
			ToolCalls        []struct {
				Function struct {
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
}

func (event chatDeltaDTO) generated() bool {
	for _, choice := range event.Choices {
		delta := choice.Delta
		if delta.Content != "" || delta.Reasoning != "" || delta.ReasoningContent != "" || delta.ThinkingContent != "" || delta.Refusal != "" {
			return true
		}
		for _, call := range delta.ToolCalls {
			if call.Function.Arguments != "" {
				return true
			}
		}
	}
	return false
}

type anthropicDeltaDTO struct {
	ContentBlock struct {
		Type string `json:"type"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
	} `json:"delta"`
}

func (event anthropicDeltaDTO) generated(eventType string) bool {
	if eventType == "content_block_start" {
		return event.ContentBlock.Type == "thinking"
	}
	if eventType != "content_block_delta" {
		return false
	}
	switch event.Delta.Type {
	case "text_delta":
		return event.Delta.Text != ""
	case "thinking_delta":
		return event.Delta.Thinking != ""
	case "input_json_delta":
		return event.Delta.PartialJSON != ""
	}
	return false
}

func (i *responseInspector) TerminalError() error {
	if i.terminalFailure {
		return errUpstreamStreamFailed
	}
	if !i.terminalSuccess {
		return errUpstreamStreamIncomplete
	}
	return nil
}

func (i *responseInspector) observeTerminal(data []byte, event streamEvent) {
	if bytes.Equal(data, []byte("[DONE]")) {
		if i.protocol == streamProtocolChat {
			i.terminalSuccess = true
		}
		return
	}
	// Chat Completions can end with {"error": {...}} without a type field.
	if i.protocol == streamProtocolChat && bytes.Contains(data, []byte(`"error"`)) {
		var envelope struct {
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal(data, &envelope) == nil && len(envelope.Error) > 0 && !bytes.Equal(bytes.TrimSpace(envelope.Error), []byte("null")) {
			i.markTerminalFailure(data)
			return
		}
	}
	if !event.typed {
		return
	}
	switch i.protocol {
	case streamProtocolResponses:
		switch event.eventType {
		case "response.completed":
			i.terminalSuccess = true
		case "response.failed", "response.incomplete", "response.error", "error":
			i.markTerminalFailure(data)
		}
	case streamProtocolChat:
		if event.eventType == "error" {
			i.markTerminalFailure(data)
		}
	case streamProtocolAnthropic:
		switch event.eventType {
		case "message_stop":
			i.terminalSuccess = true
		case "error":
			i.markTerminalFailure(data)
		}
	case streamProtocolImage:
		switch event.eventType {
		case "image_generation.completed", "image_edit.completed":
			i.terminalSuccess = true
			i.metadata.Usage.OutputImages++
		case "image_generation.failed", "image_edit.failed", "error":
			i.markTerminalFailure(data)
		}
	}
}

func (i *responseInspector) markTerminalFailure(data []byte) {
	i.terminalFailure = true
	if i.metadata.StreamFailure != nil {
		return
	}
	diagnostic := projectStreamFailureDiagnostic(data)
	if len(diagnostic.Body) > 0 {
		i.metadata.StreamFailure = &diagnostic
	}
}

func projectStreamFailureDiagnostic(data []byte) gateway.StreamFailureDiagnostic {
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil {
		return gateway.StreamFailureDiagnostic{}
	}
	projected := make(map[string]json.RawMessage)
	copySafeDiagnosticFields(projected, root, "type", "status", "code", "message", "param")
	if raw := projectSafeErrorValue(root["error"]); len(raw) > 0 {
		projected["error"] = raw
	}
	if responseRaw := root["response"]; len(responseRaw) > 0 {
		var response map[string]json.RawMessage
		if json.Unmarshal(responseRaw, &response) == nil {
			safeResponse := make(map[string]json.RawMessage)
			copySafeDiagnosticFields(safeResponse, response, "id", "status", "code", "message")
			if raw := projectSafeErrorValue(response["error"]); len(raw) > 0 {
				safeResponse["error"] = raw
			}
			if raw := projectSafeErrorValue(response["incomplete_details"]); len(raw) > 0 {
				safeResponse["incomplete_details"] = raw
			}
			if len(safeResponse) > 0 {
				if encoded, err := json.Marshal(safeResponse); err == nil {
					projected["response"] = encoded
				}
			}
		}
	}
	if len(projected) == 0 {
		return gateway.StreamFailureDiagnostic{}
	}
	encoded, err := json.Marshal(projected)
	if err != nil {
		return gateway.StreamFailureDiagnostic{}
	}
	diagnostic := gateway.StreamFailureDiagnostic{Body: encoded}
	if len(diagnostic.Body) > maxStreamFailureDiagnosticBytes {
		bounded := diagnostic.Body[:maxStreamFailureDiagnosticBytes]
		for len(bounded) > 0 && !utf8.Valid(bounded) {
			bounded = bounded[:len(bounded)-1]
		}
		diagnostic.Body = append([]byte(nil), bounded...)
		diagnostic.BodyTruncated = true
	} else {
		diagnostic.Body = append([]byte(nil), diagnostic.Body...)
	}
	return diagnostic
}

func copySafeDiagnosticFields(destination, source map[string]json.RawMessage, fields ...string) {
	for _, field := range fields {
		if raw := projectSafeScalar(source[field]); len(raw) > 0 {
			destination[field] = raw
		}
	}
}

func projectSafeErrorValue(raw json.RawMessage) json.RawMessage {
	if scalar := projectSafeScalar(raw); len(scalar) > 0 {
		return scalar
	}
	var value map[string]json.RawMessage
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	projected := make(map[string]json.RawMessage)
	copySafeDiagnosticFields(projected, value, "type", "status", "code", "message", "param", "reason")
	if len(projected) == 0 {
		return nil
	}
	encoded, err := json.Marshal(projected)
	if err != nil {
		return nil
	}
	return encoded
}

func projectSafeScalar(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	switch value.(type) {
	case nil, string, bool, float64:
		return append(json.RawMessage(nil), raw...)
	default:
		return nil
	}
}

func (i *responseInspector) Finish() {
	if !i.droppingLine && !i.imageProjection.Invalid() && len(i.pending) > 0 {
		i.inspectLine(i.pending)
	}
	i.pending = nil
	i.droppingLine = false
	i.imageProjection = imagejson.Projection{}
}
