package relational

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
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

func TestInitializeSchemaRetiresChannelTables(t *testing.T) {
	dialectDatabases(t, func(t *testing.T, db *Database) {
		ctx := context.Background()
		accounts, models := NewAccountRepository(db), NewModelRepository(db)
		create := func(provider account.Provider, authType account.AuthType, user string) account.Credential {
			value, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: provider, AuthType: authType, Name: user, UserID: user, SourceKey: user, EncryptedAccessToken: testEncryptedToken, Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1})
			if err != nil {
				t.Fatal(err)
			}
			return value
		}
		legacy := create(account.ProviderChatGPT, "channel_v1", "retired-channel-user")
		native := create(account.ProviderChatGPT, account.AuthTypeOAuth, "native-chatgpt-user")
		grok := create(account.ProviderWeb, account.AuthTypeSSO, "retired-channel-grok-user")
		manual, err := models.Create(ctx, model.Route{Provider: account.ProviderChatGPT, PublicID: "retired-channel-route", UpstreamModel: "gpt-5", Capability: model.CapabilityResponses, Origin: model.OriginManual, Enabled: true}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := models.UpsertDiscovered(ctx, account.ProviderChatGPT, []string{"gpt-5"}); err != nil {
			t.Fatal(err)
		}
		// A minimal stand-in for the channel schema, including a child row that
		// blocks deleting its account until the channel tables are gone.
		for _, statement := range []string{
			"CREATE TABLE channel_settings (provider VARCHAR(32) PRIMARY KEY)",
			"CREATE TABLE channel_accounts (account_id BIGINT PRIMARY KEY REFERENCES provider_accounts(id) ON DELETE CASCADE)",
			"CREATE TABLE channel_image_jobs (id VARCHAR(64) PRIMARY KEY)",
			"CREATE TABLE channel_image_items (id VARCHAR(64) PRIMARY KEY, job_id VARCHAR(64) REFERENCES channel_image_jobs(id) ON DELETE CASCADE, account_id BIGINT REFERENCES provider_accounts(id))",
			"INSERT INTO channel_image_jobs (id) VALUES ('job')",
		} {
			if err := db.db.Exec(statement).Error; err != nil {
				t.Fatal(err)
			}
		}
		if err := db.db.Exec("INSERT INTO channel_image_items (id, job_id, account_id) VALUES ('item', 'job', ?)", legacy.ID).Error; err != nil {
			t.Fatal(err)
		}

		for range 2 {
			if err := db.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
		}
		for _, table := range retiredChannelTables {
			if db.db.Migrator().HasTable(table) {
				t.Fatalf("%s was not dropped", table)
			}
		}
		if _, err := accounts.Get(ctx, legacy.ID); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("channel account kept: %v", err)
		}
		for _, kept := range []account.Credential{native, grok} {
			if _, err := accounts.Get(ctx, kept.ID); err != nil {
				t.Fatalf("account %s removed: %v", kept.Name, err)
			}
		}
		if _, err := models.Get(ctx, manual.ID); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("channel route kept: %v", err)
		}
		var discovered int64
		if err := db.db.Model(&modelRouteModel{}).Where("provider = ? AND origin = ?", account.ProviderChatGPT, model.OriginDiscovered).Count(&discovered).Error; err != nil || discovered != 1 {
			t.Fatalf("discovered routes = %d, %v", discovered, err)
		}
	})
}

func TestChatGPTImageModelsNeverBecomeChatRoutes(t *testing.T) {
	dialectDatabases(t, func(t *testing.T, db *Database) {
		ctx := context.Background()
		// A route created by an earlier release that classified only gpt-image-2.
		legacy := modelRouteModel{PublicID: "chatgpt/gpt-image-2.5-flare", Provider: string(account.ProviderChatGPT), UpstreamModel: "gpt-image-2.5-flare", Capability: string(model.CapabilityResponses), Origin: string(model.OriginDiscovered), Enabled: true}
		if err := db.db.WithContext(ctx).Create(&legacy).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.InitializeSchema(ctx); err != nil {
			t.Fatal(err)
		}
		repo := NewModelRepository(db)
		if err := repo.UpsertDiscovered(ctx, account.ProviderChatGPT, []string{"gpt-6", "gpt-image-2.5", "gpt-image-2.5-sunburst"}); err != nil {
			t.Fatal(err)
		}
		var rows []modelRouteModel
		if err := db.db.WithContext(ctx).Where("provider = ?", account.ProviderChatGPT).Find(&rows).Error; err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.Capability == string(model.CapabilityResponses) && row.UpstreamModel != "gpt-6" {
				t.Fatalf("image model became a chat route: %+v", row)
			}
		}
	})
}
