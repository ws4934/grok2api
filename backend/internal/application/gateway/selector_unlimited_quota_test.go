package gateway

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/chatgpt"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

// A ChatGPT Plus account has no "reason" window because its messages are not
// limited. After a complete snapshot it must rank as having quota, not as an
// unknown fallback behind Free accounts; concurrency still spreads requests.
func TestSelectorRanksAnUnlimitedSnapshotModeAsAvailable(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "selector-unlimited.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	create := func(name string, priority int) account.Credential {
		value, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderChatGPT, AuthType: account.AuthTypeOAuth, Name: name, SourceKey: name,
			EncryptedAccessToken: "encrypted", AuthStatus: account.AuthStatusActive, Enabled: true, MaxConcurrent: 1, Priority: priority,
		})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	free, plus := create("free", 1), create("plus", 10)
	now := time.Now().UTC()
	if err := accounts.ReplaceQuotaWindows(ctx, free.ID, "", now, []account.QuotaWindow{
		{AccountID: free.ID, Mode: "reason", Remaining: 5, SyncedAt: &now, Source: account.QuotaSourceUpstream, UpdatedAt: now},
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.ReplaceQuotaWindows(ctx, plus.ID, "", now, []account.QuotaWindow{
		{AccountID: plus.ID, Mode: "image_gen", Remaining: 120, SyncedAt: &now, Source: account.QuotaSourceUpstream, UpdatedAt: now},
	}); err != nil {
		t.Fatal(err)
	}
	registry := provider.NewRegistry(chatgpt.NewAdapter(chatgpt.Config{}, nil, nil, nil))
	selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), registry, time.Hour, time.Second, time.Minute)

	first, err := selector.Acquire(ctx, account.ProviderChatGPT, 0, "auto", "reason", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if first.Credential.ID != plus.ID {
		t.Fatalf("first chat went to account %d, want the higher-priority Plus account %d", first.Credential.ID, plus.ID)
	}
	// The Plus account is busy (max concurrency 1): the next chat goes to Free.
	second, err := selector.Acquire(ctx, account.ProviderChatGPT, 0, "auto", "reason", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if second.Credential.ID != free.ID {
		t.Fatalf("second chat went to account %d, want the Free account %d", second.Credential.ID, free.ID)
	}
	second.Release()
	first.Release()
}
