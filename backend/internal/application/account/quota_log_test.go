package account

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/pkg/requestmeta"
)

func TestManualQuotaFailureLogsAccountStageAndCorrelation(t *testing.T) {
	ctx := requestmeta.WithRequestID(context.Background(), "manual-request")
	db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "quota.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAccountRepository(db)
	credential, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{Provider: accountdomain.ProviderConsole, AuthType: accountdomain.AuthTypeSSO, Name: "console", SourceKey: "quota-test", EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: accountdomain.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &rateLimitConsoleQuotaAdapter{err: errors.Join(provider.ErrEgressBlocked, errors.New("Cloudflare blocked; sso=secret-value"))}
	service := NewService(repo, nil, nil, nil, provider.NewRegistry(adapter), nil, nil)
	var output bytes.Buffer
	service.SetLogger(slog.New(slog.NewJSONHandler(&output, nil)))
	if _, err := service.RefreshQuota(ctx, credential.ID); !errors.Is(err, provider.ErrEgressBlocked) {
		t.Fatal(err)
	}
	for _, want := range []string{`"account_id":`, `"provider":"grok_console"`, `"request_id":"manual-request"`, `"stage":"upstream"`, `"category":"egress_blocked"`, `Cloudflare blocked`} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("missing %q: %s", want, output.String())
		}
	}
	if strings.Contains(output.String(), "secret-value") {
		t.Fatal(output.String())
	}
}

type blockingQuotaLogAdapter struct {
	rateLimitConsoleQuotaAdapter
	started, release chan struct{}
	once             sync.Once
}

func (a *blockingQuotaLogAdapter) SyncQuota(ctx context.Context, value accountdomain.Credential) (provider.QuotaSnapshot, error) {
	a.once.Do(func() { close(a.started) })
	<-a.release
	return a.rateLimitConsoleQuotaAdapter.SyncQuota(ctx, value)
}
func TestCoalescedQuotaFailuresKeepEachCallersRequestID(t *testing.T) {
	ctx := context.Background()
	db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "coalesced.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAccountRepository(db)
	value, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{Provider: accountdomain.ProviderConsole, AuthType: accountdomain.AuthTypeSSO, Name: "coalesced", SourceKey: "coalesced", EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: accountdomain.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &blockingQuotaLogAdapter{rateLimitConsoleQuotaAdapter: rateLimitConsoleQuotaAdapter{err: provider.ErrEgressBlocked}, started: make(chan struct{}), release: make(chan struct{})}
	service := NewService(repo, nil, nil, nil, provider.NewRegistry(adapter), nil, nil)
	var output bytes.Buffer
	service.SetLogger(slog.New(slog.NewJSONHandler(&output, nil)))
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = service.RefreshQuota(requestmeta.WithRequestID(ctx, "background-request"), value.ID)
	}()
	<-adapter.started
	go func() {
		defer wg.Done()
		_, _ = service.RefreshQuota(requestmeta.WithRequestID(ctx, "manual-request"), value.ID)
	}()
	time.Sleep(20 * time.Millisecond)
	close(adapter.release)
	wg.Wait()
	if adapter.calls.Load() != 1 {
		t.Fatalf("quota calls=%d", adapter.calls.Load())
	}
	if strings.Count(output.String(), "account_quota_sync_failed") != 2 || !strings.Contains(output.String(), `"request_id":"manual-request"`) || !strings.Contains(output.String(), `"request_id":"background-request"`) {
		t.Fatalf("events=%s", output.String())
	}
}
