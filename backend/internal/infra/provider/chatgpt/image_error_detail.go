// Protocol portions adapted from SkyKoo/gptGrok2api at 15773099641575776025e21d1290b5209b058c5a. See LICENSE.reference.
package chatgpt

import (
	"errors"
	"regexp"
	"strings"
	"unicode"
)

const imageTerminalPrefix = "OpenAI image generation stopped by upstream: "
const maxImageErrorDetailRunes = 64 * 1024

// ImageErrorDetail is diagnostic text, never the upstream JSON or credentials.
// Ordinary API errors continue to expose only the short Error() message.
type ImageErrorDetail struct {
	Message   string
	Truncated bool
}

func ImageErrorDetails(err error) ImageErrorDetail {
	var terminal *imageTerminalError
	if errors.As(err, &terminal) {
		return terminal.detail
	}
	return ImageErrorDetail{}
}

var imageErrorRedactions = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	// Redact whole credential header values, including multi-cookie lines.
	{regexp.MustCompile(`(?i)(\b(?:set-)?cookie\s*[:=]\s*)[^\r\n]+`), `${1}[REDACTED]`},
	{regexp.MustCompile(`(?im)^(\s*(?:authorization|proxy-authorization|cookie|set-cookie)\s*:\s*)[^\r\n]+`), `${1}[REDACTED]`},
	{regexp.MustCompile(`(?i)\b(Bearer|Basic)\s+[A-Za-z0-9._~+/=:-]+`), `${1} [REDACTED]`},
	{regexp.MustCompile(`(?i)(["']?(?:access[_-]?token|refresh[_-]?token|id[_-]?token|session[_-]?token|token|session|api[_-]?key|x-api-key|authorization|proxy-authorization|cookie|set-cookie|password|secret|b64_json)["']?\s*[:=]\s*)(?:"(?:\\.|[^"\\])*"|'[^']*'|\[REDACTED\]|[^\s,;}\]]+)`), `${1}[REDACTED]`},
	// Signed URLs may carry secrets in the path as well as the query string.
	{regexp.MustCompile(`(?i)\b(?:https?|socks4a?|socks5h?)://[^\s<>"']+`), `[URL REDACTED]`},
	{regexp.MustCompile(`(?i)\bdata:[^\s<>"']+`), `[DATA REDACTED]`},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`), `[REDACTED]`},
	{regexp.MustCompile(`\b(?:sk|sess)-[A-Za-z0-9_-]{8,}`), `[REDACTED]`},
	{regexp.MustCompile(`[A-Za-z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`), `[EMAIL REDACTED]`},
}

func imageErrorDetail(reason string, secrets ...string) ImageErrorDetail {
	// Replace known opaque account credentials before any length limit, so a
	// credential crossing the limit cannot be persisted as an unredacted prefix.
	for _, secret := range secrets {
		if secret != "" {
			reason = strings.ReplaceAll(reason, secret, "[REDACTED]")
		}
	}
	reason = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, strings.ToValidUTF8(reason, "�"))
	for _, rule := range imageErrorRedactions {
		reason = rule.pattern.ReplaceAllString(reason, rule.replacement)
	}
	reason = strings.TrimSpace(reason)
	runes := []rune(reason)
	truncated := len(runes) > maxImageErrorDetailRunes
	if truncated {
		reason = string(runes[:maxImageErrorDetailRunes]) + "\n[diagnostic truncated: exceeded 65536 characters]"
	}
	return ImageErrorDetail{Message: imageTerminalPrefix + reason, Truncated: truncated}
}
