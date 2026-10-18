// Protocol portions adapted from SkyKoo/gptGrok2api at 15773099641575776025e21d1290b5209b058c5a. See LICENSE.reference.
package chatgpt

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

type modelObserverKey struct{}

// Observe only structural assistant metadata, never content or user-supplied text.
// A Web conversation model slug is not proof of the image tool's model version.
func notifyUpstreamModel(ctx context.Context, value any) {
	fn, _ := ctx.Value(modelObserverKey{}).(func(string))
	if fn == nil {
		return
	}
	emit := func(v any) {
		slug, _ := v.(string)
		if len(slug) == 0 || len(slug) > 100 {
			return
		}
		for _, c := range slug {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_.", c)) {
				return
			}
		}
		fn(slug)
	}
	message := func(v any) {
		m, _ := v.(map[string]any)
		a, _ := m["author"].(map[string]any)
		if a["role"] != "assistant" {
			return
		}
		meta, _ := m["metadata"].(map[string]any)
		emit(meta["model_slug"])
	}
	var visit func(any)
	visit = func(v any) {
		m, ok := v.(map[string]any)
		if !ok {
			return
		}
		message(m["message"])
		if mapping, ok := m["mapping"].(map[string]any); ok {
			for _, node := range mapping {
				if n, ok := node.(map[string]any); ok {
					message(n["message"])
				}
			}
		}
		// SSE patches have protocol-defined paths; do not recurse into arbitrary values.
		switch m["p"] {
		case "/message":
			message(m["v"])
		case "/message/metadata/model_slug":
			emit(m["v"])
		case "/message/metadata":
			meta, _ := m["v"].(map[string]any)
			emit(meta["model_slug"])
		case "", nil:
			if m["o"] == "patch" {
				if rows, ok := m["v"].([]any); ok {
					for _, row := range rows {
						visit(row)
					}
				}
			} else if envelope, ok := m["v"].(map[string]any); ok {
				message(envelope["message"])
			}
		}
	}
	visit(value)
}

// Explicit codes or the conversation endpoint's exact model rejection are
// evidence of non-acceptance. Other 404s and timeouts must not regenerate work.
func IsModelRejected(err error) bool {
	var upstream *upstreamError
	if !errors.As(err, &upstream) || (upstream.Status != http.StatusBadRequest && upstream.Status != http.StatusNotFound) {
		return false
	}
	var body map[string]any
	if json.Unmarshal([]byte(upstream.Body), &body) != nil {
		return false
	}
	// Web also returns {"detail":"Model not found"} without an error code.
	// Scope this textual variant to conversation submission/prepare endpoints;
	// upload or download failures must never be treated as a model rejection.
	switch upstream.Endpoint {
	case "conversation", "/backend-api/conversation", "/backend-api/f/conversation", "/backend-api/f/conversation/prepare":
		if detail, ok := body["detail"].(string); ok && strings.EqualFold(strings.TrimSpace(detail), "Model not found") {
			return true
		}
	}
	for _, obj := range []any{body, body["error"], body["detail"]} {
		if m, ok := obj.(map[string]any); ok {
			switch m["code"] {
			case "model_not_found", "model_not_supported", "invalid_model", "unsupported_model":
				return true
			}
		}
	}
	return false
}
