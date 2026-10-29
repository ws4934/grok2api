// Package diagnostic bounds and redacts text before it enters diagnostic storage.
package diagnostic

import (
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	authorizationPattern = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]+`)
	cookiePattern        = regexp.MustCompile(`(?i)\b(cookie|set-cookie)\b\s*[:=]\s*[^\r\n]+`)
	secretPattern        = regexp.MustCompile(`(?i)(["']?(?:authorization|proxy-authorization|x-api-key|api[_-]?key|access[_-]?token|refresh[_-]?token|id[_-]?token|token|sso(?:-rw)?|workos[_-]?(?:session|id)|password|client[_-]?secret|cf_clearance)["']?\s*[:=]\s*)(?:"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|[^\"'\s,;}]+)`)
	opaqueFieldPattern   = regexp.MustCompile(`(?i)(["'](?:encrypted[_-]?content|compaction[_-]?(?:blob|state)|opaque[_-]?(?:state|data))["']\s*[:=]\s*["'])((?:\\.|[^"'\\])*)(["']|$)`)
	urlPattern           = regexp.MustCompile(`(?i)(?:https?|postgres(?:ql)?|redis(?:s)?)://[^\s"'<>]+`)
	credentialPattern    = regexp.MustCompile(`\b(?:eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+|(?:sk-|xai-)[A-Za-z0-9_-]{16,})\b`)
	dataPattern          = regexp.MustCompile(`(?i)data:[^\s,;]+(?:;base64)?,[^\s"'<>]+`)
)

func Text(value string, limit int) string {
	value = cookiePattern.ReplaceAllString(value, "$1: [REDACTED]")
	value = authorizationPattern.ReplaceAllString(value, "$1 [REDACTED]")
	value = secretPattern.ReplaceAllStringFunc(value, func(match string) string {
		parts := secretPattern.FindStringSubmatch(match)
		prefix := parts[1]
		raw := match[len(prefix):]
		if strings.HasPrefix(raw, "\"") || strings.HasPrefix(raw, "'") {
			quote := raw[:1]
			return prefix + quote + "[REDACTED]" + quote
		}
		return prefix + "[REDACTED]"
	})
	value = opaqueFieldPattern.ReplaceAllString(value, "$1[REDACTED]$3")
	value = urlPattern.ReplaceAllStringFunc(value, URL)
	value = credentialPattern.ReplaceAllString(value, "[REDACTED]")
	value = dataPattern.ReplaceAllString(value, "[REDACTED DATA]")
	return Truncate(value, limit)
}

func Truncate(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	for limit > 0 && !utf8.ValidString(value[:limit]) {
		limit--
	}
	return value[:limit]
}

// URL removes userinfo, signed query parameters and fragments.
func URL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	parsed.RawFragment = ""
	return Truncate(parsed.String(), 4096)
}

func SensitiveKey(key string) bool {
	key = strings.ToLower(key)
	key = strings.NewReplacer("_", "", "-", "", ".", "", " ", "").Replace(key)
	for _, part := range []string{"password", "cookie", "authorization", "secret", "encryptedcontent", "compactionblob", "compactionstate", "opaquestate", "opaquedata", "accesstoken", "refreshtoken", "idtoken", "apikey", "workossession", "credential"} {
		if strings.Contains(key, part) {
			return true
		}
	}
	switch key {
	case "token", "accesstoken", "refreshtoken", "idtoken", "apikey", "xapikey", "sso", "ssorw", "workossession", "workosid", "cfclearance", "credentials", "credential", "sessiontoken":
		return true
	}
	return false
}
