package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type boundedCapture struct {
	data  []byte
	limit int
}

func (w *boundedCapture) Write(value []byte) (int, error) {
	remaining := w.limit - len(w.data)
	if remaining > 0 {
		w.data = append(w.data, value[:min(remaining, len(value))]...)
	}
	return len(value), nil
}

func (w *boundedCapture) Bytes() []byte { return w.data }

func extractCapturedImageURLs(data []byte) []string {
	results := make([]string, 0, 2)
	_ = consumeJSONObjects(bytes.NewReader(data), 8<<20, func(frame []byte) error {
		var value any
		if json.Unmarshal(frame, &value) == nil {
			collectCapturedImageURLs(value, &results)
		}
		return nil
	})
	return results
}

type liteCaptureDiagnostics struct {
	Frames         int
	ResponseFields []string
	MessageTags    []string
	ImageChunks    int
	ImageURLs      int
	ImageFields    []string
	MaxProgress    int
	SoftStop       bool
	ErrorCode      string
	ErrorMessage   string
}

func inspectLiteCapture(data []byte) liteCaptureDiagnostics {
	result := liteCaptureDiagnostics{}
	fields := make(map[string]struct{})
	tags := make(map[string]struct{})
	imageFields := make(map[string]struct{})
	_ = consumeJSONObjects(bytes.NewReader(data), 8<<20, func(frame []byte) error {
		result.Frames++
		var root map[string]any
		if json.Unmarshal(frame, &root) != nil {
			return nil
		}
		value, _ := root["result"].(map[string]any)
		response, _ := value["response"].(map[string]any)
		for key := range response {
			fields[key] = struct{}{}
		}
		if tag, _ := response["messageTag"].(string); tag != "" {
			tags[tag] = struct{}{}
		}
		if stopped, _ := response["isSoftStop"].(bool); stopped {
			result.SoftStop = true
		}
		if responseError, ok := response["error"].(map[string]any); ok {
			result.ErrorCode = fmt.Sprint(responseError["code"])
			result.ErrorMessage = firstString(responseError, "message", "error")
			if len(result.ErrorMessage) > 200 {
				result.ErrorMessage = result.ErrorMessage[:200]
			}
		}
		inspectLiteCaptureValue(response, &result, imageFields)
		return nil
	})
	result.ResponseFields = sortedSetValues(fields)
	result.MessageTags = sortedSetValues(tags)
	result.ImageFields = sortedSetValues(imageFields)
	return result
}

func inspectLiteCaptureValue(value any, result *liteCaptureDiagnostics, imageFields map[string]struct{}) {
	switch current := value.(type) {
	case map[string]any:
		for key, nested := range current {
			if key == "jsonData" {
				if encoded, _ := nested.(string); encoded != "" {
					var decoded any
					if json.Unmarshal([]byte(encoded), &decoded) == nil {
						inspectLiteCaptureValue(decoded, result, imageFields)
					}
				}
			}
			if key == "image_chunk" || key == "imageChunk" {
				if chunk, ok := nested.(map[string]any); ok {
					result.ImageChunks++
					for field := range chunk {
						imageFields[field] = struct{}{}
					}
					if firstString(chunk, "imageUrl", "image_url", "url") != "" {
						result.ImageURLs++
					}
					if progress, ok := numberAsInt(chunk["progress"]); ok && progress > result.MaxProgress {
						result.MaxProgress = progress
					}
				}
			}
			inspectLiteCaptureValue(nested, result, imageFields)
		}
	case []any:
		for _, nested := range current {
			inspectLiteCaptureValue(nested, result, imageFields)
		}
	}
}

func sortedSetValues(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func collectCapturedImageURLs(value any, results *[]string) {
	switch current := value.(type) {
	case map[string]any:
		if rawURL := imageURLFromCardData(current); rawURL != "" {
			appendCapturedImageURL(results, rawURL)
		}
		moderated, _ := current["moderated"].(bool)
		progress, hasProgress := numberAsInt(current["progress"])
		if !moderated && hasProgress && progress >= 100 {
			appendCapturedImageURL(results, firstString(current, "imageUrl", "image_url", "url"))
		}
		for _, nested := range current {
			collectCapturedImageURLs(nested, results)
		}
	case []any:
		for _, nested := range current {
			collectCapturedImageURLs(nested, results)
		}
	case string:
		trimmed := strings.TrimSpace(current)
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			var nested any
			if json.Unmarshal([]byte(trimmed), &nested) == nil {
				collectCapturedImageURLs(nested, results)
				return
			}
		}
		appendCapturedImageURL(results, trimmed)
	}
}

func appendCapturedImageURL(results *[]string, value string) {
	value = strings.TrimSpace(value)
	if !strings.Contains(value, "/generated/") || strings.Contains(value, "-part-") || strings.ContainsAny(value, "{}[]\"") {
		return
	}
	if !strings.HasPrefix(value, "https://") && !strings.HasPrefix(value, "users/") && !strings.HasPrefix(value, "/users/") {
		return
	}
	value = absoluteAssetURL(value)
	if !containsString(*results, value) {
		*results = append(*results, value)
	}
}
