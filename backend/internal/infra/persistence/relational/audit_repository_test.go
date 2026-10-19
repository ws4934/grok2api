package relational

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	repositorypkg "github.com/chenyme/grok2api/backend/internal/repository"
)

func TestAuditRepositorySumTokensByAccountsSince(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := NewAuditRepository(database)
	now := time.Now().UTC()
	values := []audit.Record{
		{RequestID: "recent-1", ClientKeyID: 1, ModelRouteID: 1, AccountID: uint64Pointer(1), TotalTokens: 120, StatusCode: 200, CreatedAt: now.Add(-time.Hour)},
		{RequestID: "recent-2", ClientKeyID: 1, ModelRouteID: 1, AccountID: uint64Pointer(1), TotalTokens: 80, StatusCode: 200, CreatedAt: now.Add(-2 * time.Hour)},
		{RequestID: "old", ClientKeyID: 1, ModelRouteID: 1, AccountID: uint64Pointer(1), TotalTokens: 500, StatusCode: 200, CreatedAt: now.Add(-25 * time.Hour)},
		{RequestID: "other", ClientKeyID: 1, ModelRouteID: 1, AccountID: uint64Pointer(2), TotalTokens: 40, StatusCode: 200, CreatedAt: now.Add(-time.Hour)},
	}
	for _, value := range values {
		if err := repository.Create(ctx, value); err != nil {
			t.Fatal(err)
		}
	}
	totals, err := repository.SumTokensByAccountsSince(ctx, []uint64{1, 2}, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if totals[1] != 200 || totals[2] != 40 {
		t.Fatalf("totals = %#v", totals)
	}
}

func TestAuditRepositoryBatchAndCursor(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "audit-cursor.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := NewAuditRepository(database)
	now := time.Now().UTC()
	values := []audit.Record{
		{RequestID: "cursor-old", ClientKeyID: 1, ModelRouteID: 1, StatusCode: 200, CreatedAt: now.Add(-48 * time.Hour)},
		{RequestID: "cursor-1", ClientKeyID: 1, ModelRouteID: 1, StatusCode: 200, CreatedAt: now.Add(-3 * time.Minute)},
		{RequestID: "cursor-2", ClientKeyID: 1, ModelRouteID: 1, StatusCode: 200, CreatedAt: now.Add(-2 * time.Minute)},
		{RequestID: "cursor-3", ClientKeyID: 1, ClientKeyName: "production", ClientIP: "203.0.113.42", ModelRouteID: 1, ModelPublicID: "grok-test", ModelUpstreamModel: "grok-test-upstream", ReasoningEffort: "xhigh", AccountName: "primary", EgressNodeID: uint64Pointer(42), EgressNodeName: "proxy-shanghai", EgressScope: "grok_web", EgressMode: audit.EgressModeProxy, StatusCode: 200, CreatedAt: now.Add(-time.Minute)},
	}
	if err := repository.CreateBatch(ctx, values); err != nil {
		t.Fatal(err)
	}
	sort := repositorypkg.SortQuery{Field: "createdAt", Direction: repositorypkg.SortDescending}
	first, hasMore, err := repository.ListCursor(ctx, repositorypkg.AuditCursorQuery{Limit: 2, Sort: sort})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || !hasMore || first[0].ID <= first[1].ID {
		t.Fatalf("first page = %#v, hasMore = %v", first, hasMore)
	}
	if first[0].ClientKeyName != "production" || first[0].ClientIP != "203.0.113.42" || first[0].ModelPublicID != "grok-test" || first[0].ModelUpstreamModel != "grok-test-upstream" || first[0].ReasoningEffort != "xhigh" || first[0].AccountName != "primary" || first[0].EgressNodeID == nil || *first[0].EgressNodeID != 42 || first[0].EgressNodeName != "proxy-shanghai" || first[0].EgressMode != audit.EgressModeProxy {
		t.Fatalf("audit snapshots = %#v", first[0])
	}
	matched, _, err := repository.ListCursor(ctx, repositorypkg.AuditCursorQuery{Limit: 10, Search: "proxy-shanghai", Sort: sort})
	if err != nil || len(matched) != 1 || matched[0].RequestID != "cursor-3" {
		t.Fatalf("egress search = %#v, err = %v", matched, err)
	}
	matched, _, err = repository.ListCursor(ctx, repositorypkg.AuditCursorQuery{Limit: 10, Search: "203.0.113.42", Sort: sort})
	if err != nil || len(matched) != 1 || matched[0].RequestID != "cursor-3" {
		t.Fatalf("client IP search = %#v, err = %v", matched, err)
	}
	second, _, err := repository.ListCursor(ctx, repositorypkg.AuditCursorQuery{Cursor: &repositorypkg.SortCursor{ID: first[len(first)-1].ID, Value: first[len(first)-1].CreatedAt}, Limit: 2, Sort: sort})
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 2 || second[0].ID >= first[len(first)-1].ID {
		t.Fatalf("second page = %#v", second)
	}
}

func TestAuditRepositoryBatchSpansMultipleSQLChunks(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "audit-multi-chunk.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := NewAuditRepository(database)
	now := time.Now().UTC()
	values := make([]audit.Record, 45)
	for index := range values {
		values[index] = audit.Record{
			EventID: fmt.Sprintf("evt_multi_chunk_audit_%04d", index), RequestID: fmt.Sprintf("multi-chunk-%04d", index),
			ClientKeyID: 1, ModelRouteID: 1, StatusCode: 200, CreatedAt: now.Add(time.Duration(index) * time.Millisecond),
			Attempts: []audit.Attempt{{Number: 1, Source: audit.AttemptSourceCredential, Stage: "credential", StartedAt: now}},
		}
	}
	if err := repository.CreateBatch(ctx, values); err != nil {
		t.Fatal(err)
	}
	if count := tableRowCount(t, database, "request_audits"); count != int64(len(values)) {
		t.Fatalf("request audits = %d", count)
	}
	if count := tableRowCount(t, database, "request_audit_attempts"); count != int64(len(values)) {
		t.Fatalf("request audit attempts = %d", count)
	}
}

func TestAuditRepositoryAtomicallyRecordsClientBillingUsage(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "audit-billing.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	key := clientKeyModel{Name: "billing", Prefix: "billing", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 120, MaxConcurrent: 8}
	if err := database.db.WithContext(ctx).Create(&key).Error; err != nil {
		t.Fatal(err)
	}
	repository := NewAuditRepository(database)
	now := time.Now().UTC()
	if err := repository.CreateBatch(ctx, []audit.Record{
		{RequestID: "reported", ClientKeyID: key.ID, ModelRouteID: 1, StatusCode: 200, CostInUSDTicks: 20, EstimatedCostInUSDTicks: 90, CreatedAt: now},
		{RequestID: "estimated", ClientKeyID: key.ID, ModelRouteID: 1, StatusCode: 200, EstimatedCostInUSDTicks: 30, CreatedAt: now},
		{RequestID: "unpriced", ClientKeyID: key.ID, ModelRouteID: 1, StatusCode: 500, CreatedAt: now},
	}); err != nil {
		t.Fatal(err)
	}
	var stored clientKeyModel
	if err := database.db.WithContext(ctx).First(&stored, key.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.BilledUsageUSDTicks != 50 {
		t.Fatalf("billed usage = %d", stored.BilledUsageUSDTicks)
	}
	if err := repository.Create(ctx, audit.Record{EventID: "evt_idempotent_billing_0001", RequestID: "idempotent", ClientKeyID: key.ID, ModelRouteID: 1, StatusCode: 200, EstimatedCostInUSDTicks: 40, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Create(ctx, audit.Record{EventID: "evt_idempotent_billing_0001", RequestID: "idempotent-retry", ClientKeyID: key.ID, ModelRouteID: 1, StatusCode: 200, EstimatedCostInUSDTicks: 40, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).First(&stored, key.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.BilledUsageUSDTicks != 90 {
		t.Fatalf("idempotent billed usage = %d", stored.BilledUsageUSDTicks)
	}
}

func TestAuditRepositorySettlesBillingReservationIdempotently(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "audit-reservation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	key := clientKeyModel{Name: "reserved", Prefix: "reserved", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 120, MaxConcurrent: 8, BillingLimitUSDTicks: 100}
	if err := database.db.WithContext(ctx).Create(&key).Error; err != nil {
		t.Fatal(err)
	}
	eventID := "evt_billing_reservation_0001"
	keys := NewClientKeyRepository(database)
	if reserved, err := keys.ReserveBillingUsage(ctx, key.ID, eventID, 80, time.Now().UTC().Add(time.Hour)); err != nil || !reserved {
		t.Fatal(err)
	}
	audits := NewAuditRepository(database)
	record := audit.Record{EventID: eventID, RequestID: "reserved-request", ClientKeyID: key.ID, ModelRouteID: 1, StatusCode: 200, EstimatedCostInUSDTicks: 30, CreatedAt: time.Now().UTC()}
	if err := audits.Create(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := audits.Create(ctx, record); err != nil {
		t.Fatal(err)
	}
	var stored clientKeyModel
	if err := database.db.WithContext(ctx).First(&stored, key.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.ReservedUsageUSDTicks != 0 || stored.BilledUsageUSDTicks != 30 {
		t.Fatalf("settled key = reserved %d, billed %d", stored.ReservedUsageUSDTicks, stored.BilledUsageUSDTicks)
	}
	if count := tableRowCount(t, database, "billing_reservations"); count != 0 {
		t.Fatalf("billing reservations = %d", count)
	}
}

func TestAuditRepositoryBatchMixedDuplicateUsesIdempotentFallback(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "audit-mixed-batch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	key := clientKeyModel{Name: "mixed", Prefix: "mixed", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 120, MaxConcurrent: 8}
	if err := database.db.WithContext(ctx).Create(&key).Error; err != nil {
		t.Fatal(err)
	}
	repository := NewAuditRepository(database)
	now := time.Now().UTC()
	existing := audit.Record{
		EventID: "evt_mixed_batch_existing_0001", RequestID: "mixed-existing", ClientKeyID: key.ID, ModelRouteID: 1,
		StatusCode: 200, EstimatedCostInUSDTicks: 20, CreatedAt: now,
		Attempts: []audit.Attempt{{Number: 1, Source: audit.AttemptSourceCredential, Stage: "credential", StartedAt: now}},
	}
	if err := repository.Create(ctx, existing); err != nil {
		t.Fatal(err)
	}
	if err := repository.CreateBatch(ctx, []audit.Record{
		{
			EventID: existing.EventID, RequestID: "duplicate-must-not-rebill", ClientKeyID: key.ID, ModelRouteID: 1,
			StatusCode: 200, EstimatedCostInUSDTicks: 999, CreatedAt: now.Add(time.Second),
			Attempts: []audit.Attempt{{Number: 1, Source: audit.AttemptSourceCredential, Stage: "duplicate", StartedAt: now}},
		},
		{
			EventID: "evt_mixed_batch_new_record_0002", RequestID: "mixed-new", ClientKeyID: key.ID, ModelRouteID: 1,
			StatusCode: 200, EstimatedCostInUSDTicks: 30, CreatedAt: now.Add(2 * time.Second),
			Attempts: []audit.Attempt{
				{Number: 1, Source: audit.AttemptSourceCredential, Stage: "credential", StartedAt: now},
				{Number: 2, Source: audit.AttemptSourceUpstreamHTTP, Stage: "upstream_response", StartedAt: now},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	var stored clientKeyModel
	if err := database.db.WithContext(ctx).First(&stored, key.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.BilledUsageUSDTicks != 50 {
		t.Fatalf("billed usage = %d", stored.BilledUsageUSDTicks)
	}
	if count := tableRowCount(t, database, "request_audits"); count != 2 {
		t.Fatalf("request audits = %d", count)
	}
	if count := tableRowCount(t, database, "request_audit_attempts"); count != 3 {
		t.Fatalf("request audit attempts = %d", count)
	}
}

func TestAuditRepositoryBatchSettlesReservationsAndReleasesZeroCost(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "audit-batch-reservations.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	key := clientKeyModel{Name: "batch-reserved", Prefix: "batch-reserved", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 120, MaxConcurrent: 8, BillingLimitUSDTicks: 1_000}
	if err := database.db.WithContext(ctx).Create(&key).Error; err != nil {
		t.Fatal(err)
	}
	events := []string{"evt_batch_reserved_priced_0001", "evt_batch_reserved_zero_000002"}
	keys := NewClientKeyRepository(database)
	for index, amount := range []int64{80, 70} {
		reserved, err := keys.ReserveBillingUsage(ctx, key.ID, events[index], amount, time.Now().UTC().Add(time.Hour))
		if err != nil || !reserved {
			t.Fatalf("reserve %s: reserved=%v err=%v", events[index], reserved, err)
		}
	}
	audits := NewAuditRepository(database)
	if err := audits.CreateBatch(ctx, []audit.Record{
		{EventID: events[0], RequestID: "batch-priced", ClientKeyID: key.ID, ModelRouteID: 1, StatusCode: 200, CostInUSDTicks: 30, CreatedAt: time.Now().UTC()},
		{EventID: events[1], RequestID: "batch-zero", ClientKeyID: key.ID, ModelRouteID: 1, StatusCode: 500, CreatedAt: time.Now().UTC()},
	}); err != nil {
		t.Fatal(err)
	}
	var stored clientKeyModel
	if err := database.db.WithContext(ctx).First(&stored, key.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.ReservedUsageUSDTicks != 0 || stored.BilledUsageUSDTicks != 30 {
		t.Fatalf("settled key = reserved %d, billed %d", stored.ReservedUsageUSDTicks, stored.BilledUsageUSDTicks)
	}
	if count := tableRowCount(t, database, "billing_reservations"); count != 0 {
		t.Fatalf("billing reservations = %d", count)
	}
}

func TestAuditRepositoryBatchRollsBackAuditBillingAndReservationTogether(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "audit-batch-rollback.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	key := clientKeyModel{Name: "batch-rollback", Prefix: "batch-rollback", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 120, MaxConcurrent: 8, BillingLimitUSDTicks: 1_000}
	if err := database.db.WithContext(ctx).Create(&key).Error; err != nil {
		t.Fatal(err)
	}
	eventID := "evt_batch_rollback_record_0001"
	keys := NewClientKeyRepository(database)
	if reserved, err := keys.ReserveBillingUsage(ctx, key.ID, eventID, 80, time.Now().UTC().Add(time.Hour)); err != nil || !reserved {
		t.Fatalf("reserve: reserved=%v err=%v", reserved, err)
	}
	audits := NewAuditRepository(database)
	err = audits.CreateBatch(ctx, []audit.Record{{
		EventID: eventID, RequestID: "batch-rollback", ClientKeyID: key.ID, ModelRouteID: 1,
		StatusCode: 200, CostInUSDTicks: 30, CreatedAt: time.Now().UTC(),
		Attempts: []audit.Attempt{{Number: 0, Source: audit.AttemptSourceCredential, Stage: "invalid", StartedAt: time.Now().UTC()}},
	}})
	if !errors.Is(err, repositorypkg.ErrInvalidRecord) {
		t.Fatalf("batch error = %v", err)
	}
	var invalid *repositorypkg.InvalidBatchRecordError
	if !errors.As(err, &invalid) || invalid.Index != 0 {
		t.Fatalf("invalid record error = %#v", invalid)
	}
	if count := tableRowCount(t, database, "request_audits"); count != 0 {
		t.Fatalf("request audits = %d", count)
	}
	if count := tableRowCount(t, database, "request_audit_attempts"); count != 0 {
		t.Fatalf("request audit attempts = %d", count)
	}
	if count := tableRowCount(t, database, "billing_reservations"); count != 1 {
		t.Fatalf("billing reservations = %d", count)
	}
	var stored clientKeyModel
	if err := database.db.WithContext(ctx).First(&stored, key.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.ReservedUsageUSDTicks != 80 || stored.BilledUsageUSDTicks != 0 {
		t.Fatalf("rolled back key = reserved %d, billed %d", stored.ReservedUsageUSDTicks, stored.BilledUsageUSDTicks)
	}
}

func TestAuditRepositoryAllowsRepeatedExternalRequestIDs(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "audit-request-id.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := NewAuditRepository(database)
	now := time.Now().UTC()
	if err := repository.CreateBatch(ctx, []audit.Record{
		{RequestID: "caller-reused-id", ClientKeyID: 1, ModelRouteID: 1, StatusCode: 200, CreatedAt: now},
		{RequestID: "caller-reused-id", ClientKeyID: 1, ModelRouteID: 1, StatusCode: 200, CreatedAt: now.Add(time.Second)},
	}); err != nil {
		t.Fatal(err)
	}
	_, total, err := repository.List(ctx, 0, 10)
	if err != nil || total != 2 {
		t.Fatalf("total = %d, err = %v", total, err)
	}
}

func TestAuditRepositoryRoundTripsFailureAttempts(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "audit-attempts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := NewAuditRepository(database)
	now := time.Now().UTC()
	status := http.StatusBadGateway
	record := audit.Record{
		EventID: "evt_failure_attempts_0001", RequestID: "failure-attempts", ClientKeyID: 1, ModelRouteID: 1,
		StatusCode: http.StatusBadGateway, CreatedAt: now,
		Attempts: []audit.Attempt{
			{
				Number: 1, Source: audit.AttemptSourceTransport, Stage: "dns_lookup", AccountID: uint64Pointer(7), AccountName: "primary",
				Method: http.MethodPost, RequestPath: "/responses", UpstreamURL: "https://api.example.test/v1/responses", StartedAt: now.Add(-2 * time.Second), DurationMS: 125,
				TransportError: "lookup api.example.test: no such host", ErrorChain: []audit.ErrorFrame{{Type: "*url.Error", Message: "Post request failed"}, {Type: "*net.DNSError", Message: "no such host"}},
			},
			{
				Number: 2, Source: audit.AttemptSourceUpstreamHTTP, Stage: "upstream_response", AccountID: uint64Pointer(8), AccountName: "secondary",
				Method: http.MethodPost, RequestPath: "/responses", UpstreamURL: "https://api.example.test/v1/responses", StartedAt: now.Add(-time.Second), DurationMS: 250,
				UpstreamStatusCode: &status, UpstreamStatus: "502 Bad Gateway", ResponseHeaders: http.Header{"Content-Type": {"application/json"}, "X-Upstream": {"edge-a", "edge-b"}},
				ResponseBody: []byte{'{', '"', 'e', 'r', 'r', 'o', 'r', '"', ':', ' ', '"', 'f', 'a', 'i', 'l', 'e', 'd', '"', '}', 0xff}, ResponseBodyTruncated: true,
			},
		},
	}
	if err := repository.Create(ctx, record); err != nil {
		t.Fatal(err)
	}
	stored, err := repository.Get(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if stored.AttemptCount != 2 || len(stored.Attempts) != 2 {
		t.Fatalf("attempt count = %d, attempts = %#v", stored.AttemptCount, stored.Attempts)
	}
	if stored.Attempts[0].Number != 1 || stored.Attempts[0].Stage != "dns_lookup" || len(stored.Attempts[0].ErrorChain) != 2 {
		t.Fatalf("transport attempt = %#v", stored.Attempts[0])
	}
	httpAttempt := stored.Attempts[1]
	if httpAttempt.Number != 2 || httpAttempt.UpstreamStatusCode == nil || *httpAttempt.UpstreamStatusCode != status || string(httpAttempt.ResponseBody) != string(record.Attempts[1].ResponseBody) || !httpAttempt.ResponseBodyTruncated || len(httpAttempt.ResponseHeaders["X-Upstream"]) != 2 {
		t.Fatalf("HTTP attempt = %#v", httpAttempt)
	}
	if err := repository.Create(ctx, record); err != nil {
		t.Fatal(err)
	}
	if count := tableRowCount(t, database, "request_audit_attempts"); count != 2 {
		t.Fatalf("idempotent attempts = %d", count)
	}
	if _, err := repository.Get(ctx, 999); !errors.Is(err, repositorypkg.ErrNotFound) {
		t.Fatalf("missing audit error = %v", err)
	}
}

func TestAuditRepositoryNormalizesUntrustedUsage(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "audit-normalize.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := NewAuditRepository(database)
	negativeFirstToken := int64(-4)
	if err := repository.Create(ctx, audit.Record{RequestID: "normalize", ClientKeyID: 1, ModelRouteID: 1, StatusCode: 200, Streaming: true, ReasoningEffort: "customer@example.com", MediaInputImages: -1, MediaOutputImages: -2, MediaOutputSeconds: -3, InputTokens: -1, TotalTokens: -2, FirstTokenMS: &negativeFirstToken, DurationMS: -3, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	values, _, err := repository.List(ctx, 0, 1)
	if err != nil || len(values) != 1 {
		t.Fatalf("values = %#v, err = %v", values, err)
	}
	if values[0].ReasoningEffort != "" || values[0].MediaInputImages != 0 || values[0].MediaOutputImages != 0 || values[0].MediaOutputSeconds != 0 || values[0].InputTokens != 0 || values[0].TotalTokens != 0 || values[0].FirstTokenMS == nil || *values[0].FirstTokenMS != 0 || values[0].DurationMS != 0 {
		t.Fatalf("normalized audit = %#v", values[0])
	}
	if err := repository.Create(ctx, audit.Record{RequestID: "invalid-ip", ClientKeyID: 1, ClientIP: "203.0.113.4 injected", ModelRouteID: 1, StatusCode: 200, CreatedAt: time.Now().UTC()}); err == nil {
		t.Fatal("invalid client IP should be rejected")
	}
}

func TestAuditRepositorySummaryAppliesRangeAndGroupsPricingTier(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "audit-summary.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := NewAuditRepository(database)
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	values := []audit.Record{
		{RequestID: "standard", ClientKeyID: 1, ModelRouteID: 1, ModelPublicID: "public", ModelUpstreamModel: "grok-build-0.1", StatusCode: 200, Streaming: true, InputTokens: 100, CachedInputTokens: 20, OutputTokens: 50, ReasoningTokens: 10, TotalTokens: 150, EstimatedCostInUSDTicks: 1_840_000, PricingModel: "grok-build-0.1", PricingVersion: "2026-07-11", DurationMS: 100, CreatedAt: now.Add(-time.Hour)},
		{RequestID: "long", ClientKeyID: 1, ModelRouteID: 1, ModelPublicID: "public", ModelUpstreamModel: "grok-build-0.1", StatusCode: 500, Streaming: false, InputTokens: 210_000, ContextInputTokens: 210_000, OutputTokens: 100, TotalTokens: 210_100, DurationMS: 300, CreatedAt: now.Add(-2 * time.Hour)},
		{RequestID: "outside", ClientKeyID: 1, ModelRouteID: 1, ModelPublicID: "public", ModelUpstreamModel: "grok-build-0.1", StatusCode: 200, TotalTokens: 999, CreatedAt: now.Add(-8 * 24 * time.Hour)},
	}
	if err := repository.CreateBatch(ctx, values); err != nil {
		t.Fatal(err)
	}
	summary, err := repository.Summarize(ctx, repositorypkg.AuditSummaryQuery{Search: "public", Start: now.Add(-7 * 24 * time.Hour), End: now})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Requests != 2 || summary.SuccessfulRequests != 1 || summary.FailedRequests != 1 || summary.TotalTokens != 210_250 || summary.DurationMS != 400 {
		t.Fatalf("summary = %#v", summary)
	}
	if summary.EstimatedCostInUSDTicks != 1_840_000 || summary.PricedRequests != 1 || summary.UnpricedRequests != 1 || summary.PricedTokens != 150 || summary.UnpricedTokens != 210_100 {
		t.Fatalf("summary pricing = %#v", summary)
	}
}

func TestAuditRepositoryStreamFailureKeepsHTTPStatusAndFiltersAsOther(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "audit-stream-failure.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := NewAuditRepository(database)
	now := time.Now().UTC()
	values := []audit.Record{
		{RequestID: "stream-interrupted", ClientKeyID: 1, ModelRouteID: 1, ModelPublicID: "public", StatusCode: 200, Streaming: true, ErrorCode: "upstream_stream_interrupted", DurationMS: 616_731, CreatedAt: now.Add(-time.Hour)},
		{RequestID: "healthy", ClientKeyID: 1, ModelRouteID: 1, ModelPublicID: "public", StatusCode: 200, Streaming: true, InputTokens: 100, OutputTokens: 50, TotalTokens: 150, DurationMS: 100, CreatedAt: now.Add(-2 * time.Hour)},
		{RequestID: "server-error", ClientKeyID: 1, ModelRouteID: 1, ModelPublicID: "public", StatusCode: 500, ErrorCode: "upstream_server_error", DurationMS: 50, CreatedAt: now.Add(-3 * time.Hour)},
	}
	if err := repository.CreateBatch(ctx, values); err != nil {
		t.Fatal(err)
	}
	// 真实 HTTP 200 被保留，但带 error_code 的流式记录计入失败。
	summary, err := repository.Summarize(ctx, repositorypkg.AuditSummaryQuery{Start: now.Add(-24 * time.Hour), End: now})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Requests != 3 || summary.SuccessfulRequests != 1 || summary.FailedRequests != 2 {
		t.Fatalf("summary = %#v", summary)
	}
	// "其它错误"筛选通过 error_code 命中 2xx 响应头之后的流失败。
	items, _, err := repository.ListCursor(ctx, repositorypkg.AuditCursorQuery{Limit: 50, Filter: repositorypkg.AuditListFilter{Status: "other"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].RequestID != "stream-interrupted" {
		t.Fatalf("other filter items = %#v", items)
	}
	if items[0].StatusCode != 200 {
		t.Fatalf("stream failure status = %d, want original HTTP 200", items[0].StatusCode)
	}
	// "成功"筛选排除带错误码的记录。
	items, _, err = repository.ListCursor(ctx, repositorypkg.AuditCursorQuery{Limit: 50, Filter: repositorypkg.AuditListFilter{Status: "success"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].RequestID != "healthy" {
		t.Fatalf("success filter items = %#v", items)
	}
}

// SummarizeDegrade aggregates the classified rows in Go; totals, node order
// (hits, then name), per-account nodes, buckets and the clamped last page must
// match the SQL aggregation they replaced.
func TestSummarizeDegradeAggregatesClassifiedRows(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	enabled := &accountModel{IdentityKey: testIdentityKey("degrade-enabled"), Provider: "grok_build", Name: "enabled", SourceKey: "degrade-enabled", Enabled: true, AuthStatus: "active", MaxConcurrent: 1}
	disabled := &accountModel{IdentityKey: testIdentityKey("degrade-disabled"), Provider: "grok_build", Name: "disabled", SourceKey: "degrade-disabled", Enabled: false, AuthStatus: "active", MaxConcurrent: 1}
	for _, value := range []*accountModel{enabled, disabled} {
		if err := database.db.WithContext(ctx).Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	firstToken := int64(100)
	record := func(accountID *uint64, node string, outputTokens int64, errorCode string, age time.Duration) audit.Record {
		return audit.Record{RequestID: fmt.Sprintf("degrade-%s", age), ClientKeyID: 1, ModelRouteID: 1, Provider: "grok_build", AccountID: accountID, EgressNodeName: node,
			StatusCode: 200, Streaming: true, OutputTokens: outputTokens, FirstTokenMS: &firstToken, DurationMS: 1100, ErrorCode: errorCode, CreatedAt: now.Add(-age)}
	}
	deleted := uint64(999)
	hard, soft := int64(2000), int64(600) // 2000 and 600 tokens/s over a 1s generation window
	records := []audit.Record{
		record(&enabled.ID, "b", hard, "", 10*time.Minute),
		record(&enabled.ID, "", hard, "", 20*time.Minute),
		record(&enabled.ID, "?", soft, "", 70*time.Minute),
		record(&disabled.ID, "a", soft, "", 80*time.Minute),
		record(&disabled.ID, "b", soft, "", 90*time.Minute),
		record(&deleted, "a", 10, audit.ErrorQualityDegraded, 100*time.Minute),
		record(nil, "b", hard, "", 110*time.Minute),
	}
	if err := NewAuditRepository(database).CreateBatch(ctx, records); err != nil {
		t.Fatal(err)
	}
	query := repositorypkg.DegradeSummaryQuery{
		Start: now.Add(-2 * time.Hour), End: now, SoftTPS: 500, HardTPS: 1000, MinGenerationMS: 1000, MinOutputTokens: 32,
		Buckets: []repositorypkg.DegradeBucketRange{{Start: now.Add(-2 * time.Hour), End: now.Add(-time.Hour)}, {Start: now.Add(-time.Hour), End: now}},
	}
	result, err := NewAuditRepository(database).SummarizeDegrade(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if want := (repositorypkg.DegradeTotals{Hits: 7, Accounts: 3, StillEnabled: 1, Disabled: 1, Deleted: 1, Hard: 3, Soft: 3, Thinking: 1, MaxTPS: 2000}); result.Totals != want {
		t.Fatalf("totals = %+v, want %+v", result.Totals, want)
	}
	if want := []repositorypkg.DegradeNode{{Name: "b", Hits: 3, Accounts: 2, MaxTPS: 2000}, {Name: "?", Hits: 2, Accounts: 1, MaxTPS: 2000}, {Name: "a", Hits: 2, Accounts: 2, MaxTPS: 600}}; !reflect.DeepEqual(result.Nodes, want) {
		t.Fatalf("nodes = %+v, want %+v", result.Nodes, want)
	}
	if want := []repositorypkg.DegradeBucket{{Index: 0, Count: 5, Severe: 2}, {Index: 1, Count: 2, Severe: 2}}; !reflect.DeepEqual(result.Buckets, want) {
		t.Fatalf("buckets = %+v, want %+v", result.Buckets, want)
	}
	nodes := make([][]string, 0, len(result.Accounts))
	for _, account := range result.Accounts {
		nodes = append(nodes, account.Nodes)
	}
	if result.AccountTotal != 3 || !reflect.DeepEqual(nodes, [][]string{{"?", "b"}, {"a", "b"}, {"a"}}) {
		t.Fatalf("accounts = %+v (total %d)", result.Accounts, result.AccountTotal)
	}
	query.AccountOffset, query.AccountLimit = 10, 2
	result, err = NewAuditRepository(database).SummarizeDegrade(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if result.AccountOffset != 2 || result.AccountTotal != 3 || len(result.Accounts) != 1 || result.Accounts[0].ID != deleted {
		t.Fatalf("clamped page = offset %d total %d accounts %+v", result.AccountOffset, result.AccountTotal, result.Accounts)
	}
}

func TestDegradeTimestampScansPostgresTimeValue(t *testing.T) {
	expected := time.Date(2026, time.August, 14, 8, 30, 0, 123, time.UTC)
	var value degradeTimestamp
	if err := value.Scan(expected); err != nil {
		t.Fatal(err)
	}
	if actual := time.Time(value); !actual.Equal(expected) {
		t.Fatalf("scanned timestamp = %s, want %s", actual, expected)
	}
}

func TestAuditRepositoryRequestMetadata(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "audit-request-body.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := NewAuditRepository(database)
	now := time.Now().UTC()
	reqHeaders := map[string][]string{
		"User-Agent":   {"curl/8.4.0"},
		"Content-Type": {"application/json"},
	}
	record := audit.Record{
		RequestID:      "req-with-body",
		ClientKeyID:    1,
		ModelRouteID:   1,
		StatusCode:     200,
		RequestMethod:  "POST",
		RequestPath:    "/v1/chat/completions",
		RequestHeaders: reqHeaders,
		CreatedAt:      now,
	}
	if err := repository.Create(ctx, record); err != nil {
		t.Fatal(err)
	}
	items, _, err := repository.List(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items len = %d, want 1", len(items))
	}
	if len(items[0].RequestHeaders) != 0 {
		t.Fatalf("list unexpectedly loaded audit payloads: %#v", items[0])
	}
	detail, err := repository.Get(ctx, items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.RequestMethod != "POST" || detail.RequestPath != "/v1/chat/completions" || detail.RequestHeaders["User-Agent"][0] != "curl/8.4.0" {
		t.Fatalf("Detail HTTP fields mismatch: %#v", detail)
	}
}

func TestAuditRepositoryPurgeOlderThanBatchesAuditsAndAttempts(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "audit-purge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := NewAuditRepository(database)
	cutoff := time.Now().UTC().Add(-24 * time.Hour)
	values := make([]audit.Record, auditPurgeBatchSize+1)
	for index := range values {
		values[index] = audit.Record{
			EventID:      fmt.Sprintf("evt_audit_purge_%04d", index),
			RequestID:    fmt.Sprintf("purge-%04d", index),
			ClientKeyID:  1,
			ModelRouteID: 1,
			StatusCode:   200,
			CreatedAt:    cutoff.Add(-time.Hour),
			Attempts: []audit.Attempt{{
				Number: 1, Source: audit.AttemptSourceCredential, Stage: "response_stream", StartedAt: cutoff.Add(-time.Hour),
			}},
		}
	}
	if err := repository.CreateBatch(ctx, values); err != nil {
		t.Fatal(err)
	}
	if err := repository.Create(ctx, audit.Record{
		EventID: "evt_audit_purge_new", RequestID: "purge-new", ClientKeyID: 1, ModelRouteID: 1,
		StatusCode: 200, CreatedAt: cutoff.Add(time.Hour),
		Attempts: []audit.Attempt{{Number: 1, Source: audit.AttemptSourceCredential, Stage: "response", StartedAt: cutoff.Add(time.Hour)}},
	}); err != nil {
		t.Fatal(err)
	}

	deleted, err := repository.PurgeOlderThan(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != int64(len(values)) {
		t.Fatalf("deleted = %d, want %d", deleted, len(values))
	}
	if count := tableRowCount(t, database, "request_audits"); count != 1 {
		t.Fatalf("remaining audits = %d", count)
	}
	if count := tableRowCount(t, database, "request_audit_attempts"); count != 1 {
		t.Fatalf("remaining attempts = %d", count)
	}
}

func uint64Pointer(value uint64) *uint64 { return &value }

func TestAuditRepositoryFiltersByProviderAndOperation(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "audit-provider-filter.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := NewAuditRepository(database)
	now := time.Now().UTC()
	if err := repository.CreateBatch(ctx, []audit.Record{
		{RequestID: "gpt-chat", ClientKeyID: 1, ModelRouteID: 1, ModelPublicID: "gpt-5", Provider: "chatgpt", Operation: "responses", StatusCode: 200, CreatedAt: now.Add(-time.Hour)},
		{RequestID: "gpt-image", ClientKeyID: 1, ModelRouteID: 1, ModelPublicID: "gpt-image-2", Provider: "chatgpt", Operation: "image", StatusCode: 200, CreatedAt: now.Add(-2 * time.Hour)},
		{RequestID: "grok-chat", ClientKeyID: 1, ModelRouteID: 1, ModelPublicID: "grok", Provider: "grok_build", Operation: "responses", StatusCode: 200, CreatedAt: now.Add(-3 * time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		filter repositorypkg.AuditListFilter
		want   []string
	}{
		{repositorypkg.AuditListFilter{Provider: "chatgpt"}, []string{"gpt-chat", "gpt-image"}},
		{repositorypkg.AuditListFilter{Operation: "responses"}, []string{"gpt-chat", "grok-chat"}},
		{repositorypkg.AuditListFilter{Provider: "chatgpt", Operation: "image"}, []string{"gpt-image"}},
	} {
		items, _, err := repository.ListCursor(ctx, repositorypkg.AuditCursorQuery{Limit: 50, Filter: tc.filter})
		if err != nil {
			t.Fatal(err)
		}
		got := make([]string, 0, len(items))
		for _, item := range items {
			got = append(got, item.RequestID)
		}
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("filter %#v = %v, want %v", tc.filter, got, tc.want)
		}
		summary, err := repository.Summarize(ctx, repositorypkg.AuditSummaryQuery{Start: now.Add(-24 * time.Hour), End: now, Filter: tc.filter})
		if err != nil || summary.Requests != int64(len(tc.want)) {
			t.Fatalf("summary %#v = %#v, err = %v", tc.filter, summary, err)
		}
	}
}
