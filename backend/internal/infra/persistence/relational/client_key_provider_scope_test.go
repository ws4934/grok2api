package relational

import (
	"context"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestClientKeyProviderScopesRoundTrip(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	keys := NewClientKeyRepository(database)
	future := clientkeydomain.ProviderScopeOf(account.ProviderWeb, "chatgpt")
	explicit, err := keys.Create(ctx, clientkeydomain.Key{Name: "explicit", Prefix: "explicit", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, ProviderScope: future})
	if err != nil || !explicit.ProviderScope.Equal(future) {
		t.Fatalf("created explicit scope = %+v, err = %v", explicit.ProviderScope, err)
	}
	all, err := keys.Create(ctx, clientkeydomain.Key{Name: "all", Prefix: "all", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, ProviderScope: clientkeydomain.AllProviders()})
	if err != nil || !all.ProviderScope.All {
		t.Fatalf("created all scope = %+v, err = %v", all.ProviderScope, err)
	}
	byID, err := keys.Get(ctx, explicit.ID)
	if err != nil || !byID.ProviderScope.Equal(future) {
		t.Fatalf("loaded scope = %+v, err = %v", byID.ProviderScope, err)
	}
	if byPrefix, err := keys.GetByPrefix(ctx, "explicit"); err != nil || !byPrefix.ProviderScope.Equal(future) {
		t.Fatalf("auth lookup scope = %+v, err = %v", byPrefix.ProviderScope, err)
	}
	listed, _, err := keys.List(ctx, repository.ClientKeyListQuery{Page: repository.PageQuery{Limit: 20}})
	if err != nil || len(listed) != 2 {
		t.Fatalf("listed = %#v, err = %v", listed, err)
	}
	for _, value := range listed {
		if (value.ID == explicit.ID && !value.ProviderScope.Equal(future)) || (value.ID == all.ID && !value.ProviderScope.All) {
			t.Fatalf("listed scope for %s = %+v", value.Name, value.ProviderScope)
		}
	}
	countRows := func(id uint64) int64 {
		t.Helper()
		var count int64
		if err := database.db.WithContext(ctx).Model(&clientKeyProviderScopeModel{}).Where("client_key_id = ?", id).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		return count
	}
	byID.ProviderScope = clientkeydomain.AllProviders()
	updated, err := keys.Update(ctx, byID)
	if err != nil || !updated.ProviderScope.All || countRows(explicit.ID) != 0 {
		t.Fatalf("scope after switching to all = %+v, err = %v", updated.ProviderScope, err)
	}
	updated.ProviderScope = clientkeydomain.ProviderScopeOf(account.ProviderBuild)
	if updated, err = keys.Update(ctx, updated); err != nil || !updated.ProviderScope.Equal(clientkeydomain.ProviderScopeOf(account.ProviderBuild)) {
		t.Fatalf("scope after switching to a list = %+v, err = %v", updated.ProviderScope, err)
	}
	if err := keys.Delete(ctx, explicit.ID); err != nil {
		t.Fatal(err)
	}
	if count := countRows(explicit.ID); count != 0 {
		t.Fatalf("deleted key kept %d provider scope rows", count)
	}
}
