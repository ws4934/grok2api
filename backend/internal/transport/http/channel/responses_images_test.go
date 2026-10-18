package channelhttp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	app "github.com/chenyme/grok2api/backend/internal/application/channel"
	keyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	domain "github.com/chenyme/grok2api/backend/internal/domain/channel"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

var onePixelPNG = func() string {
	var b bytes.Buffer
	_ = png.Encode(&b, image.NewGray(image.Rect(0, 0, 1, 1)))
	return base64.StdEncoding.EncodeToString(b.Bytes())
}()

type imageModule struct{}

func (imageModule) Descriptor() domain.ChannelDescriptor {
	return domain.ChannelDescriptor{ID: "fixture", Name: "Fixture", Version: 1, Capabilities: domain.Capabilities{ImageGeneration: true, ImageEdit: true}}
}
func (imageModule) ParseImport(b []byte) ([]domain.ImportRecord, error) {
	var v []domain.ImportRecord
	return v, json.Unmarshal(b, &v)
}
func (imageModule) Prepare(_ context.Context, c domain.Credentials) (domain.Credentials, error) {
	return c, nil
}
func (imageModule) Validate(context.Context, domain.Credentials) (domain.Snapshot, error) {
	return domain.Snapshot{Identity: domain.Identity{UserID: "u"}, Models: []string{"m"}, Quotas: []domain.FeatureQuota{}}, nil
}
func (imageModule) Refresh(_ context.Context, c domain.Credentials) (domain.Credentials, error) {
	return c, nil
}
func (imageModule) PrepareImage(_ context.Context, _ domain.Credentials, r domain.ImageRequest, _ func(domain.ImageInput) (domain.Blob, error)) (domain.PreparedImage, error) {
	return domain.PreparedImage{Request: r}, nil
}
func (imageModule) SubmitImage(_ context.Context, _ domain.Credentials, _ domain.PreparedImage, save func(domain.Checkpoint) error) (domain.Checkpoint, error) {
	cp := domain.Checkpoint{Version: 1, Submitted: true, ConversationID: "conversation"}
	return cp, save(cp)
}
func (imageModule) PollImage(_ context.Context, _ domain.Credentials, cp domain.Checkpoint) (domain.Checkpoint, error) {
	cp.FileIDs = []string{"file"}
	return cp, nil
}
func (imageModule) FetchImage(context.Context, domain.Credentials, domain.Checkpoint) ([]domain.Blob, error) {
	b, _ := base64.StdEncoding.DecodeString(onePixelPNG)
	return []domain.Blob{{Data: b, MIME: "image/png"}}, nil
}

type auditWriter struct{ repo *relational.AuditRepository }

func (a auditWriter) CreateDurable(ctx context.Context, r audit.Record) error {
	return a.repo.Create(ctx, r)
}

// responsesFixture serves Dispatch in front of stub legacy routes that answer 418.
func responsesFixture(t *testing.T) (*app.Service, *gin.Engine) {
	t.Helper()
	ctx := context.Background()
	db, e := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "responses.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = db.Close() })
	if e := db.InitializeSchema(ctx); e != nil {
		t.Fatal(e)
	}
	cipher, e := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if e != nil {
		t.Fatal(e)
	}
	registry, e := domain.NewRegistry(imageModule{})
	if e != nil {
		t.Fatal(e)
	}
	repo, keys := relational.NewChannelRepository(db), relational.NewClientKeyRepository(db)
	s := app.NewService(registry, repo, cipher, memory.NewLockStore(), relational.NewModelRepository(db))
	s.SetClientKeys(keys)
	encrypted, _ := cipher.Encrypt("secret")
	key, e := keys.Create(ctx, clientkey.Key{Name: "k", Prefix: "k", SecretHash: strings.Repeat("a", 64), EncryptedSecret: encrypted, Enabled: true, RPMLimit: 60, MaxConcurrent: 4, BillingLimitUSDTicks: 1000000})
	if e != nil {
		t.Fatal(e)
	}
	if e := s.SetExecution(keyapp.NewService(keys, memory.NewRateLimiter(), memory.NewConcurrencyLimiter(), 60, 4, cipher), auditWriter{relational.NewAuditRepository(db)}); e != nil {
		t.Fatal(e)
	}
	objects, e := localmedia.NewLocalStore(filepath.Join(t.TempDir(), "objects"))
	if e != nil {
		t.Fatal(e)
	}
	s.SetImages(repo, mediaapp.NewService(relational.NewMediaAssetRepository(db), relational.NewMediaJobRepository(db), objects, nil, mediaapp.Config{PublicBaseURL: "https://example.test", MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30}))
	imported, e := s.Import(ctx, "fixture", []byte(`[{"name":"a","credentials":{"version":1,"access_token":"AT"}}]`), false)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := s.AccountAction(ctx, "fixture", imported[0].AccountID, "validate"); e != nil {
		t.Fatal(e)
	}
	cfg := domain.DefaultSettings()
	cfg.Enabled, cfg.ImageConversationModel = true, "m"
	if e := s.SaveSettings(ctx, "fixture", cfg); e != nil {
		t.Fatal(e)
	}
	if _, e := s.SavePrice(ctx, "fixture", domain.Price{Model: "m", Operation: "image", ImageTicks: 25}); e != nil {
		t.Fatal(e)
	}
	if _, e := s.CreateModel(ctx, "fixture", app.ModelInput{PublicID: "img", UpstreamModel: "m", Operation: "image", Enabled: true}); e != nil {
		t.Fatal(e)
	}
	if e := s.SetGrant(ctx, "fixture", key.ID, true); e != nil {
		t.Fatal(e)
	}
	h := NewHandler(s)
	r := gin.New()
	v1 := r.Group("/v1", func(c *gin.Context) { c.Set(middleware.ClientKey, key) })
	v1.Use(h.Dispatch())
	h.RegisterPublic(v1)
	legacy := func(c *gin.Context) { c.Status(418) }
	v1.POST("/responses", legacy)
	v1.GET("/responses/:responseId", legacy)
	return s, r
}
func call(t *testing.T, r *gin.Engine, method, path, body string) (int, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	var v map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	return w.Code, v
}

func TestResponsesImageBackgroundLifecycle(t *testing.T) {
	s, r := responsesFixture(t)
	request := `{"model":"img","input":"draw a cat","tools":[{"type":"image_generation","size":"1024x1024","output_format":"png"}],"background":true}`

	// Before any worker runs, a queued background response can be cancelled.
	code, queued := call(t, r, "POST", "/v1/responses", request)
	if code != 200 || queued["status"] != "queued" || queued["background"] != true || !strings.HasPrefix(queued["id"].(string), "resp_") {
		t.Fatalf("background create: %d %v", code, queued)
	}
	code, canceled := call(t, r, "POST", "/v1/responses/"+queued["id"].(string)+"/cancel", "")
	if code != 200 || canceled["status"] != "cancelled" {
		t.Fatalf("cancel queued: %d %v", code, canceled)
	}

	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.RunImages(ctx); close(done) }()
	t.Cleanup(func() { stop(); <-done })

	_, created := call(t, r, "POST", "/v1/responses", request)
	id := created["id"].(string)
	var final map[string]any
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		code, final = call(t, r, "GET", "/v1/responses/"+id, "")
		if code != 200 {
			t.Fatalf("poll: %d %v", code, final)
		}
		if final["status"] != "queued" && final["status"] != "in_progress" {
			break
		}
	}
	output, _ := final["output"].([]any)
	if final["status"] != "completed" || len(output) != 1 {
		t.Fatalf("background result: %v", final)
	}
	item := output[0].(map[string]any)
	raw, e := base64.StdEncoding.DecodeString(item["result"].(string))
	if e != nil || item["type"] != "image_generation_call" || item["status"] != "completed" || item["size"] != "1x1" {
		t.Fatalf("image_generation_call: %v", item)
	}
	if _, e := png.Decode(bytes.NewReader(raw)); e != nil {
		t.Fatal("result is not the generated PNG", e)
	}
	// Cancelling a finished response is idempotent and returns the final object.
	if code, again := call(t, r, "POST", "/v1/responses/"+id+"/cancel", ""); code != 200 || again["status"] != "completed" {
		t.Fatalf("cancel completed: %d %v", code, again)
	}

	// Without background the request waits and returns the completed response.
	code, sync := call(t, r, "POST", "/v1/responses", strings.Replace(request, `,"background":true`, "", 1))
	if code != 200 || sync["status"] != "completed" || sync["background"] != false {
		t.Fatalf("synchronous response: %d %v", code, sync)
	}
}

func TestResponsesImageRoutingAndValidation(t *testing.T) {
	_, r := responsesFixture(t)
	for name, tc := range map[string]struct {
		method, path, body string
		code               int
	}{
		"unknown response id falls through": {"GET", "/v1/responses/resp_legacy", "", 418},
		"legacy model keeps legacy handler": {"POST", "/v1/responses", `{"model":"grok","input":"x","tools":[{"type":"image_generation"}]}`, 418},
		"chat request keeps legacy handler": {"POST", "/v1/responses", `{"model":"grok","input":"x"}`, 418},
		"unsupported tool option":           {"POST", "/v1/responses", `{"model":"img","input":"x","tools":[{"type":"image_generation","output_compression":50}]}`, 400},
		"unsupported top-level parameter":   {"POST", "/v1/responses", `{"model":"img","input":"x","metadata":{},"tools":[{"type":"image_generation"}]}`, 400},
		"streaming is rejected":             {"POST", "/v1/responses", `{"model":"img","input":"x","stream":true,"tools":[{"type":"image_generation"}]}`, 400},
		"extra tools are rejected":          {"POST", "/v1/responses", `{"model":"img","input":"x","tools":[{"type":"image_generation"},{"type":"web_search"}]}`, 400},
		"edit without an edit route":        {"POST", "/v1/responses", `{"model":"img","input":[{"role":"user","content":[{"type":"input_text","text":"x"},{"type":"input_image","image_url":"data:image/png;base64,` + onePixelPNG + `"}]}],"tools":[{"type":"image_generation"}]}`, 404},
		"cancel unknown response":           {"POST", "/v1/responses/resp_missing/cancel", "", 404},
	} {
		if code, body := call(t, r, tc.method, tc.path, tc.body); code != tc.code {
			t.Errorf("%s: got %d %v", name, code, body)
		}
	}
}

func TestDecodeImageResponseBuildsPromptAndInputs(t *testing.T) {
	_, in, e := decodeImageResponse([]byte(`{"model":"gpt-5","instructions":"house style","input":[{"role":"user","content":[{"type":"input_text","text":"replace the sky"},{"type":"input_image","image_url":"https://example.com/a.png"}]}],"tools":[{"type":"image_generation","action":"edit","partial_images":2,"input_image_mask":{"image_url":"https://example.com/m.png"}}],"tool_choice":{"type":"image_generation"}}`))
	if e != nil || in.Prompt != "house style\n\nreplace the sky" || len(in.Images) != 1 || in.Tool.Mask == nil {
		t.Fatalf("decoded: %+v %v", in, e)
	}
	for _, body := range []string{
		`{"model":"m","input":"x","tools":[{"type":"image_generation","action":"edit"}]}`,
		`{"model":"m","input":"x","tools":[{"type":"image_generation","partial_images":4}]}`,
		`{"model":"m","input":"x","tools":[{"type":"image_generation","input_image_mask":{"file_id":"f"}}]}`,
		`{"model":"m","input":[{"role":"assistant","content":"earlier"}],"tools":[{"type":"image_generation"}]}`,
		`{"model":"m","input":"x","tool_choice":"none","tools":[{"type":"image_generation"}]}`,
	} {
		if _, _, e := decodeImageResponse([]byte(body)); e == nil {
			t.Errorf("accepted unsupported request: %s", body)
		}
	}
}
