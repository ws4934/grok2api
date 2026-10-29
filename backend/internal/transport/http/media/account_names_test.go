package media

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/gin-gonic/gin"
)

func TestAdminMediaViewsUseCurrentEmailAndKeepStoredSnapshots(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "media-labels.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(db)
	value, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{Provider: accountdomain.ProviderWeb, Name: "Grok Web 219ff735", Email: "media@example.test", SourceKey: "media-labels", EncryptedAccessToken: "encrypted", AuthStatus: accountdomain.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	key, err := relational.NewClientKeyRepository(db).Create(ctx, clientkeydomain.Key{Name: "labels", Prefix: "labels", SecretHash: strings.Repeat("a", 64), EncryptedSecret: "encrypted", Enabled: true, RPMLimit: 60, MaxConcurrent: 4})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	jobs := relational.NewMediaJobRepository(db)
	assets := relational.NewMediaAssetRepository(db)
	job := mediadomain.Job{ID: "video_labels", Kind: mediadomain.KindVideo, RequestID: "labels-request", ClientKeyID: key.ID, ClientKeyName: key.Name, AccountID: value.ID, AccountName: value.Name, Provider: string(value.Provider), Model: "web-imagine-video", ModelRouteID: 1, UpstreamModel: "video", Prompt: "test", Seconds: 6, Quality: "720p", Size: "16:9", Status: mediadomain.StatusCompleted, Progress: 100, ResultAssetID: "vid_labels_result", CompletedAt: &now, InputJSON: "{}", CreatedAt: now, UpdatedAt: now}
	if err := jobs.CreateMediaJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	asset := mediadomain.Asset{ID: "img_labels_00000000001", Kind: "image", StorageKey: "labels.png", MIMEType: "image/png", SizeBytes: 1, SHA256: strings.Repeat("a", 64), CreatedAt: now, Origin: mediadomain.Origin{AccountID: value.ID, AccountName: value.Name, Provider: string(value.Provider)}}
	if err := assets.CreateMediaAsset(ctx, asset); err != nil {
		t.Fatal(err)
	}
	service := mediaapp.NewService(assets, jobs, nil, nil, mediaapp.Config{PublicBaseURL: "https://example.test"})
	router := gin.New()
	NewHandler(service).WithAccountNames(accounts).RegisterAdmin(router.Group("/api/admin/v1"))
	for _, path := range []string{"/api/admin/v1/media/jobs?search=media%40example.test", "/api/admin/v1/media/images", "/api/admin/v1/media/images/img_labels_00000000001"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"accountName":"media@example.test"`) || strings.Contains(rec.Body.String(), value.Name) {
			t.Fatalf("path=%s status=%d body=%s", path, rec.Code, rec.Body.String())
		}
	}
	stored, err := jobs.GetMediaJob(ctx, job.ID, key.ID)
	if err != nil || stored.AccountName != value.Name {
		t.Fatalf("snapshot=%+v err=%v", stored, err)
	}
	storedAsset, err := assets.GetMediaAsset(ctx, asset.ID)
	if err != nil || storedAsset.Origin.AccountName != value.Name {
		t.Fatalf("asset=%+v err=%v", storedAsset, err)
	}
}
