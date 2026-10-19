package relational

import (
	"context"
	"strings"
	"testing"
	"time"

	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/google/uuid"
)

func TestMediaAssetOriginSchema(t *testing.T) {
	priceDatabases(t, func(t *testing.T, database *Database) {
		ctx := context.Background()
		db := database.db.WithContext(ctx)
		repo := NewMediaAssetRepository(database)
		suffix := uuid.NewString()[:8]
		originIndexes := []string{"idx_media_assets_provider_created", "idx_media_assets_client_key_created", "idx_media_assets_account_created"}
		assertOriginIndexes := func() {
			t.Helper()
			for _, name := range originIndexes {
				if !db.Migrator().HasIndex("media_assets", name) {
					t.Fatalf("missing index %s", name)
				}
			}
		}

		// Origin round-trips and is clipped to the column sizes.
		assertOriginIndexes()
		fresh := testMediaAsset("media_origin_fresh_"+suffix, "media/origin-fresh-"+suffix+".png", time.Now().UTC())
		fresh.Origin = mediadomain.Origin{
			Provider: "grok_web", Model: strings.Repeat("m", 300), UpstreamModel: "grok/imagine", ClientKeyID: 7, ClientKeyName: "key",
			AccountID: 9, AccountName: "account", Operation: "image", JobID: "job-1", RequestID: "req-1", Prompt: strings.Repeat("猫", 1200),
		}
		if err := repo.CreateMediaAsset(ctx, fresh); err != nil {
			t.Fatal(err)
		}
		stored, err := repo.GetMediaAsset(ctx, fresh.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := fresh.Origin
		want.Model, want.Prompt = strings.Repeat("m", 255), strings.Repeat("猫", 1000)
		if stored.Origin != want {
			t.Fatalf("stored origin = %#v", stored.Origin)
		}
	})
}

func TestMediaAssetListFiltersByOrigin(t *testing.T) {
	priceDatabases(t, func(t *testing.T, database *Database) {
		ctx := context.Background()
		repo := NewMediaAssetRepository(database)
		suffix := uuid.NewString()[:8]
		provider, model := "p"+suffix, "model-"+suffix
		base := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
		asset := func(name, kind string, offset time.Duration, origin mediadomain.Origin) string {
			value := testMediaAsset("media_filter_"+name+"_"+suffix, "media/filter-"+name+"-"+suffix, base.Add(offset))
			if kind == "video" {
				value.Kind, value.MIMEType = "video", "video/mp4"
			}
			value.Origin = origin
			if err := repo.CreateMediaAsset(ctx, value); err != nil {
				t.Fatal(err)
			}
			return value.ID
		}
		first := asset("first", "image", 0, mediadomain.Origin{Provider: provider, Model: model, Operation: "image", ClientKeyID: 11, AccountID: 21, Prompt: "a red fox", JobID: "resp_img_" + suffix})
		second := asset("second", "image", time.Hour, mediadomain.Origin{Provider: provider, Model: "other", Operation: "image_edit", ClientKeyID: 12, AccountID: 22, Prompt: "a blue whale"})
		video := asset("video", "video", 2*time.Hour, mediadomain.Origin{Provider: provider, Model: model, Operation: "video", ClientKeyID: 11, AccountID: 21})
		asset("other", "image", 3*time.Hour, mediadomain.Origin{Provider: "q" + suffix, Model: model})
		input := testMediaAsset("media_filter_input_"+suffix, "media/filter-input-"+suffix, base)
		expires := base.Add(time.Hour)
		input.ExpiresAt, input.Origin = &expires, mediadomain.Origin{Provider: provider}
		if err := repo.CreateMediaAsset(ctx, input); err != nil {
			t.Fatal(err)
		}

		// A non-UTC bound must compare as the same instant on SQLite text timestamps.
		halfPast := base.Add(30 * time.Minute).In(time.FixedZone("UTC+8", 8*3600))
		for _, tc := range []struct {
			name   string
			filter repository.MediaAssetListFilter
			search string
			want   []string
		}{
			{"provider", repository.MediaAssetListFilter{Provider: provider}, "", []string{second, first}},
			{"video", repository.MediaAssetListFilter{Provider: provider, Kind: "video"}, "", []string{video}},
			{"model", repository.MediaAssetListFilter{Provider: provider, Model: model}, "", []string{first}},
			{"operation", repository.MediaAssetListFilter{Provider: provider, Operation: "image_edit"}, "", []string{second}},
			{"key", repository.MediaAssetListFilter{Provider: provider, ClientKeyID: 11}, "", []string{first}},
			{"account", repository.MediaAssetListFilter{Provider: provider, AccountID: 22}, "", []string{second}},
			{"from", repository.MediaAssetListFilter{Provider: provider, CreatedFrom: halfPast}, "", []string{second}},
			{"to", repository.MediaAssetListFilter{Provider: provider, CreatedTo: halfPast}, "", []string{first}},
			{"prompt search", repository.MediaAssetListFilter{Provider: provider}, "WHALE", []string{second}},
			{"job", repository.MediaAssetListFilter{JobID: "resp_img_" + suffix}, "", []string{first}},
		} {
			values, total, err := repo.ListMediaAssets(ctx, repository.MediaAssetListQuery{
				Page: repository.PageQuery{Limit: 10, Search: tc.search}, Filter: tc.filter,
			})
			if err != nil || total != int64(len(tc.want)) {
				t.Fatalf("%s: total = %d, err = %v", tc.name, total, err)
			}
			assertMediaAssetIDs(t, values, tc.want...)
		}
		values, _, err := repo.ListMediaAssets(ctx, repository.MediaAssetListQuery{Page: repository.PageQuery{Limit: 1}, Filter: repository.MediaAssetListFilter{Provider: provider, Operation: "image"}})
		if err != nil || len(values) != 1 || values[0].Origin.Prompt != "a red fox" || values[0].Origin.ClientKeyID != 11 || values[0].StorageKey != "" {
			t.Fatalf("listed origin projection = %#v, err = %v", values, err)
		}
	})
}
