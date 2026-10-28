package provider

import (
	"net/http"
	"strings"
	"testing"
)

func TestPublicErrorsUseChineseAndKeepTechnicalDetails(t *testing.T) {
	for _, text := range []string{
		"Generated video rejected by content moderation.",
		"We’re so sorry, but the image we created may violate our content policies. If you think we got it wrong, please retry or edit your prompt.",
		"image_content_policy_violation", "Content violates usage guidelines. SAFETY_CHECK_TYPE_VIOLENCE",
	} {
		if !IsContentSafetyRejection(text) || LocalizePublicErrorMessage(422, "", text) != ContentPolicyMessage {
			t.Errorf("unclassified safety error: %s", text)
		}
	}
	for _, text := range []string{"Account blocked", "quota policy", "connection blocked by cloudflare", "request rejected by policy"} {
		if IsContentSafetyRejection(text) {
			t.Errorf("not a content safety refusal: %s", text)
		}
	}
	if got := LocalizePublicErrorMessage(400, "invalid_argument", "reference_images 最多 14 张"); got != "reference_images 最多 14 张" {
		t.Fatal(got)
	}
	if got := LocalizePublicErrorMessage(400, "invalid_argument", "mcp__codex_app__test: tool parameter root must be an object type"); !strings.Contains(got, "mcp__codex_app__test") || !strings.Contains(got, "必须是 object") {
		t.Fatal(got)
	}
	if got := LocalizePublicErrorMessage(http.StatusUnprocessableEntity, "", "internal-token-do-not-expose"); strings.Contains(got, "internal-token") || !strings.Contains(got, "无法处理") {
		t.Fatal(got)
	}
}
