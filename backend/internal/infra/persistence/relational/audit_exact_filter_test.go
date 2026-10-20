package relational

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// "#5" selects key/account 5 only; plain "5" keeps the fuzzy match that also hits 15 and 50.
func TestAuditKeyAndAccountFiltersMatchExactIDs(t *testing.T) {
	ctx := context.Background()
	audits := NewAuditRepository(openTestDatabase(t))
	now := time.Now().UTC()
	var values []audit.Record
	for _, id := range []uint64{5, 15, 50} {
		accountID := id
		values = append(values, audit.Record{RequestID: "id-" + strconv.FormatUint(id, 10), ClientKeyID: id, ClientKeyName: "key", ModelRouteID: 1, ModelPublicID: "public", AccountID: &accountID, AccountName: "account", StatusCode: 200, CreatedAt: now})
	}
	if err := audits.CreateBatch(ctx, values); err != nil {
		t.Fatal(err)
	}
	assertAuditFilterResult(t, ctx, audits, repository.AuditListFilter{Key: "#5"}, "id-5")
	assertAuditFilterResult(t, ctx, audits, repository.AuditListFilter{Account: "#5"}, "id-5")
	for _, filter := range []repository.AuditListFilter{{Key: "5"}, {Account: "5"}} {
		items, _, err := audits.ListCursor(ctx, repository.AuditCursorQuery{Limit: 20, Filter: filter})
		if err != nil || len(items) != 3 {
			t.Fatalf("fuzzy filter %#v items = %#v, err = %v", filter, items, err)
		}
	}
	summary, err := audits.Summarize(ctx, repository.AuditSummaryQuery{Filter: repository.AuditListFilter{Key: "#5", Account: "#5"}})
	if err != nil || summary.Requests != 1 {
		t.Fatalf("exact summary = %#v, err = %v", summary, err)
	}
}
