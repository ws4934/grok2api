package observability

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/systemlog"
	"github.com/chenyme/grok2api/backend/internal/pkg/diagnostic"
)

type LogSink interface{ Enqueue(systemlog.Entry) }

// WithSystemLogs preserves the caller's logger and adds a bounded, redacted
// operational log stream. The HTTP middleware owns access-log filtering.
func WithSystemLogs(logger *slog.Logger, sink LogSink, instanceID string) *slog.Logger {
	if logger == nil {
		logger = slog.Default()
	}
	stored := slog.NewJSONHandler(&logWriter{sink: sink, instanceID: diagnostic.Text(instanceID, 128)}, &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(&logHandler{base: logger.Handler(), stored: stored})
}

type logHandler struct{ base, stored slog.Handler }

func (h *logHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.base.Enabled(ctx, level) || h.stored.Enabled(ctx, level)
}
func (h *logHandler) Handle(ctx context.Context, record slog.Record) error {
	var err error
	if h.base.Enabled(ctx, record.Level) {
		err = h.base.Handle(ctx, record)
	}
	if h.stored.Enabled(ctx, record.Level) {
		_ = h.stored.Handle(ctx, record)
	}
	return err
}
func (h *logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &logHandler{base: h.base.WithAttrs(attrs), stored: h.stored.WithAttrs(attrs)}
}
func (h *logHandler) WithGroup(name string) slog.Handler {
	return &logHandler{base: h.base.WithGroup(name), stored: h.stored.WithGroup(name)}
}

type logWriter struct {
	sink       LogSink
	instanceID string
}

func (w *logWriter) Write(data []byte) (int, error) {
	var raw map[string]any
	if json.Unmarshal(data, &raw) != nil {
		return len(data), nil
	}
	message, _ := raw["msg"].(string)
	createdAt, _ := time.Parse(time.RFC3339Nano, stringField(raw, "time"))
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	entry := systemlog.Entry{CreatedAt: createdAt, Level: diagnostic.Text(stringField(raw, "level"), 8), Message: diagnostic.Text(message, 2048), InstanceID: w.instanceID, Provider: diagnostic.Text(stringField(raw, "provider"), 64), RequestID: diagnostic.Text(stringField(raw, "request_id"), 64)}
	if id, ok := raw["account_id"].(float64); ok && id > 0 && id < 1<<53 {
		entry.AccountID = uint64(id)
	}
	delete(raw, "time")
	delete(raw, "level")
	delete(raw, "msg")
	entry.Fields = boundedFields(raw)
	w.sink.Enqueue(entry)
	return len(data), nil
}
func stringField(fields map[string]any, key string) string {
	value, _ := fields[key].(string)
	return value
}

func boundedFields(raw map[string]any) map[string]any {
	out := map[string]any{}
	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for index, key := range keys {
		if index >= 64 {
			out["_truncated"] = true
			break
		}
		safeKey := diagnostic.Text(key, 128)
		out[safeKey] = redactValue(key, raw[key], 0)
		encoded, _ := json.Marshal(out)
		if len(encoded) > 8192 {
			delete(out, safeKey)
			out["_truncated"] = true
			break
		}
	}
	return out
}
func redactValue(key string, value any, depth int) any {
	if diagnostic.SensitiveKey(key) {
		return "[REDACTED]"
	}
	if depth > 5 {
		return "[TRUNCATED]"
	}
	switch typed := value.(type) {
	case string:
		return diagnostic.Text(typed, 2048)
	case map[string]any:
		out := map[string]any{}
		keys := make([]string, 0, len(typed))
		for k := range typed {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for index, k := range keys {
			if index >= 32 {
				out["_truncated"] = true
				break
			}
			out[diagnostic.Text(k, 128)] = redactValue(k, typed[k], depth+1)
		}
		return out
	case []any:
		out := make([]any, 0, min(len(typed), 32))
		for _, v := range typed[:min(len(typed), 32)] {
			out = append(out, redactValue("", v, depth+1))
		}
		if len(typed) > 32 {
			out = append(out, "[TRUNCATED]")
		}
		return out
	default:
		return value
	}
}
