package relational

import (
	"context"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestAccountListFindsLinkedIdentityEmail(t *testing.T) {
	database := openTestDatabase(t)
	repo, ctx := NewAccountRepository(database), context.Background()
	create := func(p account.Provider, name, email, source string) account.Credential {
		t.Helper()
		c, _, err := repo.UpsertByIdentity(ctx, account.Credential{Provider: p, Name: name, Email: email, SourceKey: source,
			EncryptedAccessToken: testEncryptedToken, Enabled: true, AuthStatus: account.AuthStatusActive})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	web := create(account.ProviderWeb, "Grok Web aaaaaaaa", "Owner@Example.test", "sso:"+strings.Repeat("a", 64))
	console := create(account.ProviderConsole, "Grok Console aaaaaaaa", "", "console-sso:"+strings.Repeat("a", 64))
	create(account.ProviderConsole, "unrelated", "", "unrelated-source")
	if err := repo.ReconcileProviderLinks(ctx, console.ID); err != nil {
		t.Fatal(err)
	}
	values, total, err := repo.List(ctx, repository.AccountListQuery{Page: repository.PageQuery{Limit: 10, Search: "owner@example.test"}, Filter: repository.AccountListFilter{Provider: string(account.ProviderConsole)}})
	if err != nil || total != 1 || len(values) != 1 || values[0].ID != console.ID || len(values[0].LinkedAccounts) != 1 || values[0].LinkedAccounts[0].ID != web.ID {
		t.Fatalf("linked Console search: values=%+v total=%d err=%v", values, total, err)
	}
	webOnly := create(account.ProviderWeb, "Grok Web bbbbbbbb", "", "sso:"+strings.Repeat("b", 64))
	build := create(account.ProviderBuild, "Build primary", "build@example.test", "build-source")
	if err := repo.LinkWebToBuild(ctx, webOnly.ID, build.ID); err != nil {
		t.Fatal(err)
	}
	values, total, err = repo.List(ctx, repository.AccountListQuery{Page: repository.PageQuery{Limit: 10, Search: "build@example.test"}, Filter: repository.AccountListFilter{Provider: string(account.ProviderWeb)}})
	if err != nil || total != 1 || len(values) != 1 || values[0].ID != webOnly.ID {
		t.Fatalf("linked Web search: values=%+v total=%d err=%v", values, total, err)
	}
}
