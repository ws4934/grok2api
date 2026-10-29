package audit

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/pkg/requestparams"
)

func TestAuditFreezesParametersBeforeEnqueue(t *testing.T) {
	repo := newGatedAuditRepository()
	service := NewService(repo, slog.Default(), 8, 1)
	service.Start()
	ctx, parameters := requestparams.Start(context.Background(), []byte(`{"api_key":"do-not-save","n":5}`))
	requestparams.RecordUpstream(ctx, map[string]any{"enable_nsfw": false})
	done := make(chan error, 1)
	go func() {
		done <- service.Create(ctx, auditdomain.Record{EventID: "evt_parameter_freeze", RequestID: "parameter-freeze", ClientKeyID: 1, ModelRouteID: 1, StatusCode: 400, RequestParameters: parameters})
	}()
	select {
	case records := <-repo.started:
		requestparams.RecordUpstream(ctx, map[string]any{"enable_nsfw": true})
		if len(records) != 1 || records[0].RequestParameters == nil || string(records[0].RequestParameters.Data().Upstream.Values) != `{"enable_nsfw":false}` || strings.Contains(string(records[0].RequestParameters.Data().Client.Values), "do-not-save") {
			t.Errorf("queued snapshot changed: %+v", records)
		}
	case <-time.After(3 * time.Second):
		t.Error("audit did not reach writer")
	}
	close(repo.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	closeAuditService(t, service)
}

func TestRequestMetadataRedactsWebSocketProtocolCredentials(t *testing.T) {
	record := sanitizeRequestMetadata(auditdomain.Record{RequestHeaders: map[string][]string{
		"sEc-WeBsOcKeT-pRoToCoL": {"realtime", "openai-insecure-api-key.test-secret"},
		"User-Agent":             {"audit-test"},
	}})
	if got := record.RequestHeaders["sEc-WeBsOcKeT-pRoToCoL"]; len(got) != 1 || got[0] != "[REDACTED]" {
		t.Fatalf("WebSocket credentials leaked: %+v", record.RequestHeaders)
	}
	if got := record.RequestHeaders["User-Agent"]; len(got) != 1 || got[0] != "audit-test" {
		t.Fatalf("ordinary header lost: %+v", record.RequestHeaders)
	}
}
