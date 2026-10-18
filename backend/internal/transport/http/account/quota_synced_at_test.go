package account

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

// A synced account that reports no limits has no windows; quotaSyncedAt still
// tells the accounts page that it was synced.
func TestAccountResponseCarriesQuotaSyncedAt(t *testing.T) {
	synced := time.Date(2026, 10, 18, 9, 30, 0, 0, time.UTC)
	data, err := json.Marshal(newAccountResponse(accountapp.View{Credential: accountdomain.Credential{ID: 1, Provider: accountdomain.ProviderChatGPT, QuotaSyncedAt: &synced}}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"quotaSyncedAt":"2026-10-18T09:30:00Z"`) {
		t.Fatalf("response = %s", data)
	}
	data, err = json.Marshal(newAccountResponse(accountapp.View{Credential: accountdomain.Credential{ID: 2, Provider: accountdomain.ProviderChatGPT}}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "quotaSyncedAt") {
		t.Fatalf("never-synced account exposes quotaSyncedAt: %s", data)
	}
}
