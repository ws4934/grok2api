package relational

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestAccountDisplayNamesAndHistoricalSearch(t *testing.T) {
	verifyAccountDisplayNames(t, openTestDatabase(t))
}
func TestPostgresAccountDisplayNamesAndHistoricalSearch(t *testing.T) {
	verifyAccountDisplayNames(t, openPostgresTestDatabase(t))
}
func verifyAccountDisplayNames(t *testing.T, db *Database) {
	t.Helper()
	ctx := context.Background()
	repo := NewAccountRepository(db)
	create := func(provider account.Provider, name, email, source string) account.Credential {
		t.Helper()
		value, _, err := repo.UpsertByIdentity(ctx, account.Credential{Provider: provider, AuthType: account.AuthTypeSSO, Name: name, Email: email, SourceKey: source, EncryptedAccessToken: "encrypted", AuthStatus: account.AuthStatusActive})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	web := create(account.ProviderWeb, "Grok Web 219ff735", "web-label@example.test", "label-web")
	console := create(account.ProviderConsole, "Grok Console 219ff735", "", "label-console")
	unlinked := create(account.ProviderConsole, "Grok Console 1234abcd", "", "label-unlinked")
	custom := create(account.ProviderConsole, "生产账号", "custom-label@example.test", "label-custom")
	if err := db.db.Create(&webConsoleAccountLinkModel{WebAccountID: web.ID, ConsoleAccountID: console.ID, CreatedAt: time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}
	ids := []uint64{web.ID, console.ID, unlinked.ID, custom.ID, web.ID, 0}
	for i := range 520 {
		ids = append(ids, uint64(900000+i))
	}
	names, err := repo.DisplayNames(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[uint64]string{web.ID: web.Email, console.ID: web.Email, unlinked.ID: unlinked.Name, custom.ID: custom.Name} {
		if names[id] != want {
			t.Fatalf("id=%d name=%q want=%q", id, names[id], want)
		}
	}
	if len(names) != 4 {
		t.Fatalf("names=%v", names)
	}
	audits := NewAuditRepository(db)
	now := time.Now().UTC()
	if err := audits.Create(ctx, audit.Record{RequestID: "label-history", ClientKeyID: 1, ModelRouteID: 1, AccountID: &console.ID, AccountName: console.Name, StatusCode: 200, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	rows, _, err := audits.ListCursor(ctx, repository.AuditCursorQuery{Limit: 20, Filter: repository.AuditListFilter{Account: web.Email}})
	if err != nil || len(rows) != 1 || rows[0].AccountName != console.Name {
		t.Fatalf("history=%+v err=%v", rows, err)
	}
	// Updates to current email must immediately change the read label, not history.
	if err := db.db.Model(&accountModel{}).Where("id = ?", console.ID).Update("email", "own-label@example.test").Error; err != nil {
		t.Fatal(err)
	}
	names, err = repo.DisplayNames(ctx, []uint64{console.ID})
	if err != nil || names[console.ID] != "own-label@example.test" {
		t.Fatalf("names=%v err=%v", names, err)
	}
	stored, err := audits.Get(ctx, rows[0].ID)
	if err != nil || stored.AccountName != console.Name {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
}
func TestMediaJobSnapshotClipsLongEmailWithoutFailingGeneration(t *testing.T) {
	db := openTestDatabase(t)
	ctx := context.Background()
	key := clientKeyModel{Name: "long-mail", Prefix: "long-mail", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 60, MaxConcurrent: 4}
	if err := db.db.Create(&key).Error; err != nil {
		t.Fatal(err)
	}
	job := testMediaJob("video_long_email", 0, key.ID, "completed", time.Now().UTC())
	job.AccountName = strings.Repeat("a", 180) + "@example.test"
	if err := NewMediaJobRepository(db).CreateMediaJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	stored, err := NewMediaJobRepository(db).GetMediaJob(ctx, job.ID, key.ID)
	if err != nil || len(stored.AccountName) != 160 {
		t.Fatalf("name=%q err=%v", stored.AccountName, err)
	}
}

func TestDisplayNameSQLMatchesNativeLabelsAndSorting(t *testing.T) {
	verifyDisplayNameSQLMatchesNativeLabelsAndSorting(t, openTestDatabase(t))
}
func TestPostgresDisplayNameSQLMatchesNativeLabelsAndSorting(t *testing.T) {
	verifyDisplayNameSQLMatchesNativeLabelsAndSorting(t, openPostgresTestDatabase(t))
}
func verifyDisplayNameSQLMatchesNativeLabelsAndSorting(t *testing.T, db *Database) {
	t.Helper()
	ctx := context.Background()
	repo := NewAccountRepository(db)
	inputs := []account.Credential{
		{Provider: account.ProviderWeb, Name: "Grok Web aaaaaaaa", Email: "z-sort@example.test", SourceKey: "sort-z"},
		{Provider: account.ProviderConsole, Name: "Grok Console bbbbbbbb", Email: "a-sort@example.test", SourceKey: "sort-a"},
		{Provider: account.ProviderChatGPT, Name: "chatgpt-cbb90abd", Email: "c-sort@example.test", SourceKey: "sort-gpt"},
		{Provider: account.ProviderConsole, Name: "Grok Console primary", Email: "invisible-sort@example.test", SourceKey: "sort-custom"},
	}
	ids := make([]uint64, 0, len(inputs))
	for i := range inputs {
		inputs[i].EncryptedAccessToken = "encrypted"
		inputs[i].AuthStatus = account.AuthStatusActive
		stored, _, err := repo.UpsertByIdentity(ctx, inputs[i])
		if err != nil {
			t.Fatal(err)
		}
		inputs[i].ID = stored.ID
		ids = append(ids, stored.ID)
	}
	labels, err := repo.DisplayNames(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range inputs {
		if labels[value.ID] != value.DisplayName() {
			t.Fatalf("SQL=%q native=%q", labels[value.ID], value.DisplayName())
		}
	}
	page, _, err := repo.List(ctx, repository.AccountListQuery{Page: repository.PageQuery{Limit: 10, Sort: repository.SortQuery{Field: "name", Direction: repository.SortAscending}}, Filter: repository.AccountListFilter{RestrictIDs: true, AccountIDs: ids}})
	if err != nil || len(page) != 4 || page[0].ID != inputs[1].ID || page[1].ID != inputs[2].ID || page[2].ID != inputs[3].ID || page[3].ID != inputs[0].ID {
		t.Fatalf("account sort=%+v err=%v", page, err)
	}
	key := clientKeyModel{Name: "sort-labels", Prefix: "sort-labels", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 60, MaxConcurrent: 4}
	if err := db.db.Create(&key).Error; err != nil {
		t.Fatal(err)
	}
	jobs := NewMediaJobRepository(db)
	for i := range 2 {
		job := testMediaJob("video_sort_labels_"+string(rune('a'+i)), inputs[i].ID, key.ID, "completed", time.Now().UTC())
		job.AccountName = inputs[i].Name
		if err := jobs.CreateMediaJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	sorted, _, err := jobs.ListMediaJobs(ctx, repository.MediaJobListQuery{Page: repository.PageQuery{Limit: 10, Search: "video_sort_labels_", Sort: repository.SortQuery{Field: "account", Direction: repository.SortAscending}}})
	if err != nil || len(sorted) != 2 || sorted[0].AccountID != inputs[1].ID {
		t.Fatalf("job sort=%+v err=%v", sorted, err)
	}
}
