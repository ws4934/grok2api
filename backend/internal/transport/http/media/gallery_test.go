package media

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/gin-gonic/gin"
)

func TestAdminGalleryFiltersAndDetailExposeOrigin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "media-gallery.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	objects, err := localmedia.NewLocalStore(filepath.Join(t.TempDir(), "objects-gallery"))
	if err != nil {
		t.Fatal(err)
	}
	service := mediaapp.NewService(relational.NewMediaAssetRepository(database), relational.NewMediaJobRepository(database), objects, nil,
		mediaapp.Config{PublicBaseURL: "https://api.example", MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30, CleanupThresholdPercent: 80, CleanupInterval: time.Minute})
	raw, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	origin := mediadomain.Origin{
		Provider: "chatgpt", Model: "chatgpt/gpt-image-2", UpstreamModel: "chatgpt/gpt-image-2", ClientKeyID: 4, ClientKeyName: "key",
		AccountID: 8, AccountName: "account", Operation: "image_edit", RequestID: "req-gallery", Prompt: "a fox",
	}
	attributed, err := service.SaveImage(mediadomain.WithOrigin(ctx, origin), raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SaveImage(ctx, raw); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	NewHandler(service).RegisterAdmin(router.Group("/api/admin/v1"))
	get := func(path string) (int, json.RawMessage) {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/v1/media/images"+path, nil))
		var envelope struct {
			Data json.RawMessage `json:"data"`
		}
		_ = json.Unmarshal(recorder.Body.Bytes(), &envelope)
		return recorder.Code, envelope.Data
	}
	want := mediaAssetDTO{
		ID: attributed.ID, Kind: "image", MimeType: "image/png", SizeBytes: attributed.SizeBytes, SHA256: attributed.SHA256,
		CreatedAt: attributed.CreatedAt.Format("2006-01-02T15:04:05Z"), URL: "https://api.example/v1/media/images/" + attributed.ID,
		Provider: origin.Provider, Model: origin.Model, UpstreamModel: origin.UpstreamModel, ClientKeyID: 4, ClientKeyName: "key",
		AccountID: 8, AccountName: "account", Operation: "image_edit", RequestID: "req-gallery", Prompt: "a fox",
	}

	status, data := get("?provider=chatgpt&clientKeyId=4&accountId=8&operation=image_edit&from=2000-01-01T00:00:00Z&to=2999-01-01T00:00:00%2B08:00")
	var page struct {
		Items []mediaAssetDTO `json:"items"`
		Total int64           `json:"total"`
	}
	if err := json.Unmarshal(data, &page); status != http.StatusOK || err != nil || page.Total != 1 || len(page.Items) != 1 || page.Items[0] != want {
		t.Fatalf("filtered list status = %d, page = %+v, err = %v", status, page, err)
	}
	if status, data = get("?kind=video"); status != http.StatusOK || json.Unmarshal(data, &page) != nil || page.Total != 0 {
		t.Fatalf("video list status = %d, body = %s", status, data)
	}
	var detail mediaAssetDTO
	if status, data = get("/" + attributed.ID); status != http.StatusOK || json.Unmarshal(data, &detail) != nil || detail != want {
		t.Fatalf("detail status = %d, body = %s", status, data)
	}
	for path, code := range map[string]int{
		"/img_missing_asset_000000": http.StatusNotFound,
		"?kind=audio":               http.StatusBadRequest,
		"?clientKeyId=abc":          http.StatusBadRequest,
		"?from=yesterday":           http.StatusBadRequest,
		"/stats":                    http.StatusOK,
	} {
		if status, _ := get(path); status != code {
			t.Fatalf("GET %s status = %d, want %d", path, status, code)
		}
	}
}
