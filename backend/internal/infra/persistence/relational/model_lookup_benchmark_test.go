package relational

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// BenchmarkGetByPublicIDCandidates measures the per-request route lookup with
// 800 routes and 600 accounts that each support only 5 models, so the
// availability predicate has to scan accounts for most routes.
func BenchmarkGetByPublicIDCandidates(b *testing.B) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(b.TempDir(), "lookup.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		b.Fatal(err)
	}
	accounts := NewAccountRepository(database)
	models := NewModelRepository(database)
	upstream := make([]string, 0, 400)
	for i := range 400 {
		upstream = append(upstream, fmt.Sprintf("model-%03d", i))
	}
	for _, p := range []account.Provider{account.ProviderBuild, account.ProviderChatGPT} {
		if err := models.UpsertDiscovered(ctx, p, upstream); err != nil {
			b.Fatal(err)
		}
		for i := range 300 {
			name := fmt.Sprintf("%s-%d", p, i)
			c, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: p, AuthType: account.AuthTypeSSO, Name: name, SourceKey: name,
				EncryptedAccessToken: "e", Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1})
			if err != nil {
				b.Fatal(err)
			}
			if err := models.ReplaceAccountCapabilities(ctx, c.ID, upstream[:5], time.Now()); err != nil {
				b.Fatal(err)
			}
		}
	}
	for b.Loop() {
		if _, err := models.GetByPublicIDCandidates(ctx, "model-001"); err != nil {
			b.Fatal(err)
		}
	}
}
