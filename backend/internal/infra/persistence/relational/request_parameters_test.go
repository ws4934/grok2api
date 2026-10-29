package relational

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/pkg/requestparams"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestAuditParameterPersistence(t *testing.T) {
	verifyAuditParameterPersistence(t, openTestDatabase(t))
}
func TestPostgresAuditParameterPersistence(t *testing.T) {
	verifyAuditParameterPersistence(t, openPostgresTestDatabase(t))
}

func verifyAuditParameterPersistence(t *testing.T, db *Database) {
	t.Helper()
	ctx := context.Background()
	repo := NewAuditRepository(db)
	// Exercise redaction at the durable boundary as well as transport capture.
	parameters := requestparams.Parse(`{"client":{"values":{"api_key":"must-not-persist","seed":999999999999999999,"n":5}},"effective":{"values":{"n":2,"quality":"medium"}},"upstream":{"values":{"enable_nsfw":true}}}`)
	if err := repo.Create(ctx, audit.Record{RequestID: "parameter-persistence", ClientKeyID: 1, ModelRouteID: 1, StatusCode: 400, RequestParameters: parameters, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	rows, _, err := repo.List(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var id uint64
	for _, row := range rows {
		if row.RequestID == "parameter-persistence" {
			id = row.ID
			if row.RequestParameters != nil {
				t.Fatal("list loaded parameter payload")
			}
		}
	}
	if id == 0 {
		t.Fatal("audit missing")
	}
	cursor, _, err := repo.ListCursor(ctx, repository.AuditCursorQuery{Search: "parameter-persistence", Limit: 10})
	if err != nil || len(cursor) != 1 || cursor[0].RequestParameters != nil {
		t.Fatalf("cursor loaded/lost payload: %+v err=%v", cursor, err)
	}
	detail, err := repo.Get(ctx, id)
	if err != nil || detail.RequestParameters == nil {
		t.Fatalf("detail lost payload: %+v %v", detail, err)
	}
	d := detail.RequestParameters.Data()
	if strings.Contains(string(d.Client.Values), "must-not-persist") || !strings.Contains(string(d.Client.Values), `999999999999999999`) || string(d.Upstream.Values) != `{"enable_nsfw":true}` {
		t.Fatalf("parameter persistence mismatch: client=%s upstream=%s", d.Client.Values, d.Upstream.Values)
	}
}
