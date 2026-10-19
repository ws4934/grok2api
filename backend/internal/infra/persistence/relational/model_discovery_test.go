package relational

import (
	"context"
	"os"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
)

// dialectDatabases runs a test on SQLite and, when configured, PostgreSQL.
func dialectDatabases(t *testing.T, run func(*testing.T, *Database)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) { run(t, openTestDatabase(t)) })
	t.Run("postgres", func(t *testing.T) {
		dsn := os.Getenv("TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("TEST_POSTGRES_DSN is not configured")
		}
		db, err := OpenPostgres(context.Background(), dsn, 8, 8)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if err := db.InitializeSchema(context.Background()); err != nil {
			t.Fatal(err)
		}
		run(t, db)
	})
}

func TestChatGPTImageAndVoiceModelsNeverBecomeChatRoutes(t *testing.T) {
	dialectDatabases(t, func(t *testing.T, db *Database) {
		ctx := context.Background()
		repo := NewModelRepository(db)
		if err := repo.UpsertDiscovered(ctx, account.ProviderChatGPT, []string{"gpt-6", "gpt-image-2.5", "gpt-image-2.5-sunburst", "codex-gpt-image-2.5", "chatgpt-tts", "chatgpt-transcribe"}); err != nil {
			t.Fatal(err)
		}
		// PostgreSQL tests share one database, so only this test's models are checked.
		var chatRoutes []modelRouteModel
		if err := db.db.WithContext(ctx).Where("provider = ? AND capability = ? AND upstream_model IN ?", account.ProviderChatGPT, model.CapabilityResponses,
			[]string{"gpt-6", "gpt-image-2.5", "gpt-image-2.5-sunburst", "codex-gpt-image-2.5", "chatgpt-tts", "chatgpt-transcribe"}).Find(&chatRoutes).Error; err != nil {
			t.Fatal(err)
		}
		if len(chatRoutes) != 1 || chatRoutes[0].UpstreamModel != "gpt-6" {
			t.Fatalf("chat routes = %+v, want only gpt-6", chatRoutes)
		}
	})
}
