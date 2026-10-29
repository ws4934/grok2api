package observability

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/systemlog"
)

type collectingSink struct{ entries []systemlog.Entry }

func (s *collectingSink) Enqueue(entry systemlog.Entry) { s.entries = append(s.entries, entry) }

func TestSystemLogsRedactBoundPreserveContextAndStdout(t *testing.T) {
	var stdout bytes.Buffer
	sink := &collectingSink{}
	logger := WithSystemLogs(slog.New(slog.NewJSONHandler(&stdout, nil)), sink, "node-1").With("account_id", 42, "provider", "console", "request_id", "req-1")
	logger.Warn("account_quota_sync_failed", "error", "GET https://user:secret-value@upstream.test/path?signature=secret-value", "headers", map[string]any{"Authorization": "Bearer secret-value", "SSO": "secret-value"}, "payload", map[string]any{"workosSession": "secret-value", "nested": []any{map[string]any{"api_key": "secret-value"}}})
	if len(sink.entries) != 1 {
		t.Fatalf("entries = %d", len(sink.entries))
	}
	entry := sink.entries[0]
	encoded, _ := json.Marshal(entry)
	if strings.Contains(string(encoded), "secret-value") {
		t.Fatalf("leak: %s", encoded)
	}
	if entry.AccountID != 42 || entry.Provider != "console" || entry.RequestID != "req-1" || entry.InstanceID != "node-1" || entry.Level != "WARN" {
		t.Fatalf("context = %+v", entry)
	}
	if !strings.Contains(stdout.String(), "account_quota_sync_failed") {
		t.Fatal("original logger lost")
	}
	logger.WithGroup("group").Info("grouped", "accessToken", "secret-value", "safe", "retained")
	encoded, _ = json.Marshal(sink.entries[1])
	if strings.Contains(string(encoded), "secret-value") || !strings.Contains(string(encoded), "retained") {
		t.Fatalf("group: %s", encoded)
	}
	logger.Info("large", "large", strings.Repeat("中", 100000), "nested", []any{strings.Repeat("x", 50000)})
	encoded, _ = json.Marshal(sink.entries[2].Fields)
	if len(encoded) > 8220 {
		t.Fatalf("fields bytes=%d", len(encoded))
	}
}
func TestFailedStaticAssetRequestsRemainVisible(t *testing.T) {
	sink := &collectingSink{}
	logger := WithSystemLogs(slog.New(slog.DiscardHandler), sink, "node")
	logger.Error("http_request", "path", "/assets/missing.js", "status", 404)
	if len(sink.entries) != 1 {
		t.Fatalf("entries=%d", len(sink.entries))
	}
}
