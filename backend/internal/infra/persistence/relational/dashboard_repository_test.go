package relational

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

func TestDashboardRepositorySnapshot(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	active := &accountModel{IdentityKey: testIdentityKey("active"), Provider: "grok_build", Name: "active", SourceKey: "active", Enabled: true, AuthStatus: "active", MaxConcurrent: 1}
	exhausted := &accountModel{IdentityKey: testIdentityKey("exhausted"), Provider: "grok_build", Name: "exhausted", SourceKey: "exhausted", Enabled: true, AuthStatus: "active", MaxConcurrent: 1}
	enabledRoute := &modelRouteModel{PublicID: "enabled", Provider: "grok_build", UpstreamModel: "enabled", Capability: "responses", Enabled: true}
	internalKind := "quality_guard"
	rows := []any{
		active,
		exhausted,
		&accountModel{IdentityKey: testIdentityKey("disabled"), Provider: "grok_build", Name: "disabled", SourceKey: "disabled", Enabled: false, AuthStatus: "active", MaxConcurrent: 1},
		enabledRoute,
		&modelRouteModel{PublicID: "disabled", Provider: "grok_build", UpstreamModel: "disabled", Capability: "responses", Enabled: false},
		&clientKeyModel{Name: "active", Prefix: "gkp_active", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true},
		&clientKeyModel{Name: "expired", Prefix: "gkp_expired", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, ExpiresAt: timePointer(now.Add(-time.Hour))},
		&clientKeyModel{Name: "internal", Prefix: "quality-guard-internal", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, InternalKind: &internalKind, Enabled: true},
	}
	for _, row := range rows {
		if err := database.db.WithContext(ctx).Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := database.db.WithContext(ctx).Create(&accountModelCapabilityModel{AccountID: active.ID, UpstreamModel: enabledRoute.UpstreamModel}).Error; err != nil {
		t.Fatal(err)
	}
	for _, value := range []accountCredentialModel{
		{AccountID: 1, AuthType: "oauth", EncryptedPrimary: testEncryptedToken, UpdatedAt: now},
		{AccountID: 2, AuthType: "oauth", EncryptedPrimary: testEncryptedToken, UpdatedAt: now},
		{AccountID: 3, AuthType: "oauth", EncryptedPrimary: testEncryptedToken, UpdatedAt: now},
	} {
		if err := database.db.WithContext(ctx).Create(&value).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := database.db.WithContext(ctx).Create(&quotaRecoveryModel{AccountID: exhausted.ID, Kind: "free", Status: "exhausted", NextProbeAt: timePointer(now.Add(24 * time.Hour)), UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	firstTokenOne := int64(100)
	firstTokenTwo := int64(300)
	audits := []requestAuditModel{
		{RequestID: "success-1", ClientKeyID: 1, ModelRouteID: 1, ModelPublicID: "grok-primary", Provider: "grok_build", Operation: "responses", UsageSource: "upstream", StatusCode: 200, Streaming: true, OutputTokens: 90, TotalTokens: 100, FirstTokenMS: &firstTokenOne, DurationMS: 1100, CreatedAt: now.Add(-23 * time.Hour)},
		{RequestID: "success-2", ClientKeyID: 1, ModelRouteID: 1, ModelPublicID: "grok-secondary", Provider: "grok_web", Operation: "responses", UsageSource: "upstream", StatusCode: 201, Streaming: true, OutputTokens: 10, TotalTokens: 50, FirstTokenMS: &firstTokenTwo, DurationMS: 1300, CreatedAt: now.Add(-time.Hour)},
		{RequestID: "failed", ClientKeyID: 1, ModelRouteID: 1, ModelPublicID: "grok-primary", Provider: "grok_build", Operation: "responses", UsageSource: "upstream", StatusCode: 500, Streaming: true, OutputTokens: 100, TotalTokens: 10, DurationMS: 550, CreatedAt: now.Add(-2 * time.Hour)},
		{RequestID: "outside", ClientKeyID: 1, ModelRouteID: 1, Provider: "grok_build", Operation: "responses", UsageSource: "upstream", StatusCode: 200, TotalTokens: 999, CreatedAt: now.Add(-25 * time.Hour)},
	}
	for index := range audits {
		if err := database.db.WithContext(ctx).Create(&audits[index]).Error; err != nil {
			t.Fatal(err)
		}
	}

	boundaries := testDashboardBoundaries(now.Add(-24*time.Hour), 2*time.Hour, 12)
	snapshot, err := NewDashboardRepository(database).Snapshot(ctx, testDashboardWindow(boundaries), now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Resources.ActiveAccounts != 1 || snapshot.Resources.TotalAccounts != 3 || len(snapshot.Resources.AccountsByProvider) != 1 || snapshot.Resources.AccountsByProvider["grok_build"] != 3 || snapshot.Resources.EnabledModels != 1 || snapshot.Resources.TotalModels != 2 || snapshot.Resources.ActiveClientKeys != 1 || snapshot.Resources.TotalClientKeys != 2 {
		t.Fatalf("resources = %#v", snapshot.Resources)
	}
	if snapshot.Usage.Requests != 3 || snapshot.Usage.SuccessfulRequests != 2 || snapshot.Usage.FailedRequests != 1 || snapshot.Usage.Tokens != 160 {
		t.Fatalf("usage = %#v", snapshot.Usage)
	}
	if snapshot.Usage.FirstTokenSamples != 2 || snapshot.Usage.FirstTokenTotalMS != 400 || snapshot.Usage.ThroughputSamples != 2 || snapshot.Usage.ThroughputTokens != 100 || snapshot.Usage.GenerationTotalMS != 2000 {
		t.Fatalf("performance usage = %#v", snapshot.Usage)
	}
	var bucketRequests int64
	var bucketTokens int64
	bucketsByIndex := make(map[int]dashboardBucketSummary)
	for _, bucket := range snapshot.Buckets {
		bucketRequests += bucket.Requests
		bucketTokens += bucket.Tokens
		bucketsByIndex[bucket.Index] = dashboardBucketSummary{Requests: bucket.Requests, Tokens: bucket.Tokens}
	}
	if bucketRequests != 3 || bucketTokens != 160 {
		t.Fatalf("buckets = %#v", snapshot.Buckets)
	}
	if bucketsByIndex[0] != (dashboardBucketSummary{Requests: 1, Tokens: 100}) || bucketsByIndex[11] != (dashboardBucketSummary{Requests: 2, Tokens: 60}) {
		t.Fatalf("bucket distribution = %#v", bucketsByIndex)
	}
	if len(snapshot.TopModels) != 3 || snapshot.TopModels[0].Model != "grok-primary" || snapshot.TopModels[0].Requests != 2 || snapshot.TopModels[0].Tokens != 110 || snapshot.TopModels[2].Model != "enabled" || snapshot.TopModels[2].Requests != 0 {
		t.Fatalf("top models = %#v", snapshot.TopModels)
	}
	if len(snapshot.Providers) != 2 || snapshot.Providers[0].Provider != "grok_build" || snapshot.Providers[0].Requests != 2 || snapshot.Providers[1].Provider != "grok_web" || snapshot.Providers[1].Requests != 1 {
		t.Fatalf("providers = %#v", snapshot.Providers)
	}
	var activityRequests int64
	for _, bucket := range snapshot.ActivityBuckets {
		activityRequests += bucket.Requests
	}
	if activityRequests != 3 {
		t.Fatalf("activity buckets = %#v", snapshot.ActivityBuckets)
	}
}

func TestDashboardRepositoryCounts2xxWithErrorAsFailure(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "dashboard-stream-failure.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	audits := []requestAuditModel{
		{RequestID: "healthy", ClientKeyID: 1, ModelRouteID: 1, Provider: "grok_build", Operation: "responses", UsageSource: "upstream", StatusCode: 200, CreatedAt: now.Add(-2 * time.Minute)},
		{RequestID: "stream-interrupted", ClientKeyID: 1, ModelRouteID: 1, Provider: "grok_build", Operation: "responses", UsageSource: "upstream", StatusCode: 200, Streaming: true, ErrorCode: "upstream_stream_interrupted", CreatedAt: now.Add(-time.Minute)},
	}
	if err := database.db.WithContext(ctx).Create(&audits).Error; err != nil {
		t.Fatal(err)
	}
	boundaries := testDashboardBoundaries(now.Add(-time.Hour), time.Hour, 2)
	snapshot, err := NewDashboardRepository(database).Snapshot(ctx, testDashboardWindow(boundaries), now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Usage.Requests != 2 || snapshot.Usage.SuccessfulRequests != 1 || snapshot.Usage.FailedRequests != 1 {
		t.Fatalf("usage = %#v", snapshot.Usage)
	}
}

func BenchmarkDashboardUsageAggregate(b *testing.B) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(b.TempDir(), "dashboard-benchmark.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		b.Fatal(err)
	}
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	audits := make([]requestAuditModel, 50_000)
	for index := range audits {
		firstTokenMS := int64(100 + index%900)
		audits[index] = requestAuditModel{
			RequestID: fmt.Sprintf("benchmark-%d", index), ClientKeyID: 1, ModelRouteID: 1,
			Provider: "grok_build", Operation: "responses", UsageSource: "upstream", StatusCode: 200,
			Streaming: true, InputTokens: 1000, OutputTokens: 100, TotalTokens: 1100,
			FirstTokenMS: &firstTokenMS, DurationMS: firstTokenMS + 2000, CreatedAt: now.Add(-time.Duration(index%3600) * time.Second),
		}
	}
	if err := database.db.WithContext(ctx).CreateInBatches(audits, 100).Error; err != nil {
		b.Fatal(err)
	}
	start := now.Add(-24 * time.Hour)
	legacySelect := "COUNT(*) AS requests, COALESCE(SUM(CASE WHEN status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END), 0) AS successful_requests, COALESCE(SUM(CASE WHEN status_code < 200 OR status_code >= 300 THEN 1 ELSE 0 END), 0) AS failed_requests, COALESCE(SUM(input_tokens), 0) AS input_tokens, COALESCE(SUM(cached_input_tokens), 0) AS cached_input_tokens, COALESCE(SUM(output_tokens), 0) AS output_tokens, COALESCE(SUM(reasoning_tokens), 0) AS reasoning_tokens, COALESCE(SUM(total_tokens), 0) AS tokens, COALESCE(SUM(CASE WHEN cost_in_usd_ticks > 0 THEN cost_in_usd_ticks ELSE estimated_cost_in_usd_ticks END), 0) AS billed_cost_usd_ticks"
	b.Run("legacy", func(b *testing.B) {
		for b.Loop() {
			var usage dashboardBenchmarkUsage
			if err := database.db.WithContext(ctx).Model(&requestAuditModel{}).Select(legacySelect).Where("created_at >= ? AND created_at < ?", start, now.Add(time.Second)).Scan(&usage).Error; err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("performance", func(b *testing.B) {
		for b.Loop() {
			var usage dashboardBenchmarkUsage
			if err := database.db.WithContext(ctx).Model(&requestAuditModel{}).Select(dashboardUsageAggregateSelect).Where("created_at >= ? AND created_at < ?", start, now.Add(time.Second)).Scan(&usage).Error; err != nil {
				b.Fatal(err)
			}
		}
	})
}

type dashboardBenchmarkUsage struct {
	Requests           int64
	SuccessfulRequests int64
	FailedRequests     int64
	InputTokens        int64
	CachedInputTokens  int64
	OutputTokens       int64
	ReasoningTokens    int64
	Tokens             int64
	BilledCostUSDTicks int64
	FirstTokenSamples  int64
	FirstTokenTotalMS  int64
	ThroughputSamples  int64
	ThroughputTokens   int64
	GenerationTotalMS  int64
}

func TestDashboardRepositoryUsesFullDurationOnlyWithReasoningEvidence(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "dashboard-short-tail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	lateFirst := int64(19763)
	normalFirst := int64(200)
	burstFirst := int64(10000)
	rows := []requestAuditModel{
		{RequestID: "late-reasoning-flush", ClientKeyID: 1, ModelRouteID: 1, ModelPublicID: "grok-4.6", Provider: "grok_build", Operation: "chat", UsageSource: "upstream", StatusCode: 200, Streaming: true, OutputTokens: 1511, ReasoningTokens: 1400, FirstTokenMS: &lateFirst, DurationMS: 19827, CreatedAt: now.Add(-time.Hour)},
		{RequestID: "normal-tail", ClientKeyID: 1, ModelRouteID: 1, ModelPublicID: "grok-4.6", Provider: "grok_build", Operation: "chat", UsageSource: "upstream", StatusCode: 200, Streaming: true, OutputTokens: 80, FirstTokenMS: &normalFirst, DurationMS: 2200, CreatedAt: now.Add(-30 * time.Minute)},
		{RequestID: "no-reasoning-burst", ClientKeyID: 1, ModelRouteID: 1, ModelPublicID: "grok-4.6", Provider: "grok_build", Operation: "chat", UsageSource: "upstream", StatusCode: 200, Streaming: true, OutputTokens: 2000, FirstTokenMS: &burstFirst, DurationMS: 10100, CreatedAt: now.Add(-15 * time.Minute)},
	}
	if err := database.db.WithContext(ctx).Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	boundaries := testDashboardBoundaries(now.Add(-2*time.Hour), time.Hour, 2)
	snapshot, err := NewDashboardRepository(database).Snapshot(ctx, testDashboardWindow(boundaries), now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Usage.ThroughputTokens != 3591 || snapshot.Usage.GenerationTotalMS != 19827+2000+100 {
		t.Fatalf("short-tail generation window = %#v", snapshot.Usage)
	}
}

func TestDashboardRepositoryRanksTopModels(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "dashboard-top-models.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	rows := []requestAuditModel{
		{RequestID: "primary-1", ClientKeyID: 1, ModelRouteID: 1, ModelPublicID: "grok-primary", Provider: "grok_build", Operation: "responses", UsageSource: "upstream", StatusCode: 200, InputTokens: 80, CachedInputTokens: 20, OutputTokens: 20, ReasoningTokens: 5, TotalTokens: 100, CostInUSDTicks: 1_000_000_000, EstimatedCostInUSDTicks: 9_000_000_000, CreatedAt: now.Add(-3 * time.Hour)},
		{RequestID: "primary-2", ClientKeyID: 1, ModelRouteID: 1, ModelPublicID: "grok-primary", Provider: "grok_build", Operation: "responses", UsageSource: "upstream", StatusCode: 200, InputTokens: 30, CachedInputTokens: 5, OutputTokens: 20, ReasoningTokens: 10, TotalTokens: 50, EstimatedCostInUSDTicks: 2_000_000_000, CreatedAt: now.Add(-2 * time.Hour)},
		{RequestID: "fallback", ClientKeyID: 1, ModelRouteID: 1, ModelUpstreamModel: "grok-fallback", Provider: "grok_build", Operation: "responses", UsageSource: "upstream", StatusCode: 200, TotalTokens: 200, CostInUSDTicks: 4_000_000_000, CreatedAt: now.Add(-time.Hour)},
	}
	if err := database.db.WithContext(ctx).Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	boundaries := testDashboardBoundaries(now.Add(-24*time.Hour), time.Hour, 24)
	snapshot, err := NewDashboardRepository(database).Snapshot(ctx, testDashboardWindow(boundaries), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.TopModels) != 2 || snapshot.TopModels[0].Model != "grok-fallback" || snapshot.TopModels[0].BilledCostUSDTicks != 4_000_000_000 || snapshot.TopModels[1].Model != "grok-primary" || snapshot.TopModels[1].Requests != 2 || snapshot.TopModels[1].InputTokens != 110 || snapshot.TopModels[1].CachedInputTokens != 25 || snapshot.TopModels[1].OutputTokens != 40 || snapshot.TopModels[1].ReasoningTokens != 15 || snapshot.TopModels[1].Tokens != 150 || snapshot.TopModels[1].BilledCostUSDTicks != 3_000_000_000 {
		t.Fatalf("top models = %#v", snapshot.TopModels)
	}
}

func TestDashboardRepositoryFillsTopModelsFromEnabledRoutes(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "dashboard-enabled-models.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	for index := 0; index < 12; index++ {
		name := fmt.Sprintf("grok-%02d", index)
		route := modelRouteModel{PublicID: "Build/" + name, Provider: "grok_build", UpstreamModel: name, Capability: "responses", Enabled: true}
		if err := database.db.WithContext(ctx).Create(&route).Error; err != nil {
			t.Fatal(err)
		}
	}
	duplicate := modelRouteModel{PublicID: "Web/grok-00", Provider: "grok_web", UpstreamModel: "grok-00", Capability: "responses", Enabled: true}
	if err := database.db.WithContext(ctx).Create(&duplicate).Error; err != nil {
		t.Fatal(err)
	}
	disabled := modelRouteModel{PublicID: "Build/disabled", Provider: "grok_build", UpstreamModel: "disabled", Capability: "responses", Enabled: false}
	if err := database.db.WithContext(ctx).Create(&disabled).Error; err != nil {
		t.Fatal(err)
	}

	boundaries := testDashboardBoundaries(now.Add(-24*time.Hour), time.Hour, 24)
	snapshot, err := NewDashboardRepository(database).Snapshot(ctx, testDashboardWindow(boundaries), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.TopModels) != 10 {
		t.Fatalf("top models count = %d, want 10: %#v", len(snapshot.TopModels), snapshot.TopModels)
	}
	for index, item := range snapshot.TopModels {
		want := fmt.Sprintf("grok-%02d", index)
		if item.Model != want || item.Requests != 0 || item.Tokens != 0 || item.BilledCostUSDTicks != 0 {
			t.Fatalf("top model %d = %#v, want zero-usage %q", index, item, want)
		}
	}
}

type dashboardBucketSummary struct {
	Requests int64
	Tokens   int64
}

func timePointer(value time.Time) *time.Time { return &value }

func testDashboardBoundaries(start time.Time, step time.Duration, count int) []time.Time {
	values := make([]time.Time, count+1)
	for index := range values {
		values[index] = start.Add(time.Duration(index) * step)
	}
	return values
}

func testDashboardWindow(boundaries []time.Time) repository.DashboardSnapshotWindow {
	return repository.DashboardSnapshotWindow{
		BucketBoundaries:   boundaries,
		ActivityBoundaries: boundaries,
	}
}

// Snapshot groups audits by UTC hour or quarter hour and folds the slots into
// local buckets; each bucket must still match a direct comparison against its
// boundaries, including +05:30/+05:45 zones, a DST change and boundaries off
// the quarter hours.
func TestDashboardRepositoryFoldsUTCSlotsIntoLocalBuckets(t *testing.T) {
	assertDashboardLocalBuckets(t, openTestDatabase(t))
}

func TestPostgresDashboardFoldsUTCSlotsIntoLocalBuckets(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not configured")
	}
	database, err := OpenPostgres(context.Background(), dsn, 4, 2)
	if err != nil {
		t.Fatal(err)
	}
	// Close after the helper's cleanup has removed its rows from the shared database.
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertDashboardLocalBuckets(t, database)
}

func assertDashboardLocalBuckets(t *testing.T, database *Database) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2031, 11, 3, 10, 7, 0, 0, time.UTC)
	var audits []requestAuditModel
	add := func(createdAt time.Time) {
		audits = append(audits, requestAuditModel{RequestID: fmt.Sprintf("fold-%d", len(audits)), ClientKeyID: 1, ModelRouteID: 1, Provider: "grok_build", Operation: "responses", UsageSource: "upstream", StatusCode: 200, TotalTokens: int64(len(audits) + 1), CreatedAt: createdAt.UTC()})
	}
	for createdAt := now.Add(-9 * 24 * time.Hour); createdAt.Before(now); createdAt = createdAt.Add(7*time.Minute + 13*time.Second) {
		add(createdAt)
	}
	var windows [][2][]time.Time
	for _, zone := range []string{"UTC", "Asia/Kolkata", "Asia/Kathmandu", "America/New_York"} {
		location, err := time.LoadLocation(zone)
		if err != nil {
			t.Fatal(err)
		}
		local := now.In(location)
		hour := time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), 0, 0, 0, location)
		tomorrow := time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, location)
		var hours, days []time.Time
		for index := -23; index <= 1; index++ {
			hours = append(hours, hour.Add(time.Duration(index)*time.Hour).UTC())
		}
		for index := -7; index <= 0; index++ {
			days = append(days, tomorrow.AddDate(0, 0, index).UTC())
		}
		var shifted []time.Time
		for _, boundary := range hours {
			shifted = append(shifted, boundary.Add(7*time.Minute+13500*time.Millisecond))
		}
		windows = append(windows, [2][]time.Time{hours, days}, [2][]time.Time{days, days}, [2][]time.Time{shifted, days})
		for _, boundary := range append(append(hours, days...), shifted...) {
			add(boundary)
			add(boundary.Add(-time.Microsecond))
		}
	}
	if err := database.db.WithContext(ctx).CreateInBatches(audits, 200).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.db.Where("request_id LIKE ?", "fold-%").Delete(&requestAuditModel{}).Error; err != nil {
			t.Error(err)
		}
	})
	expected := func(boundaries []time.Time) map[int][2]int64 {
		buckets := make(map[int][2]int64)
		for _, audit := range audits {
			for index := 0; index+1 < len(boundaries); index++ {
				if !audit.CreatedAt.Before(boundaries[index]) && audit.CreatedAt.Before(boundaries[index+1]) {
					bucket := buckets[index]
					buckets[index] = [2]int64{bucket[0] + 1, bucket[1] + audit.TotalTokens}
				}
			}
		}
		return buckets
	}
	for _, window := range windows {
		snapshot, err := NewDashboardRepository(database).Snapshot(ctx, repository.DashboardSnapshotWindow{BucketBoundaries: window[0], ActivityBoundaries: window[1]}, now)
		if err != nil {
			t.Fatal(err)
		}
		buckets := make(map[int][2]int64)
		var total [2]int64
		for _, bucket := range snapshot.Buckets {
			buckets[bucket.Index] = [2]int64{bucket.Requests, bucket.Tokens}
			total = [2]int64{total[0] + bucket.Requests, total[1] + bucket.Tokens}
		}
		activity := make(map[int][2]int64)
		for _, bucket := range snapshot.ActivityBuckets {
			activity[bucket.Index] = [2]int64{bucket.Requests}
		}
		wantActivity := expected(window[1])
		for index, bucket := range wantActivity {
			wantActivity[index] = [2]int64{bucket[0]}
		}
		if want := expected(window[0]); !reflect.DeepEqual(buckets, want) || total != [2]int64{snapshot.Usage.Requests, snapshot.Usage.Tokens} || !reflect.DeepEqual(activity, wantActivity) {
			t.Fatalf("window %s..%s: buckets %v want %v, usage %+v, activity %v want %v", window[0][0], window[0][len(window[0])-1], buckets, want, snapshot.Usage, activity, wantActivity)
		}
	}
}

// Reports read inside a read-only transaction: with _txlock=immediate a plain
// transaction would hold SQLite's write lock and stall audit writes.
func TestReportsDoNotBlockSQLiteWriters(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	var hold atomic.Value
	hold.Store(func() {})
	// Scan runs through the row callbacks; hold the report after its first read.
	if err := database.db.Callback().Row().After("gorm:row").Register("test:hold_report", func(*gorm.DB) { hold.Load().(func())() }); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	reports := []struct {
		name string
		run  func() error
	}{
		{"dashboard", func() error {
			boundaries := testDashboardBoundaries(now.Truncate(time.Hour).Add(-time.Hour), time.Hour, 2)
			_, err := NewDashboardRepository(database).Snapshot(ctx, testDashboardWindow(boundaries), now)
			return err
		}},
		{"degrade", func() error {
			_, err := NewAuditRepository(database).SummarizeDegrade(ctx, repository.DegradeSummaryQuery{Start: now.Add(-time.Hour), End: now, SoftTPS: 500, HardTPS: 1000})
			return err
		}},
	}
	for _, report := range reports {
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		hold.Store(func() { once.Do(func() { close(entered); <-release }) })
		done := make(chan error, 1)
		go func() { done <- report.run() }()
		select {
		case <-entered:
		case err := <-done:
			t.Fatalf("%s report finished before it was held open: %v", report.name, err)
		}
		writeErr := database.db.WithContext(ctx).Create(&requestAuditModel{RequestID: "write-during-" + report.name, ClientKeyID: 1, ModelRouteID: 1, Provider: "grok_build", Operation: "responses", UsageSource: "upstream", StatusCode: 200, CreatedAt: now}).Error
		hold.Store(func() {})
		close(release)
		if err := <-done; err != nil {
			t.Fatalf("%s report: %v", report.name, err)
		}
		if writeErr != nil {
			t.Errorf("audit write while the %s report was open: %v", report.name, writeErr)
		}
	}
}
