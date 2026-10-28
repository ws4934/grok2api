package account

import (
	"context"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type quotaReadCounter struct {
	repository.AccountRepository
	reads int
}

func (r *quotaReadCounter) GetQuotaWindows(ctx context.Context, ids []uint64) (map[uint64][]accountdomain.QuotaWindow, error) {
	r.reads++
	return r.AccountRepository.GetQuotaWindows(ctx, ids)
}

func TestReconcileKnownQuotaReadsSnapshotOnlyOnce(t *testing.T) {
	repo := quotaFrequencyDatabase(t)
	ctx := context.Background()
	credential, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{Provider: accountdomain.ProviderChatGPT, AuthType: accountdomain.AuthTypeOAuth, Name: "quota", SourceKey: "quota", EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: accountdomain.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	now, reset := time.Now().UTC(), time.Now().UTC().Add(time.Hour)
	if err := repo.SaveQuotaWindows(ctx, credential.ID, "", now, []accountdomain.QuotaWindow{{AccountID: credential.ID, Mode: "image_gen", Remaining: 5, ResetAt: &reset, SyncedAt: &now, UpdatedAt: now, Source: accountdomain.QuotaSourceUpstream}}); err != nil {
		t.Fatal(err)
	}
	counter := &quotaReadCounter{AccountRepository: repo}
	service := NewService(counter, nil, nil, nil, provider.NewRegistry(), nil, nil)
	state, err := service.ReconcileRateLimit(ctx, credential.ID, "image_gen", 0)
	if err != nil || state != RateLimitReconcileExhausted || counter.reads != 1 {
		t.Fatalf("state=%s reads=%d err=%v", state, counter.reads, err)
	}
	windows, err := repo.GetQuotaWindows(ctx, []uint64{credential.ID})
	if err != nil || len(windows[credential.ID]) != 1 || windows[credential.ID][0].Remaining != 0 || !windows[credential.ID][0].ResetAt.Equal(reset) {
		t.Fatalf("windows=%+v err=%v", windows, err)
	}
}
