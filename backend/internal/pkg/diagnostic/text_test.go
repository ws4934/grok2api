package diagnostic

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTextRedactsCredentialsAndSignedURLs(t *testing.T) {
	for _, input := range []string{
		"Authorization: Bearer top-secret", "Cookie: sso=top-secret; another=value", `{"password":"top-secret with spaces"}`,
		"sso=top-secret", "workosSession=top-secret", "cf_clearance=top-secret", "access_token=top-secret",
		"GET https://user:top-secret@assets.test/image.png?signature=top-secret#top-secret",
		"postgres://user:top-secret@database.test/app", "password='top-secret with spaces'",
		"data:image/png;base64,top-secret", "eyJabcdef.eyJtop-secret.abcdef",
	} {
		if got := Text(input, 4096); strings.Contains(got, "top-secret") || strings.Contains(got, "with spaces") {
			t.Errorf("leaked: %q => %q", input, got)
		}
	}
	if got := Text("duration_ms=123 account_id=42 retry_after_seconds=12", 4096); got != "duration_ms=123 account_id=42 retry_after_seconds=12" {
		t.Fatal(got)
	}
}
func TestTruncateKeepsUTF8(t *testing.T) {
	for i := range 16 {
		got := Truncate("账号额度同步失败", i)
		if len(got) > i || !utf8.ValidString(got) {
			t.Fatal(got)
		}
	}
}
