package account

import "testing"

func TestDisplayNameMatchesAccountPageIdentityRules(t *testing.T) {
	for _, test := range []struct {
		value Credential
		want  string
	}{
		{Credential{Provider: ProviderWeb, Name: "Grok Web 219ff735", Email: " web@example.test "}, "web@example.test"},
		{Credential{Provider: ProviderConsole, Name: "Grok Console cbb90abd", Email: "own@example.test", LinkedAccounts: []LinkedAccount{{Email: "peer@example.test"}}}, "own@example.test"},
		{Credential{Provider: ProviderConsole, Name: "Grok Console cbb90abd", LinkedAccounts: []LinkedAccount{{Email: " "}, {Email: "peer@example.test"}}}, "peer@example.test"},
		{Credential{Provider: ProviderConsole, Name: "生产账号", Email: "own@example.test"}, "生产账号"},
		{Credential{Provider: ProviderConsole, Name: "Grok Console primary", Email: "own@example.test"}, "Grok Console primary"},
		{Credential{Provider: ProviderChatGPT, Name: "chatgpt-cbb90abd", Email: "gpt@example.test"}, "gpt@example.test"},
		{Credential{Provider: Provider("other"), Name: "other-user-123", UserID: "user-123456789", Email: "user@example.test"}, "user@example.test"},
		{Credential{Provider: ProviderWeb, Name: "Grok Web 219ff735"}, "Grok Web 219ff735"},
	} {
		if got := test.value.DisplayName(); got != test.want {
			t.Errorf("%+v => %q; want %q", test.value, got, test.want)
		}
	}
}
