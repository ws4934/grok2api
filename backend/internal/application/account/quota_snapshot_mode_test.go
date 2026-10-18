package account

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/chatgpt"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

var _ quotaModeSnapshotAdapter = (*chatgpt.Adapter)(nil)

// snapshotQuotaAdapter answers every quota sync with its whole snapshot, like
// ChatGPT conversation/init.
type snapshotQuotaAdapter struct {
	modeCalls atomic.Int64
	resetAt   time.Time
}

func (*snapshotQuotaAdapter) Provider() accountdomain.Provider { return accountdomain.ProviderChatGPT }

func (*snapshotQuotaAdapter) Definition() provider.Definition {
	return provider.Definition{
		Provider: accountdomain.ProviderChatGPT, ModelNamespace: accountdomain.ProviderChatGPT.ModelNamespace(),
		Profile: provider.ProfileSurface{QuotaModes: []string{"reason", "image_gen", "file_upload"}},
		Quota:   provider.QuotaRemoteWindow, Credential: provider.CredentialSurface{AuthType: accountdomain.AuthTypeOAuth},
	}
}

func (*snapshotQuotaAdapter) QuotaModeSyncReadsSnapshot() {}

func (a *snapshotQuotaAdapter) SyncQuota(_ context.Context, credential accountdomain.Credential) (provider.QuotaSnapshot, error) {
	now := time.Now().UTC()
	return provider.QuotaSnapshot{SyncedAt: now, Windows: []accountdomain.QuotaWindow{
		{AccountID: credential.ID, Mode: "reason", Remaining: 5, SyncedAt: &now, Source: accountdomain.QuotaSourceUpstream, UpdatedAt: now},
		{AccountID: credential.ID, Mode: "image_gen", Remaining: 0, UsagePercent: 100, ResetAt: &a.resetAt, SyncedAt: &now, Source: accountdomain.QuotaSourceUpstream, UpdatedAt: now},
	}}, nil
}

func (a *snapshotQuotaAdapter) SyncQuotaMode(context.Context, accountdomain.Credential, string) (accountdomain.QuotaWindow, error) {
	a.modeCalls.Add(1)
	return accountdomain.QuotaWindow{}, errors.New("unexpected single-mode sync")
}

// Chat traffic refreshes "reason" only, but the response carries every
// window: a legacy exhausted image_gen window must be replaced and scheduled,
// and a mode the upstream no longer limits must be dropped and unscheduled.
func TestQuotaModeRefreshStoresWholeSnapshot(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "snapshot-quota-mode.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	credential, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderChatGPT, AuthType: accountdomain.AuthTypeOAuth, Name: "chatgpt", SourceKey: "chatgpt",
		EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: accountdomain.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-time.Hour)
	if err := accounts.SaveQuotaWindows(ctx, credential.ID, "", old, []accountdomain.QuotaWindow{
		{AccountID: credential.ID, Mode: "image_gen", Remaining: 0, UsagePercent: 100, SyncedAt: &old, Source: accountdomain.QuotaSourceUpstream},
		{AccountID: credential.ID, Mode: "file_upload", Remaining: 0, SyncedAt: &old, Source: accountdomain.QuotaSourceUpstream},
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	adapter := &snapshotQuotaAdapter{resetAt: now.Add(2 * time.Hour).Truncate(time.Second)}
	service := NewService(accounts, nil, nil, nil, provider.NewRegistry(adapter), nil, nil)
	queue := memory.NewQuotaRecoveryQueue()
	service.SetQuotaRecoveryQueue(queue)
	if err := queue.ScheduleQuotaRecovery(ctx, accountdomain.QuotaRecoveryEvent{AccountID: credential.ID, Mode: "file_upload", DueAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	window, err := service.RefreshQuotaMode(ctx, credential.ID, "reason")
	if err != nil {
		t.Fatal(err)
	}
	if window.Mode != "reason" || window.Remaining != 5 || adapter.modeCalls.Load() != 0 {
		t.Fatalf("window = %#v, single-mode calls = %d", window, adapter.modeCalls.Load())
	}
	stored, err := accounts.GetQuotaWindows(ctx, []uint64{credential.ID})
	if err != nil {
		t.Fatal(err)
	}
	byMode := map[string]accountdomain.QuotaWindow{}
	for _, value := range stored[credential.ID] {
		byMode[value.Mode] = value
	}
	if len(byMode) != 2 || byMode["image_gen"].ResetAt == nil || !byMode["image_gen"].ResetAt.Equal(adapter.resetAt) || byMode["image_gen"].SyncedAt == nil || byMode["image_gen"].SyncedAt.Before(now.Add(-time.Second)) {
		t.Fatalf("stored windows = %#v", stored[credential.ID])
	}
	due, err := queue.ClaimDueQuotaRecoveries(ctx, now.Add(3*time.Hour), 10, time.Minute)
	if err != nil || len(due) != 1 || due[0].Mode != "image_gen" {
		t.Fatalf("recovery events = %#v, err = %v", due, err)
	}
	if latest, err := accounts.Get(ctx, credential.ID); err != nil || latest.QuotaSyncedAt == nil || latest.QuotaSyncedAt.Before(now.Add(-time.Second)) {
		t.Fatalf("quota sync marker = %v, err = %v", latest.QuotaSyncedAt, err)
	}
}
