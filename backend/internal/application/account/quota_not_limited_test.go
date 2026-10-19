package account

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

// newNotLimitedFixture stores a ChatGPT account whose file_upload window is
// exhausted and scheduled for recovery, while the snapshotQuotaAdapter
// snapshot no longer lists file_upload.
func newNotLimitedFixture(t *testing.T) (*Service, *relational.AccountRepository, *memory.QuotaRecoveryQueue, uint64) {
	t.Helper()
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "quota-not-limited.db"))
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
	now := time.Now().UTC()
	old := now.Add(-time.Hour)
	if err := accounts.SaveQuotaWindows(ctx, credential.ID, "", old, []accountdomain.QuotaWindow{
		{AccountID: credential.ID, Mode: "file_upload", Remaining: 0, UsagePercent: 100, SyncedAt: &old, Source: accountdomain.QuotaSourceUpstream},
	}); err != nil {
		t.Fatal(err)
	}
	adapter := &snapshotQuotaAdapter{resetAt: now.Add(2 * time.Hour).Truncate(time.Second)}
	service := NewService(accounts, nil, nil, nil, provider.NewRegistry(adapter), nil, nil)
	queue := memory.NewQuotaRecoveryQueue()
	service.SetQuotaRecoveryQueue(queue)
	if err := queue.ScheduleQuotaRecovery(ctx, accountdomain.QuotaRecoveryEvent{AccountID: credential.ID, Mode: "file_upload", DueAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	return service, accounts, queue, credential.ID
}

func TestRefreshQuotaModeReportsModeTheSnapshotNoLongerLimits(t *testing.T) {
	ctx := context.Background()
	service, accounts, queue, id := newNotLimitedFixture(t)

	if _, err := service.RefreshQuotaMode(ctx, id, "file_upload"); !errors.Is(err, accountdomain.ErrQuotaNotLimited) {
		t.Fatalf("refresh error = %v, want ErrQuotaNotLimited", err)
	}
	stored, err := accounts.GetQuotaWindows(ctx, []uint64{id})
	if err != nil {
		t.Fatal(err)
	}
	modes := map[string]bool{}
	for _, value := range stored[id] {
		modes[value.Mode] = true
	}
	if len(modes) != 2 || !modes["reason"] || !modes["image_gen"] {
		t.Fatalf("stored windows = %#v", stored[id])
	}
	due, err := queue.ClaimDueQuotaRecoveries(ctx, time.Now().UTC().Add(3*time.Hour), 10, time.Minute)
	if err != nil || len(due) != 1 || due[0].Mode != "image_gen" {
		t.Fatalf("recovery events = %#v, err = %v", due, err)
	}
}

func TestBackgroundRefreshDoesNotRetryModeTheSnapshotNoLongerLimits(t *testing.T) {
	service, _, _, id := newNotLimitedFixture(t)

	service.QueueQuotaRefresh(id, "file_upload")
	request := <-service.quotaRefreshQueue
	service.runQuotaRefresh(context.Background(), request)

	service.quotaRefreshMu.Lock()
	state := service.quotaRefreshes[request.key]
	service.quotaRefreshMu.Unlock()
	if state != nil {
		t.Fatalf("refresh retained retry state: %#v", state)
	}
}
