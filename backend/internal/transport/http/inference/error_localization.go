package inference

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

// localizeErrorJSON touches protocol error objects only, never generated text,
// tool arguments, refusal output, or arbitrary nested application data.
func localizeErrorJSON(raw []byte, status int) []byte {
	if !bytes.Contains(raw, []byte(`"error"`)) {
		return raw
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil {
		return raw
	}
	changed := localizeErrorField(root, "error", status)
	if rawString(root["type"]) == "error" {
		changed = localizeErrorMessage(root, status) || changed
	}
	if response := root["response"]; len(response) > 0 {
		localized := localizeErrorJSON(response, status)
		if !bytes.Equal(localized, response) {
			root["response"], changed = localized, true
		}
	}
	if item := root["item"]; len(item) > 0 {
		if localized := localizeImageCallError(item, status); !bytes.Equal(localized, item) {
			root["item"], changed = localized, true
		}
	}
	var output []json.RawMessage
	if json.Unmarshal(root["output"], &output) == nil {
		outputChanged := false
		for index, item := range output {
			if localized := localizeImageCallError(item, status); !bytes.Equal(localized, item) {
				output[index], outputChanged = localized, true
			}
		}
		if outputChanged {
			root["output"], _ = json.Marshal(output)
			changed = true
		}
	}
	if !changed {
		return raw
	}
	encoded, err := json.Marshal(root)
	if err != nil {
		return raw
	}
	return encoded
}

func localizeImageCallError(raw json.RawMessage, status int) json.RawMessage {
	var item map[string]json.RawMessage
	if json.Unmarshal(raw, &item) != nil || rawString(item["type"]) != "image_generation_call" || !localizeErrorField(item, "error", status) {
		return raw
	}
	encoded, _ := json.Marshal(item)
	return encoded
}

func localizeErrorField(root map[string]json.RawMessage, key string, status int) bool {
	var problem map[string]json.RawMessage
	if json.Unmarshal(root[key], &problem) != nil || problem == nil || !localizeErrorMessage(problem, status) {
		return false
	}
	root[key], _ = json.Marshal(problem)
	return true
}

func localizeErrorMessage(problem map[string]json.RawMessage, status int) bool {
	message := rawString(problem["message"])
	if message == "" {
		return false
	}
	code := rawString(problem["code"])
	if code == "" {
		code = rawString(problem["type"])
	}
	localized := provider.LocalizePublicErrorMessage(status, code, message)
	if localized == message {
		return false
	}
	problem["message"], _ = json.Marshal(localized)
	return true
}

func rawString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

// preserveHTTPErrorEnvelope retains upstream machine fields when they already
// form an API error object, while filling protocol-required defaults.
func preserveHTTPErrorEnvelope(raw []byte, status int, anthropic bool) []byte {
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil {
		return nil
	}
	var problem map[string]json.RawMessage
	if json.Unmarshal(root["error"], &problem) != nil || rawString(problem["message"]) == "" {
		return nil
	}
	if anthropic {
		root["type"], _ = json.Marshal("error")
		if rawString(problem["type"]) == "" || rawString(problem["type"]) == "upstream_error" {
			problem["type"], _ = json.Marshal(anthropicUpstreamHTTPErrorType(status))
		}
	} else {
		if rawString(problem["type"]) == "" || rawString(problem["type"]) == "upstream_error" {
			problem["type"], _ = json.Marshal(middleware.OpenAIErrorType(status))
		}
		if _, exists := problem["code"]; !exists {
			code, _ := gateway.ClassifyUpstreamHTTPError(status, raw)
			problem["code"], _ = json.Marshal(code)
		}
		if _, exists := problem["param"]; !exists {
			problem["param"] = json.RawMessage("null")
		}
	}
	_ = localizeErrorMessage(problem, status)
	root["error"], _ = json.Marshal(problem)
	encoded, _ := json.Marshal(root)
	return encoded
}

func writeLocalizedHTTPError(c *gin.Context, status int, raw []byte, anthropic bool) string {
	code, message := gateway.ClassifyUpstreamHTTPError(status, raw)
	if localized := preserveHTTPErrorEnvelope(raw, status, anthropic); localized != nil {
		middleware.NoteError(c, code, message)
		c.Data(status, "application/json", localized)
	} else if anthropic {
		writeAnthropicError(c, status, anthropicUpstreamHTTPErrorType(status), message, code)
	} else {
		writeOpenAIError(c, status, code, message)
	}
	return code
}

// errorSSELocalizer buffers possible errors across reads. A successful image
// event declared by SSE's event field (or a leading JSON type) passes through
// without retaining its Base64 line again. Other layouts keep the bounded JSON
// path. The inspector independently validates the complete JSON before counting.
type errorSSELocalizer struct {
	pending           []byte
	passingLine       bool
	successImageEvent bool
}

func (f *errorSSELocalizer) filter(chunk []byte, final bool) []byte {
	var out []byte
	if f.passingLine {
		index := bytes.IndexByte(chunk, '\n')
		if index < 0 {
			return chunk
		}
		out, chunk = append(out, chunk[:index+1]...), chunk[index+1:]
		f.passingLine = false
	}
	f.pending = append(f.pending, chunk...)
	for {
		index := bytes.IndexByte(f.pending, '\n')
		if index < 0 {
			if final {
				out = append(out, localizeSSEDataLine(f.pending)...)
				f.pending = nil
			} else {
				imageData := f.successImageEvent && bytes.HasPrefix(bytes.TrimSpace(f.pending), []byte("data:"))
				if imageData || successImageSSEPrefix(f.pending) || len(f.pending) > maxStreamEventInspectionBytes {
					out, f.pending, f.passingLine = append(out, f.pending...), nil, true
				}
			}
			return out
		}
		line := f.pending[:index+1]
		out = append(out, localizeSSEDataLine(line)...)
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			f.successImageEvent = false
		} else if event, ok := bytes.CutPrefix(trimmed, []byte("event:")); ok {
			f.successImageEvent = successImageEventType(bytes.TrimSpace(event))
		}
		f.pending = f.pending[index+1:]
	}
}

func successImageEventType(typ []byte) bool {
	return bytes.Equal(typ, []byte("image_generation.completed")) || bytes.Equal(typ, []byte("image_edit.completed")) ||
		bytes.Equal(typ, []byte("image_generation.partial_image")) || bytes.Equal(typ, []byte("image_edit.partial_image"))
}

// A fast path, not a field-order requirement: when type is not the first JSON
// member, the ordinary localization path still processes the complete event.
func successImageSSEPrefix(line []byte) bool {
	line = bytes.TrimSpace(line)
	for _, token := range []string{"data:", "{", `"type"`, ":"} {
		var ok bool
		line, ok = bytes.CutPrefix(line, []byte(token))
		if !ok {
			return false
		}
		line = bytes.TrimSpace(line)
	}
	for _, typ := range []string{`"image_generation.completed"`, `"image_edit.completed"`, `"image_generation.partial_image"`, `"image_edit.partial_image"`} {
		if tail, ok := bytes.CutPrefix(line, []byte(typ)); ok {
			tail = bytes.TrimSpace(tail)
			return len(tail) > 0 && (tail[0] == ',' || tail[0] == '}')
		}
	}
	return false
}

func localizeSSEDataLine(line []byte) []byte {
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return line
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
	localized := localizeErrorJSON(payload, http.StatusBadGateway)
	if bytes.Equal(localized, payload) {
		return line
	}
	out := append([]byte("data: "), localized...)
	if bytes.HasSuffix(line, []byte("\r\n")) {
		return append(out, '\r', '\n')
	}
	if bytes.HasSuffix(line, []byte("\n")) {
		return append(out, '\n')
	}
	return out
}
