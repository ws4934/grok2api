package inference

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/chatgpt"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

// imageToolAdapter keeps the native ChatGPT declaration and fakes the upstream image steps.
type imageToolAdapter struct {
	*chatgpt.Adapter
	png []byte
}

func (a *imageToolAdapter) ListModels(context.Context, account.Credential) ([]string, error) {
	return []string{"gpt-5"}, nil
}

// ForwardResponse answers the conversation path without an upstream.
func (a *imageToolAdapter) ForwardResponse(context.Context, provider.ResponseResourceRequest) (*provider.Response, error) {
	body := `{"id":"resp_text","object":"response","status":"completed","model":"gpt-5","output":[]}`
	return &provider.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func (a *imageToolAdapter) SubmitImage(_ context.Context, request provider.ImageJobRequest, save func(provider.ImageCheckpoint) error) (provider.ImageCheckpoint, error) {
	if request.Prompt == "fail" {
		return provider.ImageCheckpoint{}, &providerRequestError{"upstream rejected the prompt"}
	}
	cp := provider.ImageCheckpoint{Submitted: true, UpstreamID: "conversation"}
	return cp, save(cp)
}

func (a *imageToolAdapter) PollImage(_ context.Context, _ provider.ImageJobRequest, cp provider.ImageCheckpoint) (provider.ImageCheckpoint, error) {
	cp.Outputs = []string{"file"}
	return cp, nil
}

func (a *imageToolAdapter) FetchImage(context.Context, provider.ImageJobRequest, provider.ImageCheckpoint) ([]byte, error) {
	return a.png, nil
}

type providerRequestError struct{ message string }

func (e *providerRequestError) Error() string              { return e.message }
func (e *providerRequestError) HTTPStatusCode() int        { return http.StatusBadRequest }
func (e *providerRequestError) RequestScopedFailure() bool { return true }

type imageToolFixture struct {
	gateway *gateway.Service
	router  *gin.Engine
	key     clientkeydomain.Key
}

func newImageToolFixture(t *testing.T) *imageToolFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "responses-images.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	var pixel bytes.Buffer
	if err := png.Encode(&pixel, image.NewGray(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	adapter := &imageToolAdapter{Adapter: chatgpt.NewAdapter(chatgpt.Config{ImageConversationModel: "gpt-5", VisionTokens: 256}, nil, cipher, nil), png: pixel.Bytes()}
	registry := provider.NewRegistry(adapter)
	accountRepo, modelRepo, auditRepo := relational.NewAccountRepository(database), relational.NewModelRepository(database), relational.NewAuditRepository(database)
	if err := modelRepo.UpsertRoutes(ctx, chatgpt.Routes()); err != nil {
		t.Fatal(err)
	}
	token, err := cipher.Encrypt("AT")
	if err != nil {
		t.Fatal(err)
	}
	credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderChatGPT, AuthType: account.AuthTypeOAuth, Name: "gpt", UserID: "user-1", SourceKey: "gpt",
		EncryptedAccessToken: token, ExpiresAt: time.Now().Add(24 * time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, cipher, nil)
	if _, err := modelapp.NewService(modelRepo, accountRepo, accountService, registry).SyncAccount(ctx, credential.ID); err != nil {
		t.Fatal(err)
	}
	keys := relational.NewClientKeyRepository(database)
	key, err := keys.Create(ctx, clientkeydomain.Key{
		Name: "images", Prefix: "images", SecretHash: strings.Repeat("a", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 60, MaxConcurrent: 4, ProviderScope: clientkeydomain.AllProviders(),
	})
	if err != nil {
		t.Fatal(err)
	}
	selector := gateway.NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := gateway.NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(keys, nil, nil, 60, 4, nil), registry, selector, relational.NewResponseRepository(database), 3)
	jobs := relational.NewMediaJobRepository(database)
	service.ConfigureMedia(jobs, 2)
	objects, err := localmedia.NewLocalStore(filepath.Join(t.TempDir(), "objects"))
	if err != nil {
		t.Fatal(err)
	}
	service.ConfigureMediaAssets(mediaapp.NewService(relational.NewMediaAssetRepository(database), jobs, objects, nil, mediaapp.Config{PublicBaseURL: "https://example.test", MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30, CleanupThresholdPercent: 90, CleanupInterval: time.Hour}))
	router := gin.New()
	v1 := router.Group("/v1", func(c *gin.Context) {
		c.Set(middleware.ClientKey, key)
		c.Set(middleware.RequestIDKey, "request-images")
	})
	NewHandler(service, nil, 32<<20).Register(v1)
	return &imageToolFixture{gateway: service, router: router, key: key}
}

func (f *imageToolFixture) startWorkers(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.gateway.RunMediaWorkers(ctx); close(done) }()
	t.Cleanup(func() { stop(); <-done })
}

func (f *imageToolFixture) call(t *testing.T, method, path, body string, headers ...string) (int, map[string]any) {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	for index := 0; index+1 < len(headers); index += 2 {
		request.Header.Set(headers[index], headers[index+1])
	}
	recorder := httptest.NewRecorder()
	f.router.ServeHTTP(recorder, request)
	var value map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &value)
	return recorder.Code, value
}

func TestResponsesImageToolBackgroundGetCancelAndSync(t *testing.T) {
	f := newImageToolFixture(t)
	request := `{"model":"gpt-image-2","input":"draw a cat","tools":[{"type":"image_generation","size":"1024x1024","output_format":"png"}],"background":true}`

	// Before any worker runs, a queued background response can be cancelled.
	code, queued := f.call(t, "POST", "/v1/responses", request)
	id, _ := queued["id"].(string)
	if code != 200 || queued["status"] != "queued" || queued["background"] != true || !strings.HasPrefix(id, gateway.ImageJobIDPrefix) {
		t.Fatalf("background create: %d %v", code, queued)
	}
	if code, canceled := f.call(t, "POST", "/v1/responses/"+id+"/cancel", ""); code != 200 || canceled["status"] != "cancelled" {
		t.Fatalf("cancel queued: %d %v", code, canceled)
	}

	f.startWorkers(t)
	_, created := f.call(t, "POST", "/v1/responses", request)
	id, _ = created["id"].(string)
	var final map[string]any
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if code, final = f.call(t, "GET", "/v1/responses/"+id, ""); code != 200 {
			t.Fatalf("poll: %d %v", code, final)
		}
		if final["status"] != "queued" && final["status"] != "in_progress" {
			break
		}
	}
	output, _ := final["output"].([]any)
	if final["status"] != "completed" || len(output) != 1 || final["model"] != "gpt-image-2" {
		t.Fatalf("background result: %v", final)
	}
	item := output[0].(map[string]any)
	raw, err := base64.StdEncoding.DecodeString(item["result"].(string))
	if err != nil || item["type"] != "image_generation_call" || item["status"] != "completed" || item["size"] != "1x1" || item["output_format"] != "png" {
		t.Fatalf("image_generation_call: %v", item)
	}
	if _, err := png.Decode(bytes.NewReader(raw)); err != nil {
		t.Fatal("result is not the generated PNG", err)
	}
	if usage, _ := final["usage"].(map[string]any); usage == nil || usage["output_tokens"] != float64(2) {
		t.Fatalf("usage: %v", final["usage"])
	}
	// Cancelling a finished response is idempotent and returns the final object.
	if code, again := f.call(t, "POST", "/v1/responses/"+id+"/cancel", ""); code != 200 || again["status"] != "completed" {
		t.Fatalf("cancel completed: %d %v", code, again)
	}
	// Without background the request waits and returns the completed response.
	code, sync := f.call(t, "POST", "/v1/responses", strings.Replace(request, `,"background":true`, "", 1))
	if code != 200 || sync["status"] != "completed" || sync["background"] != false {
		t.Fatalf("synchronous response: %d %v", code, sync)
	}
	// A request the provider rejects fails with the official error object.
	code, failed := f.call(t, "POST", "/v1/responses", `{"model":"gpt-image-2","input":"fail","tools":[{"type":"image_generation"}]}`)
	if problem, _ := failed["error"].(map[string]any); code != 200 || failed["status"] != "failed" || problem["code"] != "invalid_request" {
		t.Fatalf("failed response: %d %v", code, failed)
	}
}

func TestResponsesImageToolIdempotencyKey(t *testing.T) {
	f := newImageToolFixture(t)
	request := `{"model":"gpt-image-2","input":"draw a cat","tools":[{"type":"image_generation"}],"background":true}`
	_, first := f.call(t, "POST", "/v1/responses", request, "Idempotency-Key", "client-1")
	_, second := f.call(t, "POST", "/v1/responses", request, "Idempotency-Key", "client-1")
	if first["id"] == nil || first["id"] != second["id"] {
		t.Fatalf("idempotent responses = %v, %v", first["id"], second["id"])
	}
	code, conflict := f.call(t, "POST", "/v1/responses", strings.Replace(request, "a cat", "a dog", 1), "Idempotency-Key", "client-1")
	if problem, _ := conflict["error"].(map[string]any); code != http.StatusConflict || problem["code"] != "idempotency_conflict" {
		t.Fatalf("reused key with another body: %d %v", code, conflict)
	}
}

func TestResponsesImageToolRoutingAndValidation(t *testing.T) {
	f := newImageToolFixture(t)
	for name, tc := range map[string]struct {
		method, path, body string
		code               int
		errorCode          string
	}{
		"unknown stored response stays on the stored path": {"GET", "/v1/responses/resp_legacy", "", 404, "response_not_found"},
		"unknown image response":                           {"GET", "/v1/responses/" + gateway.ImageJobIDPrefix + "missing", "", 404, "response_not_found"},
		"non-image model keeps the conversation path":      {"POST", "/v1/responses", `{"model":"grok-4","input":"x","tools":[{"type":"image_generation"}]}`, 404, "model_not_found"},
		"unsupported tool option":                          {"POST", "/v1/responses", `{"model":"gpt-image-2","input":"x","tools":[{"type":"image_generation","output_compression":50}]}`, 400, "unsupported_parameter"},
		"unsupported top-level parameter":                  {"POST", "/v1/responses", `{"model":"gpt-image-2","input":"x","metadata":{},"tools":[{"type":"image_generation"}]}`, 400, "unsupported_parameter"},
		"background streaming is rejected":                 {"POST", "/v1/responses", `{"model":"gpt-image-2","input":"x","stream":true,"background":true,"tools":[{"type":"image_generation"}]}`, 400, "unsupported_parameter"},
		"provider rejects the options":                     {"POST", "/v1/responses", `{"model":"gpt-image-2","input":"x","tools":[{"type":"image_generation","quality":"ultra"}]}`, 400, "invalid_parameter"},
		"cancel of a stored response":                      {"POST", "/v1/responses/resp_legacy/cancel", "", 404, "response_not_found"},
	} {
		code, body := f.call(t, tc.method, tc.path, tc.body)
		problem, _ := body["error"].(map[string]any)
		if code != tc.code || problem["code"] != tc.errorCode {
			t.Errorf("%s: got %d %v", name, code, body)
		}
	}
}

func TestResponsesImageToolDefaultModelAndStream(t *testing.T) {
	f := newImageToolFixture(t)
	f.startWorkers(t)
	isImage := func(body map[string]any) bool {
		output, _ := body["output"].([]any)
		return len(output) == 1 && output[0].(map[string]any)["type"] == "image_generation_call"
	}
	request := `{"model":"gpt-5","input":"draw a cat","tools":[{"type":"image_generation"}],"background":true}`
	if code, body := f.call(t, "POST", "/v1/responses", request); code != 200 || body["id"] != "resp_text" {
		t.Fatalf("no default image model: %d %v", code, body)
	}
	f.gateway.UpdateDefaultImageModel("gpt-image-2")
	code, body := f.call(t, "POST", "/v1/responses", strings.Replace(request, `,"background":true`, "", 1))
	if code != 200 || body["status"] != "completed" || body["model"] != "gpt-5" || !isImage(body) {
		t.Fatalf("default image model: %d %v", code, body)
	}

	recorder := httptest.NewRecorder()
	streamRequest := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-image-2","input":"draw a cat","stream":true,"tools":[{"type":"image_generation"}]}`))
	streamRequest.Header.Set("Content-Type", "application/json")
	f.router.ServeHTTP(recorder, streamRequest)
	var names []string
	var completed map[string]any
	for _, block := range strings.Split(recorder.Body.String(), "\n\n") {
		var name, data string
		for _, line := range strings.Split(block, "\n") {
			if value, ok := strings.CutPrefix(line, "event: "); ok {
				name = value
			} else if value, ok := strings.CutPrefix(line, "data: "); ok {
				data = value
			}
		}
		if name == "" {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(data), &event) != nil || event["type"] != name || event["sequence_number"] != float64(len(names)) {
			t.Fatalf("event %s: %s", name, data)
		}
		names = append(names, name)
		if name == "response.completed" {
			completed, _ = event["response"].(map[string]any)
		}
	}
	want := "response.created,response.in_progress,response.output_item.added,response.image_generation_call.in_progress,response.image_generation_call.generating,response.image_generation_call.completed,response.output_item.done,response.completed"
	if strings.Join(names, ",") != want || !isImage(completed) || completed["status"] != "completed" {
		t.Fatalf("stream events = %v completed = %v", names, completed)
	}
}
