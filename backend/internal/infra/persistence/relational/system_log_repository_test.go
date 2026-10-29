package relational

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/systemlog"
)

func TestSystemLogRepositoryFiltersCursorAndRetention(t *testing.T) {
	verifySystemLogRepository(t, openTestDatabase(t))
}
func TestPostgresSystemLogRepositoryFiltersCursorAndRetention(t *testing.T) {
	verifySystemLogRepository(t, openPostgresTestDatabase(t))
}
func verifySystemLogRepository(t *testing.T, db *Database) {
	t.Helper()
	ctx := context.Background()
	repo := NewSystemLogRepository(db)
	// Tests may share a temporary PG database; isolate this new feature's rows.
	if err := db.db.Exec("DELETE FROM system_logs").Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	values := make([]systemlog.Entry, 0, 15)
	for i := range 15 {
		level := "INFO"
		if i%2 == 0 {
			level = "WARN"
		}
		values = append(values, systemlog.Entry{CreatedAt: now.Add(time.Duration(i) * time.Second), Level: level, Message: "quota_100%_failure", InstanceID: "node", Provider: "console", AccountID: 42, RequestID: "req-" + strconv.Itoa(i), Fields: map[string]any{"stage": "upstream", "error": "Cloudflare blocked"}})
	}
	values[0].CreatedAt = now.AddDate(0, 0, -10)
	if err := repo.AppendSystemLogs(ctx, values); err != nil {
		t.Fatal(err)
	}
	first, more, err := repo.ListSystemLogs(ctx, systemlog.Query{Limit: 4, Provider: "console", AccountID: 42, Level: "WARN", Search: "100%_failure"})
	if err != nil || !more || len(first) != 4 || first[0].RequestID != "req-14" || first[3].RequestID != "req-8" {
		t.Fatalf("first=%+v more=%v err=%v", first, more, err)
	}
	second, more, err := repo.ListSystemLogs(ctx, systemlog.Query{Limit: 4, BeforeID: first[3].ID, Level: "WARN"})
	if err != nil || more || len(second) != 3 || second[0].RequestID != "req-6" || second[2].RequestID != "req-2" {
		t.Fatalf("second=%+v more=%v err=%v", second, more, err)
	}
	for _, query := range []systemlog.Query{{Limit: 20, Search: "100X"}, {Limit: 20, Provider: "web"}, {Limit: 20, AccountID: 99}, {Limit: 20, RequestID: "nonexistent"}} {
		rows, _, err := repo.ListSystemLogs(ctx, query)
		if err != nil || len(rows) != 0 {
			t.Fatalf("query=%+v rows=%v err=%v", query, rows, err)
		}
	}
	exact, _, err := repo.ListSystemLogs(ctx, systemlog.Query{Limit: 20, RequestID: "req-4", Since: now})
	if err != nil || len(exact) != 1 || exact[0].Fields["error"] != "Cloudflare blocked" {
		t.Fatalf("exact=%+v err=%v", exact, err)
	}
	if err := repo.PruneSystemLogs(ctx, now.AddDate(0, 0, -7), 10); err != nil {
		t.Fatal(err)
	}
	rows, more, err := repo.ListSystemLogs(ctx, systemlog.Query{Limit: 20})
	if err != nil || more || len(rows) != 10 || rows[9].RequestID != "req-5" {
		t.Fatalf("retained=%+v more=%v err=%v", rows, more, err)
	}
}
func TestSystemLogsSurviveDatabaseReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "restart.db")
	db, err := OpenSQLite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err := NewSystemLogRepository(db).AppendSystemLogs(ctx, []systemlog.Entry{{CreatedAt: time.Now().UTC(), Level: "ERROR", Message: "persisted", Fields: map[string]any{}}}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = OpenSQLite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, _, err := NewSystemLogRepository(db).ListSystemLogs(ctx, systemlog.Query{Limit: 50})
	if err != nil || len(rows) != 1 || rows[0].Message != "persisted" {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
}

func TestSystemLogAppendCapIsAtomicWhenPruningFails(t *testing.T) {
	db := openTestDatabase(t)
	repo := NewSystemLogRepository(db)
	ctx := context.Background()
	values := make([]systemlog.Entry, systemlog.MaxEntries+5)
	for i := range values {
		values[i] = systemlog.Entry{CreatedAt: time.Now().UTC(), Level: "INFO", Message: "bounded", Fields: map[string]any{}}
	}
	if err := repo.AppendSystemLogs(ctx, values); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.db.Model(&systemLogModel{}).Count(&count).Error; err != nil || count != systemlog.MaxEntries {
		t.Fatalf("count=%d err=%v", count, err)
	}
	if err := db.db.Exec(`CREATE TRIGGER system_logs_prune_denied BEFORE DELETE ON system_logs BEGIN SELECT RAISE(ABORT, 'simulated retention failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if err := repo.AppendSystemLogs(ctx, []systemlog.Entry{{CreatedAt: time.Now().UTC(), Level: "ERROR", Message: "must rollback", Fields: map[string]any{}}}); err == nil {
		t.Fatal("prune failure must roll back append")
	}
	if err := db.db.Model(&systemLogModel{}).Count(&count).Error; err != nil || count != systemlog.MaxEntries {
		t.Fatalf("count=%d err=%v", count, err)
	}
	var leaked int64
	db.db.Model(&systemLogModel{}).Where("message = ?", "must rollback").Count(&leaked)
	if leaked != 0 {
		t.Fatal("failed retention left a committed row")
	}
}
