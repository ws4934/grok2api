package provider

import (
	"net/http"
	"regexp"
	"strings"
	"unicode"
)

const ContentPolicyViolation = "content_policy_violation"
const ContentPolicyMessage = "内容未通过上游安全审核，请修改提示词或参考素材后重试。"

// IsContentSafetyRejection matches explicit upstream refusals, not a bare
// "policy" or "blocked" that could instead describe an account or network.
func IsContentSafetyRejection(text string) bool {
	text = strings.ToLower(text)
	return ContainsAny(text,
		"content_policy_violation", "content-policy-violation", "content_filter", "content-filter",
		"content-moderated", "content_moderated", "moderation_blocked", "safety_check_type_",
		"rejected by content moderation", "blocked by content moderation", "content violates usage guidelines",
		"violate our content policies", "violates our content policies", "violates the content policy",
		"violates our content policy", "violates content policy", "content policy violation",
		"content safety policy", "content filter", "未通过上游内容审核", "未通过上游安全审核", "内容审核未通过",
	)
}

var publicToolName = regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9_]*(?:__[A-Za-z0-9_]+)+\b`)
var publicChannelLabel = regexp.MustCompile(`(?i)\b(?:Grok\s+(?:Web|Console|Build)|ChatGPT|Adobe(?:\s+Firefly)?|Muse|Console)\s+`)

// LocalizePublicErrorMessage changes only human-readable text. Callers retain
// their protocol's status, machine codes, parameters and envelope. Raw upstream
// messages belong in request diagnostics, never in a generic fallback here.
func LocalizePublicErrorMessage(status int, code, message string) string {
	message = strings.TrimSpace(message)
	text := strings.ToLower(code + " " + message)
	if IsContentSafetyRejection(text) {
		return ContentPolicyMessage
	}
	// Channel labels belong to diagnostics, not the client's failure reason.
	// This runs only on error messages; model IDs, codes and generated text stay intact.
	message = strings.TrimSpace(publicChannelLabel.ReplaceAllString(message, ""))
	switch {
	case strings.Contains(text, "fully transparent"):
		return "生成的图片完全透明，没有可见内容，请调整提示词后重试。"
	case strings.Contains(text, "did not satisfy background=opaque"):
		return "生成的图片仍包含透明区域，未满足 background=opaque 的不透明背景要求。"
	case ContainsAny(text, "no transparent pixels", "does not contain transparent", "has no alpha", "transparency requirement"):
		return "生成的图片未包含透明区域，无法满足透明背景要求。"
	case strings.Contains(text, "image output could not be decoded"):
		return "生成的图片解码失败，无法处理输出结果。"
	case strings.Contains(text, "image output exceeds webp"):
		return "生成的图片边长超过 WebP 的 16384 像素上限。"
	case strings.Contains(text, "image output could not be encoded"):
		return "生成的图片无法在大小限制内编码为所需格式。"
	case strings.Contains(text, "image output format is unsupported"):
		return "生成的图片格式不受支持。"
	case strings.Contains(text, "exceeds 25 megapixels"):
		return "输入图片超过 2500 万像素，请缩小图片后重试。"
	case strings.Contains(text, "input image too large"):
		return "输入图片过大，请缩小图片或压缩后重试。"
	case strings.Contains(text, "not a supported image"):
		return "输入文件不是支持的图片格式。"
	case strings.Contains(text, "root must be an object"):
		if tool := publicToolName.FindString(message); tool != "" {
			return "工具 " + tool + " 的参数根节点必须是 object 类型。"
		}
		return "工具参数的根节点必须是 object 类型。"
	}
	// Existing local validation messages are already deliberate Chinese text;
	// preserve their field names, numeric limits and actionable detail.
	if strings.ContainsFunc(message, func(r rune) bool { return unicode.Is(unicode.Han, r) }) {
		return message
	}
	switch {
	case code == "rate_limit_exceeded", code == "rate_limit_error", code == "upstream_rate_limited":
		return "请求过于频繁，请稍后重试。"
	case code == "invalid_argument", code == "invalid_request_error", code == "invalid_request", code == "invalid_parameter":
		return "请求参数无效，请检查模型、提示词及输入参数。"
	case code == "request_canceled", code == "response_cancelled":
		return "请求已取消。"
	case code == "media_postprocessing_failed":
		return "媒体已生成，但结果处理失败。"
	case code == "input_unavailable":
		return "输入素材不存在、已过期或无法读取，请重新上传。"
	case code == "submission_uncertain", code == "result_uncertain":
		return "上游可能已接收生成请求，但结果暂时无法确认，请查询任务状态，避免重复提交。"
	case code == "result_unavailable":
		return "生成结果不存在或已从存储中移除。"
	case code == "upstream_header_timeout":
		return "等待上游响应头超时。"
	case code == "upstream_stream_idle_timeout":
		return "上游响应长时间无数据。"
	case code == "upstream_response_empty":
		return "上游返回了空响应。"
	case code == "upstream_network_error":
		return "连接上游服务失败。"
	case status == http.StatusBadRequest:
		return "请求参数无效，请检查模型、提示词及输入参数。"
	case status == http.StatusUnauthorized:
		return "身份验证失败，请检查 API Key。"
	case status == http.StatusPaymentRequired:
		return "上游服务额度不足，请稍后重试。"
	case status == http.StatusForbidden:
		return "上游拒绝了该请求。"
	case status == http.StatusNotFound:
		return "请求的资源不存在或已过期。"
	case status == http.StatusConflict:
		return "请求与当前资源状态冲突，请检查后重试。"
	case status == http.StatusRequestEntityTooLarge:
		return "请求内容超过大小限制。"
	case status == http.StatusUnsupportedMediaType:
		return "请求的内容类型不受支持。"
	case status == http.StatusUnprocessableEntity:
		return "上游无法处理该请求，请检查提示词及输入素材。"
	case status == http.StatusTooManyRequests:
		return "请求过于频繁，请稍后重试。"
	case status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout:
		return "上游服务响应超时，请稍后重试。"
	case status == http.StatusServiceUnavailable:
		return "上游服务暂不可用，请稍后重试。"
	default:
		return "上游服务处理失败，请稍后重试。"
	}
}
