package app

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/systemlog"
	"github.com/chenyme/grok2api/backend/internal/infra/observability"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
)

type performanceLogSink struct{ entries []systemlog.Entry }

func (s *performanceLogSink) Enqueue(entry systemlog.Entry) { s.entries = append(s.entries, entry) }

func TestPerformanceMetricsAreDebugOnlyAndNotSystemLogEntries(t *testing.T) {
	database, err := relational.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	previous := perfmetrics.Default
	t.Cleanup(func() { perfmetrics.Default = previous })
	for _, level := range []slog.Level{slog.LevelInfo, slog.LevelDebug} {
		t.Run(level.String(), func(t *testing.T) {
			perfmetrics.Default = perfmetrics.NewRegistry()
			perfmetrics.Default.Add("quota_refresh_events", perfmetrics.Labels{Subsystem: "quota", Outcome: "account_missing"}, 1)
			var stdout bytes.Buffer
			sink := &performanceLogSink{}
			logger := observability.WithSystemLogs(slog.New(slog.NewJSONHandler(&stdout, &slog.HandlerOptions{Level: level})), sink, "test")
			app := &Application{database: database, logger: logger}
			app.logPerformanceMetrics()
			if len(sink.entries) != 0 {
				t.Fatalf("periodic metrics persisted %d system log entries", len(sink.entries))
			}
			if level == slog.LevelInfo && stdout.Len() != 0 {
				t.Fatalf("routine metrics logged at INFO: %s", stdout.String())
			}
			if level == slog.LevelDebug && (!bytes.Contains(stdout.Bytes(), []byte(`"level":"DEBUG"`)) || !bytes.Contains(stdout.Bytes(), []byte(`"name":"quota_refresh_events"`))) {
				t.Fatalf("debug metrics lost: %s", stdout.String())
			}
			for _, sample := range perfmetrics.Default.CollectAndReset() {
				if sample.Count != 0 {
					t.Fatalf("interval counter not collected: %+v", sample)
				}
			}
			logger.Warn("quota_refresh_failed", "account_id", 42, "error", "temporary failure")
			if len(sink.entries) != 1 || sink.entries[0].Message != "quota_refresh_failed" {
				t.Fatalf("operational failure lost: %+v", sink.entries)
			}
		})
	}
}
