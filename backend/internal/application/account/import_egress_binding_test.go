package account

import (
	"context"
	"encoding/base64"
	"path/filepath"
	"sync"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

type egressBindingImportAdapter struct {
	rtImportAdapter
	mu    sync.Mutex
	bound map[string]uint64
}

func (*egressBindingImportAdapter) ParseImportedCredentials([]byte) ([]provider.CredentialSeed, error) {
	return []provider.CredentialSeed{
		{Name: "stored", SourceKey: "rt:stored", OIDCClientID: "client", RefreshToken: "stored-rt"},
		{Name: "new", SourceKey: "rt:new", OIDCClientID: "client", RefreshToken: "new-rt"},
	}, nil
}

func (a *egressBindingImportAdapter) PrepareImportedCredential(ctx context.Context, seed provider.CredentialSeed) (provider.CredentialSeed, error) {
	existing, _ := infraegress.ExistingAccount(ctx)
	a.mu.Lock()
	a.bound[seed.SourceKey] = existing.ID
	a.mu.Unlock()
	return a.rtImportAdapter.PrepareImportedCredential(ctx, seed)
}

// Re-importing a stored account must exchange through that account's egress,
// not a fresh pending one.
func TestImportBindsReimportToStoredAccountEgress(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "import-egress.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	stored, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, AuthType: accountdomain.AuthTypeOAuth, Name: "stored", SourceKey: "rt:stored",
		EncryptedAccessToken: "access", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &egressBindingImportAdapter{bound: map[string]uint64{}}
	service := NewService(accounts, nil, nil, nil, provider.NewRegistry(adapter), cipher, nil)
	if _, err := service.ImportCredentialDocumentsWithProgress(ctx, accountdomain.ProviderBuild, [][]byte{[]byte("ignored")}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if adapter.bound["rt:stored"] != stored.ID || adapter.bound["rt:new"] != 0 {
		t.Fatalf("egress bindings = %v, stored account %d", adapter.bound, stored.ID)
	}
}
