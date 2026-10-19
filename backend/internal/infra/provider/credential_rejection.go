package provider

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// CredentialRejection 表示上游响应或错误是否构成「凭据被拒」的稳定判定。
// 与网关 UpstreamFailure 的 CredentialRejected / PermanentAccountDenial / SpendingLimitBlocked 分类保持一致，
// 供 account.Service 等非网关路径复用同一套失效收敛语义。
type CredentialRejection struct {
	// Rejected 表示该响应/错误应被认定为凭据级失效（需标 reauthRequired）。
	Rejected bool
	// PermanentAccountDenial 表示上游明确拒绝该账号访问聊天端点（非凭据本身失效）。
	// Build 账号此类拒绝按现有网关逻辑是 model-scoped，不应标 reauth；仅 Rejected 为真时才标。
	// 管理端 detect 路径同样仅持久化模型阻断，避免把仍可用于其他模型的账号移出号池。
	PermanentAccountDenial bool
	// SpendingLimitBlocked 表示付费账号被 spending-limit 永久阻断（402/403 personal-team-blocked:spending-limit），
	// 由调用方写入额度恢复状态，不应误判为 OAuth 凭据失效。
	SpendingLimitBlocked bool
	// QuotaExhausted 表示请求被账号级或模型级额度限制拒绝。
	QuotaExhausted bool
	// FreeQuotaExhausted 表示免费额度已经耗尽。
	FreeQuotaExhausted bool
	// ModelQuotaExhausted 表示额度限制只针对当前模型。
	ModelQuotaExhausted bool
}

// ClassifyCredentialRejection 按上游 HTTP 状态码与错误体判定凭据是否被拒。
// status 为上游 HTTP 状态；body 为响应正文（可为 nil）；err 为 Provider 返回的错误（可为 nil）。
func ClassifyCredentialRejection(status int, body []byte, err error) CredentialRejection {
	var result CredentialRejection
	if err != nil {
		if errors.Is(err, ErrUnauthorized) {
			result.Rejected = true
			return result
		}
		if httpStatus, ok := ErrorHTTPStatus(err); ok && httpStatus == http.StatusUnauthorized {
			result.Rejected = true
			return result
		}
	}
	switch status {
	case http.StatusUnauthorized:
		result.Rejected = true
	case http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests:
		upstreamCode, upstreamType, upstreamMessage := ExtractUpstreamErrorMetadata(body)
		metadataText := strings.ToLower(strings.Join([]string{upstreamCode, upstreamType, upstreamMessage}, " "))
		result.SpendingLimitBlocked = strings.Contains(metadataText, "personal-team-blocked:spending-limit")
		result.ModelQuotaExhausted = strings.Contains(metadataText, "used all the included free usage for model")
		result.FreeQuotaExhausted = result.ModelQuotaExhausted || strings.Contains(metadataText, "subscription:free-usage-exhausted")
		creditExhausted := ContainsAny(metadataText, "run out of credits", "out of credits", "usage balance exhausted", "usage limit reached")
		result.QuotaExhausted = status == http.StatusPaymentRequired || result.SpendingLimitBlocked || result.FreeQuotaExhausted || creditExhausted
		permanentDenial := IsPermanentAccountDenial(metadataText)
		result.PermanentAccountDenial = permanentDenial
		if status == http.StatusForbidden {
			result.Rejected = !result.QuotaExhausted && !permanentDenial && ContainsAny(metadataText,
				"authentication", "unauthorized", "invalid token", "token expired")
		}
	}
	return result
}

// ExtractUpstreamErrorMetadata 从上游错误响应正文中提取 code/type/message 三元组。
func ExtractUpstreamErrorMetadata(body []byte) (string, string, string) {
	if len(body) == 0 {
		return "", "", ""
	}
	var payload any
	if json.Unmarshal(body, &payload) != nil {
		return "", "", strings.TrimSpace(string(body))
	}
	root, ok := payload.(map[string]any)
	if !ok {
		return "", "", ""
	}
	if nested, ok := root["error"].(map[string]any); ok {
		code := FirstNonEmptyFailure(firstStringValue(nested, "code", "error_code"), firstStringValue(root, "code", "error_code"))
		errorType := FirstNonEmptyFailure(firstStringValue(nested, "type", "error_type"), firstStringValue(root, "type", "error_type"))
		message := FirstNonEmptyFailure(firstStringValue(nested, "message", "error"), firstStringValue(root, "message"))
		return code, errorType, message
	}
	message := FirstNonEmptyFailure(firstStringValue(root, "error"), firstStringValue(root, "message"))
	return firstStringValue(root, "code", "error_code"), firstStringValue(root, "type", "error_type"), message
}

// IsPermanentAccountDenial 判定 403 是否为「账号被永久拒绝访问聊天端点」。
func IsPermanentAccountDenial(text string) bool {
	if strings.Contains(text, "access to the chat endpoint is denied") {
		return true
	}
	return strings.Trim(strings.TrimSpace(text), " .!\t\r\n") == "access denied"
}

// ContainsAny 报告 text 是否包含任意一个 signal 子串。
func ContainsAny(text string, signals ...string) bool {
	for _, signal := range signals {
		if strings.Contains(text, signal) {
			return true
		}
	}
	return false
}

func firstStringValue(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key]; ok {
			if s, ok := value.(string); ok {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

// FirstNonEmptyFailure 返回第一个非空白字符串。
func FirstNonEmptyFailure(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
