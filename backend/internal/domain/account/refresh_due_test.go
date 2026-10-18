package account

import (
	"testing"
	"time"
)

func TestCredentialRefreshDueAtLeadFollowsTheProvider(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	jitter := func(provider Provider, expiresAt time.Time, lead time.Duration) time.Duration {
		return expiresAt.Sub(CredentialRefreshDueAt(provider, 42, expiresAt, now)) - lead
	}
	week := now.Add(7 * 24 * time.Hour)
	// Short-lived providers keep the 5–8 minute window.
	if j := jitter(ProviderBuild, week, 5*time.Minute); j < 0 || j > 3*time.Minute {
		t.Fatalf("build lead jitter = %v", j)
	}
	// ChatGPT tokens live about a week: renew a day ahead.
	if j := jitter(ProviderChatGPT, week, 24*time.Hour); j < 0 || j > 3*time.Minute {
		t.Fatalf("chatgpt lead jitter = %v", j)
	}
	// A short ChatGPT token renews halfway, never in a loop.
	if j := jitter(ProviderChatGPT, now.Add(10*time.Hour), 5*time.Hour); j < 0 || j > 3*time.Minute {
		t.Fatalf("short chatgpt lead jitter = %v", j)
	}
	if j := jitter(ProviderChatGPT, now.Add(4*time.Minute), 5*time.Minute); j < 0 || j > 3*time.Minute {
		t.Fatalf("expiring chatgpt lead jitter = %v", j)
	}
	if !CredentialRefreshDueAt(ProviderChatGPT, 42, time.Time{}, now).IsZero() {
		t.Fatal("no expiry must mean no schedule")
	}
}
