package provider

import (
	"encoding/json"
	"strings"
)

// IsDefinitiveAccountBlockBody accepts only explicit error code or message signals.
func IsDefinitiveAccountBlockBody(body []byte) bool {
	return IsDefinitiveAccountBlockText(errorSignalText(body))
}

func IsDefinitiveAccountBlockText(value string) bool {
	value = strings.ToLower(value)
	return strings.Contains(value, "blocked-user") || strings.Contains(value, "user is blocked")
}

// IsDPoPProofRequiredBody reports the Console protocol-level DPoP challenge.
// It must not be attributed to an account credential or physical egress node.
func IsDPoPProofRequiredBody(body []byte) bool {
	return IsDPoPProofRequiredText(errorSignalText(body))
}

func IsDPoPProofRequiredText(value string) bool {
	normalized := strings.NewReplacer("-", "_", ":", "_", ".", "_", " ", "_").Replace(strings.ToLower(strings.TrimSpace(value)))
	return strings.Contains(normalized, "unauthorized_dpop_required") || strings.Contains(normalized, "dpop_proof_required")
}

// errorSignalText joins the code, message and error strings of a JSON error
// body and of its nested error object; a body that is not JSON stays as is.
func errorSignalText(body []byte) string {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return string(body)
	}
	values := []string{
		jsonStringField(payload, "code"),
		jsonStringField(payload, "message"),
		jsonStringField(payload, "error"),
	}
	if nested, ok := payload["error"].(map[string]any); ok {
		values = append(values,
			jsonStringField(nested, "code"),
			jsonStringField(nested, "message"),
			jsonStringField(nested, "error"),
		)
	}
	return strings.Join(values, " ")
}

func jsonStringField(value map[string]any, key string) string {
	result, _ := value[key].(string)
	return result
}
