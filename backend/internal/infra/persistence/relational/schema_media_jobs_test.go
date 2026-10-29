package relational

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestMediaJobLifecycleFencesClaimsAndCancels(t *testing.T) {
	verifyMediaJobLifecycle(t, openTestDatabase(t))
}

func TestMediaJobChecks(t *testing.T) {
	verifyMediaJobChecks(t, openTestDatabase(t))
}

func TestPostgresMediaJobChecksAndLifecycle(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not configured")
	}
	ctx := context.Background()
	database, err := OpenPostgres(ctx, dsn, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	verifyMediaJobChecks(t, database)
	verifyMediaJobLifecycle(t, database)
	verifyMediaJobDeleteSettlement(t, database)
	verifyMediaJobReadSettlement(t, database)
}

func TestMediaJobModelTagsAllowAllVideoProvidersAndPrimaryScopes(t *testing.T) {
	modelType := reflect.TypeOf(mediaJobModel{})
	providerField, ok := modelType.FieldByName("Provider")
	if !ok {
		t.Fatal("Provider field missing")
	}
	providerTag := providerField.Tag.Get("gorm")
	if strings.Contains(providerTag, "check:") {
		t.Fatalf("closed provider tag: %s", providerTag)
	}
	scopeField, ok := modelType.FieldByName("EgressScope")
	if !ok {
		t.Fatal("EgressScope field missing")
	}
	scopeTag := scopeField.Tag.Get("gorm")
	if strings.Contains(scopeTag, "check:") {
		t.Fatalf("closed scope tag: %s", scopeTag)
	}
	inputField, ok := modelType.FieldByName("InputJSON")
	if !ok {
		t.Fatal("InputJSON field missing")
	}
	inputTag := inputField.Tag.Get("gorm")
	limitLiteral := strconv.Itoa(mediadomain.MaxInputJSONBytes)
	if !strings.Contains(inputTag, "chk_media_jobs_input_json") ||
		!strings.Contains(inputTag, limitLiteral) || strings.Contains(inputTag, "1048576") {
		t.Fatalf("input_json tag = %q, want limit %s", inputTag, limitLiteral)
	}
	countField, ok := modelType.FieldByName("InputImageCount")
	if !ok {
		t.Fatal("InputImageCount field missing")
	}
	countTag := countField.Tag.Get("gorm")
	maxImages := strconv.Itoa(mediadomain.MaxVideoInputImages)
	if !strings.Contains(countTag, "chk_media_jobs_input_image_count") ||
		!strings.Contains(countTag, "not null") || !strings.Contains(countTag, "input_image_count BETWEEN 0 AND "+maxImages) {
		t.Fatalf("input_image_count tag = %q, want max %s", countTag, maxImages)
	}
	if !strings.Contains(countTag, "kind <> 'image' OR input_image_count <= "+strconv.Itoa(mediadomain.MaxInputImages)) {
		t.Fatalf("image job input ceiling changed: %s", countTag)
	}
	operationField, ok := modelType.FieldByName("Operation")
	if !ok {
		t.Fatal("Operation field missing")
	}
	operationTag := operationField.Tag.Get("gorm")
	if !strings.Contains(operationTag, "chk_media_jobs_operation") ||
		!strings.Contains(operationTag, "'generate','edit','extend'") {
		t.Fatalf("operation tag = %q", operationTag)
	}
	for fieldName, expected := range map[string]string{
		"Seconds": "seconds BETWEEN 0 AND 60",
		"Size":    "length(trim(size)) BETWEEN 0 AND 32",
		"Quality": "length(trim(quality)) BETWEEN 0 AND 32",
	} {
		field, exists := modelType.FieldByName(fieldName)
		if !exists {
			t.Fatalf("%s field missing", fieldName)
		}
		if tag := field.Tag.Get("gorm"); !strings.Contains(tag, expected) {
			t.Fatalf("%s tag = %q, want %q", fieldName, tag, expected)
		}
	}
}

func mediaJobFixtureOwners(t *testing.T, database *Database, name string) (uint64, uint64, uint64) {
	t.Helper()
	ctx := context.Background()
	accountValue, _, err := NewAccountRepository(database).UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderChatGPT, AuthType: accountdomain.AuthTypeOAuth, Name: name, SourceKey: name,
		EncryptedAccessToken: testEncryptedToken, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	var keys []uint64
	for _, suffix := range []string{"a", "b"} {
		key := clientKeyModel{Name: name + suffix, Prefix: name + suffix, SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 60, MaxConcurrent: 4}
		if err := database.db.WithContext(ctx).Create(&key).Error; err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key.ID)
	}
	return accountValue.ID, keys[0], keys[1]
}

func testImageJob(id string, accountID, keyID uint64, status mediadomain.Status) mediadomain.Job {
	now := time.Now().UTC()
	return mediadomain.Job{
		ID: id, Kind: mediadomain.KindImage, RequestID: "request-" + id, ClientKeyID: keyID, AccountID: accountID,
		Provider: string(accountdomain.ProviderChatGPT), Model: "gpt-image-2", ModelRouteID: 1, UpstreamModel: "chatgpt/gpt-image-2",
		Operation: mediadomain.VideoOperationEdit, Prompt: "edit", Size: "1024x1024", Status: status,
		InputJSON: `{"image_urls":["data:image/png;base64,AAAA"]}`, InputImageCount: 1,
		Params:    mediadomain.JobParams{OutputFormat: "webp", ResponseModel: "gpt-5", ResponseBackground: true},
		CreatedAt: now, UpdatedAt: now,
	}
}

// verifyMediaJobChecks 覆盖 media_jobs 的约束与仓库校验：幂等键按 client key 唯一、图像任务不收
// 视频操作、视频最长 60 秒、Provider 只查写法（是否支持视频由网关按注册表判断）、主出口不能是
// 资源作用域。
func verifyMediaJobChecks(t *testing.T, database *Database) {
	t.Helper()
	ctx := context.Background()
	accountID, keyID, otherKeyID := mediaJobFixtureOwners(t, database, "checks")
	jobs := NewMediaJobRepository(database)
	image := testImageJob("resp_img_checks_ok", accountID, keyID, mediadomain.StatusUncertain)
	image.IdempotencyKey, image.PayloadHash = "client-retry-1", strings.Repeat("a", 64)
	if err := jobs.CreateMediaJob(ctx, image); err != nil {
		t.Fatalf("image job rejected: %v", err)
	}
	duplicate := testImageJob("resp_img_checks_dup", accountID, keyID, mediadomain.StatusQueued)
	duplicate.IdempotencyKey = image.IdempotencyKey
	if err := jobs.CreateMediaJob(ctx, duplicate); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("duplicate idempotency key = %v", err)
	}
	otherKey := duplicate
	otherKey.ClientKeyID = otherKeyID
	if err := jobs.CreateMediaJob(ctx, otherKey); err != nil {
		t.Fatalf("same idempotency key on another client key: %v", err)
	}
	if found, err := jobs.GetMediaJobByIdempotencyKey(ctx, keyID, image.IdempotencyKey); err != nil || found.ID != image.ID || found.PayloadHash != image.PayloadHash || found.Params != image.Params {
		t.Fatalf("idempotent lookup = %#v err = %v", found, err)
	}
	extend := testImageJob("resp_img_checks_extend", accountID, keyID, mediadomain.StatusQueued)
	extend.Operation = mediadomain.VideoOperationExtend
	if err := jobs.CreateMediaJob(ctx, extend); err == nil {
		t.Fatal("image job accepted the video-only extend operation")
	}
	for _, tc := range []struct {
		kind  mediadomain.Kind
		count int
		ok    bool
	}{
		{mediadomain.KindVideo, mediadomain.MaxVideoInputImages, true},
		{mediadomain.KindVideo, mediadomain.MaxVideoInputImages + 1, false},
		{mediadomain.KindImage, mediadomain.MaxInputImages, true},
		{mediadomain.KindImage, mediadomain.MaxInputImages + 1, false},
	} {
		job := testMediaJob("video_count_"+string(tc.kind)+strconv.Itoa(tc.count), accountID, keyID, mediadomain.StatusQueued, time.Now().UTC())
		job.Kind, job.InputImageCount = tc.kind, tc.count
		if err := jobs.CreateMediaJob(ctx, job); (err == nil) != tc.ok {
			t.Errorf("%s with %d images: %v", tc.kind, tc.count, err)
		}
	}
	for _, tc := range []struct {
		id, provider, scope string
		seconds             int
		ok                  bool
	}{
		{"video_checks_adobe", "adobe", "", 30, true},
		{"video_checks_console", "grok_console", "grok_console", 15, true},
		{"video_checks_long", "adobe", "", 61, false},
		{"video_checks_bad_provider", "Invalid-Provider", "", 6, false},
		{"video_checks_asset_scope", "grok_web", "grok_console_asset", 6, false},
	} {
		job := testMediaJob(tc.id, accountID, keyID, mediadomain.StatusQueued, time.Now().UTC())
		job.Provider, job.EgressScope, job.Seconds = tc.provider, tc.scope, tc.seconds
		if err := jobs.CreateMediaJob(ctx, job); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.id, err, tc.ok)
		}
	}
}

func verifyMediaJobLifecycle(t *testing.T, database *Database) {
	t.Helper()
	ctx := context.Background()
	accountID, keyID, _ := mediaJobFixtureOwners(t, database, "lifecycle")
	jobs := NewMediaJobRepository(database)
	assets := NewMediaAssetRepository(database)
	job := testImageJob("resp_img_lifecycle_claim", accountID, keyID, mediadomain.StatusQueued)
	if err := jobs.CreateMediaJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	first, ok, err := jobs.TryClaimMediaJob(ctx, job.ID, now, now.Add(time.Hour), "claim_lifecycle_first")
	if err != nil || !ok || first.Generation != 1 {
		t.Fatalf("first claim = %#v ok=%v err=%v", first, ok, err)
	}
	if _, ok, err := jobs.TryClaimMediaJob(ctx, job.ID, now, now.Add(time.Hour), "claim_lifecycle_racing"); err != nil || ok {
		t.Fatalf("claim during a live lease = %v err = %v", ok, err)
	}
	// The first worker stalls past its lease; recovery claims the next generation.
	expired := now.Add(-time.Second)
	first.LeaseUntil = &expired
	first.ResultAssetID = "img_lifecycle_result_0001"
	if err := jobs.UpdateMediaJob(ctx, first); err != nil {
		t.Fatal(err)
	}
	second, ok, err := jobs.TryClaimMediaJob(ctx, job.ID, now, now.Add(time.Hour), "claim_lifecycle_second")
	if err != nil || !ok || second.Generation != 2 || second.ResultAssetID == "" {
		t.Fatalf("recovery claim = %#v ok=%v err=%v", second, ok, err)
	}
	first.Status = mediadomain.StatusCompleted
	if err := jobs.UpdateMediaJob(ctx, first); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("stale finish = %v", err)
	}
	if err := jobs.RenewMediaJobLease(ctx, first, now.Add(time.Hour)); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("stale renew = %v", err)
	}
	if err := jobs.RenewMediaJobLease(ctx, second, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	protected, err := assets.ListProtectedMediaAssetIDs(ctx)
	if _, ok := protected["img_lifecycle_result_0001"]; err != nil || !ok {
		t.Fatalf("running image result is not protected: %v err=%v", protected, err)
	}

	// Submitted work cannot be canceled; unsubmitted work can, and the claim is fenced out.
	second.Submitted = true
	if err := jobs.UpdateMediaJob(ctx, second); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.CancelMediaJob(ctx, job.ID, keyID, now); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("cancel submitted = %v", err)
	}
	second.Submitted = false
	if err := jobs.UpdateMediaJob(ctx, second); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.CancelMediaJob(ctx, job.ID, keyID+1000, now); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("cancel by another key = %v", err)
	}
	canceled, err := jobs.CancelMediaJob(ctx, job.ID, keyID, now)
	if err != nil || canceled.Status != mediadomain.StatusCanceled || canceled.ClaimToken != "" {
		t.Fatalf("cancel = %#v err = %v", canceled, err)
	}
	if err := jobs.UpdateMediaJob(ctx, second); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("write after cancel = %v", err)
	}
	if again, err := jobs.CancelMediaJob(ctx, job.ID, 0, now); err != nil || again.Status != mediadomain.StatusCanceled {
		t.Fatalf("admin cancel of a canceled job = %#v err = %v", again, err)
	}

	uncertain := testImageJob("resp_img_lifecycle_uncertain", accountID, keyID, mediadomain.StatusUncertain)
	failed := testImageJob("resp_img_lifecycle_failed", accountID, keyID, mediadomain.StatusFailed)
	for _, value := range []mediadomain.Job{uncertain, failed} {
		if err := jobs.CreateMediaJob(ctx, value); err != nil {
			t.Fatal(err)
		}
		if err := jobs.MarkMediaJobUsageRecorded(ctx, value.ID, now); err != nil {
			t.Fatal(err)
		}
	}
	if input, _ := jobs.GetMediaJobInput(ctx, uncertain.ID); input != uncertain.InputJSON {
		t.Fatalf("uncertain input compacted to %q", input)
	}
	if input, _ := jobs.GetMediaJobInput(ctx, failed.ID); input != "{}" {
		t.Fatalf("failed input kept: %q", input)
	}
	listed, total, err := jobs.ListMediaJobs(ctx, repository.MediaJobListQuery{
		Page:   repository.PageQuery{Limit: 10, Search: "resp_img_lifecycle"},
		Filter: repository.MediaJobListFilter{Kind: string(mediadomain.KindImage), Provider: string(accountdomain.ProviderChatGPT), Status: string(mediadomain.StatusUncertain)},
	})
	if err != nil || total != 1 || listed[0].ID != uncertain.ID || listed[0].Kind != mediadomain.KindImage {
		t.Fatalf("filtered list = %#v total=%d err=%v", listed, total, err)
	}
	stats, err := jobs.SummarizeMediaJobs(ctx, string(mediadomain.KindImage))
	if err != nil || stats.Canceled < 1 || stats.Uncertain < 1 || stats.Failed < 1 {
		t.Fatalf("image stats = %#v err = %v", stats, err)
	}
	if err := jobs.DeleteMediaJob(ctx, canceled.ID); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("delete unsettled canceled image job: %v", err)
	}
	if err := jobs.MarkMediaJobUsageRecorded(ctx, canceled.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := jobs.DeleteMediaJob(ctx, canceled.ID); err != nil {
		t.Fatalf("delete canceled job: %v", err)
	}
}
