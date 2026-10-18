package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
)

func testVideoBytes(fill byte) []byte {
	return append([]byte{0, 0, 0, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, bytes.Repeat([]byte{fill}, 64)...)
}

// createOriginTestJob stores a media job with its own key and account.
func createOriginTestJob(t *testing.T, database *relational.Database, id string, status mediadomain.Status, resultAssetID, inputJSON string) mediadomain.Job {
	t.Helper()
	ctx := context.Background()
	accountValue, _, err := relational.NewAccountRepository(database).UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, WebTier: accountdomain.WebTierBasic,
		Name: id + "-account", SourceKey: id + "-account", EncryptedAccessToken: "encrypted-access-token", AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := relational.NewClientKeyRepository(database).Create(ctx, clientkeydomain.Key{
		Name: id + "-key", Prefix: id, SecretHash: strings.Repeat("a", 64), EncryptedSecret: "encrypted-secret",
		Enabled: true, RPMLimit: 60, MaxConcurrent: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	job := mediadomain.Job{
		ID: id, RequestID: "req-" + id, ClientKeyID: key.ID, ClientKeyName: key.Name,
		AccountID: accountValue.ID, AccountName: accountValue.Name, Provider: string(accountdomain.ProviderWeb),
		Model: "grok-imagine-video", ModelRouteID: 1, UpstreamModel: "grok-imagine-video", Prompt: "prompt of " + id,
		Seconds: 6, Size: "16:9", Quality: "720p", Status: status, InputJSON: inputJSON, ResultAssetID: resultAssetID,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := relational.NewMediaJobRepository(database).CreateMediaJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	return job
}

func TestSavedAssetsPersistContextOrigin(t *testing.T) {
	database, objects, tickets, cleanup := openUploadTestDeps(t)
	defer cleanup()
	ctx := context.Background()
	assets := relational.NewMediaAssetRepository(database)
	service := NewServiceWithTickets(assets, relational.NewMediaJobRepository(database), tickets, objects, nil,
		Config{PublicBaseURL: "https://api.example", MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30, CleanupThresholdPercent: 80, CleanupInterval: time.Minute})
	raw, _ := base64.StdEncoding.DecodeString(onePixelPNG)
	origin := mediadomain.Origin{
		Provider: "grok_console", Model: "grok-imagine-image", UpstreamModel: "console/grok-imagine-image",
		ClientKeyID: 3, ClientKeyName: "key", AccountID: 5, AccountName: "account",
		Operation: "image", RequestID: "req-origin", Prompt: strings.Repeat("p", 1200),
	}
	stored := origin
	stored.Prompt = strings.Repeat("p", 1000)
	attributed := mediadomain.WithOrigin(ctx, origin)

	want := map[string]mediadomain.Origin{}
	save := func(asset mediadomain.Asset, err error, expected mediadomain.Origin) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		want[asset.ID] = expected
	}
	asset, err := service.SaveImage(ctx, raw)
	save(asset, err, mediadomain.Origin{})
	asset, err = service.SaveImage(attributed, raw)
	save(asset, err, stored)
	asset, err = service.SaveVideo(ctx, "", "video/mp4", bytes.NewReader(testVideoBytes(5)))
	save(asset, err, mediadomain.Origin{})
	asset, err = service.SaveVideo(attributed, "", "video/mp4", bytes.NewReader(testVideoBytes(6)))
	save(asset, err, stored)

	// The upstream PUT has no gateway context: the ticket's job supplies it.
	job := createOriginTestJob(t, database, "video_origin_ticket", mediadomain.StatusInProgress, "", "{}")
	uploadURL, assetID, err := service.IssueVideoUpload(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	asset, err = service.ReceiveVideoUpload(ctx, strings.TrimPrefix(uploadURL, "https://api.example/v1/media/uploads/"), "video/mp4", bytes.NewReader(testVideoBytes(7)))
	save(asset, err, mediadomain.JobOrigin(job, "video"))
	if asset.ID != assetID {
		t.Fatalf("uploaded asset = %s, ticket asset = %s", asset.ID, assetID)
	}

	for id, expected := range want {
		value, err := assets.GetMediaAsset(ctx, id)
		if err != nil || value.Origin != expected {
			t.Fatalf("asset %s origin = %#v, want %#v, err = %v", id, value.Origin, expected, err)
		}
	}
}

// Gallery delete used to ignore job references: it removed an active job's
// result and any temporary input it was handed.
func TestAdminDeleteImagesRespectsJobReferences(t *testing.T) {
	database, objects, tickets, cleanup := openUploadTestDeps(t)
	defer cleanup()
	ctx := context.Background()
	assets := relational.NewMediaAssetRepository(database)
	jobs := relational.NewMediaJobRepository(database)
	service := NewServiceWithTickets(assets, jobs, tickets, objects, nil,
		Config{PublicBaseURL: "https://api.example", MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30, CleanupThresholdPercent: 80, CleanupInterval: time.Minute})
	raw, _ := base64.StdEncoding.DecodeString(onePixelPNG)
	result, err := service.SaveImage(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	other, err := service.SaveImage(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	input, err := service.SaveInputImage(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	video, err := service.SaveVideo(ctx, "", "video/mp4", bytes.NewReader(testVideoBytes(8)))
	if err != nil {
		t.Fatal(err)
	}
	active := createOriginTestJob(t, database, "image_ref_active", mediadomain.StatusInProgress, result.ID, `{"image_url":"`+mediadomain.InputReference(input.ID)+`"}`)
	createOriginTestJob(t, database, "video_ref_done", mediadomain.StatusCompleted, video.ID, "{}")

	for _, ids := range [][]string{{result.ID}, {other.ID, result.ID}} {
		if deleted, err := service.AdminDeleteImages(ctx, ids); !errors.Is(err, ErrAssetInUse) || deleted != 0 {
			t.Fatalf("delete %v = %d, %v; want ErrAssetInUse", ids, deleted, err)
		}
	}
	if deleted, err := service.AdminDeleteImages(ctx, []string{input.ID}); err != nil || deleted != 0 {
		t.Fatalf("delete input = %d, %v", deleted, err)
	}
	for _, id := range []string{result.ID, other.ID, input.ID} {
		if _, err := assets.GetMediaAsset(ctx, id); err != nil {
			t.Fatalf("asset %s removed: %v", id, err)
		}
	}

	// Terminal jobs tolerate a missing result, as after capacity cleanup.
	active.Status = mediadomain.StatusCompleted
	if err := jobs.UpdateMediaJob(ctx, active); err != nil {
		t.Fatal(err)
	}
	if deleted, err := service.AdminDeleteImages(ctx, []string{result.ID, other.ID, video.ID}); err != nil || deleted != 3 {
		t.Fatalf("delete terminal references = %d, %v", deleted, err)
	}
}
