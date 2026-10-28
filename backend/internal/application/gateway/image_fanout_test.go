package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	pricingapp "github.com/chenyme/grok2api/backend/internal/application/pricing"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	pricingdomain "github.com/chenyme/grok2api/backend/internal/domain/pricing"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

// singleImageAdapter draws one image per upstream request, like ChatGPT Web.
type singleImageAdapter struct {
	staleTokenImageAdapter
	delay     time.Duration
	failFor   uint64
	drawMu    sync.Mutex
	byAccount map[uint64]int
	counts    []int
}

func (a *singleImageAdapter) Definition() provider.Definition {
	definition := a.staleTokenImageAdapter.Definition()
	definition.Media.ImageEdit = true
	definition.Media.SingleImagePerRequest = true
	return definition
}

func (a *singleImageAdapter) draw(credential account.Credential, count int) (*provider.Response, error) {
	a.drawMu.Lock()
	a.byAccount[credential.ID]++
	a.counts = append(a.counts, count)
	drawn := len(a.counts)
	a.drawMu.Unlock()
	time.Sleep(a.delay)
	if credential.ID == a.failFor {
		return nil, errors.New("upstream drawing failed")
	}
	body := fmt.Sprintf(`{"created":1,"data":[{"b64_json":"image-%d"}],"usage":{"input_tokens":5,"output_tokens":100,"total_tokens":105,"output_tokens_details":{"image_tokens":100}}}`, drawn)
	return &provider.Response{
		StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": {"application/json"}, "Content-Length": {"999"}},
		Body: io.NopCloser(strings.NewReader(body)), QuotaUnits: 1,
	}, nil
}

func (a *singleImageAdapter) GenerateImage(_ context.Context, request provider.ImageGenerationRequest) (*provider.Response, error) {
	return a.draw(request.Credential, request.Count)
}

func (a *singleImageAdapter) EditImage(_ context.Context, request provider.ImageEditRequest) (*provider.Response, error) {
	return a.draw(request.Credential, request.Count)
}

type fanOutFixture struct {
	service  *Service
	audits   *relational.AuditRepository
	keys     *relational.ClientKeyRepository
	key      clientkey.Key
	accounts []uint64
}

// newFanOutFixture serves "img-fan" (image and edit) from accounts that each
// take one request at a time, priced at 1 USD per image.
func newFanOutFixture(t *testing.T, accounts int, adapter *singleImageAdapter) fanOutFixture {
	t.Helper()
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "fan.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo, modelRepo, auditRepo := relational.NewAccountRepository(database), relational.NewModelRepository(database), relational.NewAuditRepository(database)
	keyRepo := relational.NewClientKeyRepository(database)
	if err := modelRepo.UpsertRoutes(ctx, []modeldomain.Route{
		{PublicID: "img-fan", Provider: account.ProviderBuild, UpstreamModel: "img-fan", Capability: modeldomain.CapabilityImage, Enabled: true},
		{PublicID: "img-fan-edit", Provider: account.ProviderBuild, UpstreamModel: "img-fan-edit", Capability: modeldomain.CapabilityImageEdit, Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	fixture := fanOutFixture{audits: auditRepo, keys: keyRepo}
	for index := range accounts {
		name := fmt.Sprintf("fan-%d", index)
		credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: name, SourceKey: name,
			EncryptedAccessToken: "fresh", EncryptedRefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"img-fan", "img-fan-edit"}, time.Now()); err != nil {
			t.Fatal(err)
		}
		fixture.accounts = append(fixture.accounts, credential.ID)
	}
	fixture.key, err = keyRepo.Create(ctx, clientkey.Key{
		Name: "k", Prefix: "kkkk", SecretHash: strings.Repeat("f", 64), EncryptedSecret: "encrypted",
		Enabled: true, RPMLimit: 60, MaxConcurrent: 4, BillingLimitUSDTicks: 100_000_000_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter.byAccount = map[uint64]int{}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	fixture.service = NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(keyRepo, nil, nil, 60, 4, nil), registry, selector, relational.NewResponseRepository(database), 1)
	prices, err := pricingapp.NewService(ctx, relational.NewModelPriceRepository(database), func(account.Provider) bool { return true }, registry.PricingModel)
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []pricingapp.Input{
		{Provider: string(account.ProviderBuild), PricingModel: "img-fan", Operation: "image", Rates: map[pricingdomain.Component]string{pricingdomain.OutputImage: "1"}},
		{Provider: string(account.ProviderBuild), PricingModel: "img-fan-edit", Operation: "image_edit", Rates: map[pricingdomain.Component]string{pricingdomain.InputImage: "0", pricingdomain.OutputImage: "1"}},
	} {
		if _, err := prices.Put(ctx, input); err != nil {
			t.Fatal(err)
		}
	}
	fixture.service.SetPricing(prices)
	return fixture
}

type fannedResponse struct {
	Data  []map[string]any `json:"data"`
	Usage struct {
		InputTokens        float64            `json:"input_tokens"`
		OutputTokens       float64            `json:"output_tokens"`
		TotalTokens        float64            `json:"total_tokens"`
		OutputTokenDetails map[string]float64 `json:"output_tokens_details"`
	} `json:"usage"`
}

// readFanned reads the merged response like the transport does.
func readFanned(t *testing.T, result *Result) fannedResponse {
	t.Helper()
	raw, err := io.ReadAll(result.Body)
	result.Finalize(Usage{}, "", "")
	_ = result.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var response fannedResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatalf("body = %s: %v", raw, err)
	}
	return response
}

// checkFannedAudits expects one successful one-image audit per delivered
// image under the client's request ID, plus failures failures, and the
// delivered images billed.
func checkFannedAudits(t *testing.T, fixture fanOutFixture, requestID string, delivered, failures int) {
	t.Helper()
	ctx := context.Background()
	records, total, err := fixture.audits.List(ctx, 0, 20)
	if err != nil || int(total) != delivered+failures {
		t.Fatalf("audits = %d, err = %v, want %d", total, err, delivered+failures)
	}
	successes := 0
	for _, record := range records {
		if record.RequestID != requestID {
			t.Fatalf("request id = %q", record.RequestID)
		}
		if record.ErrorCode == "" && record.StatusCode == http.StatusOK {
			successes++
			if record.MediaOutputImages != 1 {
				t.Fatalf("images per audit = %d", record.MediaOutputImages)
			}
		}
	}
	key, err := fixture.keys.Get(ctx, fixture.key.ID)
	if err != nil {
		t.Fatal(err)
	}
	const oneImage = 10_000_000_000 // 1 USD
	if successes != delivered || key.BilledUsageUSDTicks != int64(delivered)*oneImage || key.ReservedUsageUSDTicks != 0 {
		t.Fatalf("successes = %d billed = %d reserved = %d, want %d images", successes, key.BilledUsageUSDTicks, key.ReservedUsageUSDTicks, delivered)
	}
}

func TestImagesFanOutAcrossAccounts(t *testing.T) {
	adapter := &singleImageAdapter{delay: 100 * time.Millisecond}
	fixture := newFanOutFixture(t, 3, adapter)
	result, err := fixture.service.GenerateImage(context.Background(), ImageGenerationInput{RequestID: "req-fan", ClientKey: fixture.key, PublicModel: "img-fan", Prompt: "fox", Count: 3})
	if err != nil {
		t.Fatal(err)
	}
	if result.Header.Get("Content-Length") != "" {
		t.Fatal("merged response kept a single image's Content-Length")
	}
	response := readFanned(t, result)
	if len(response.Data) != 3 || response.Usage.InputTokens != 5 || response.Usage.OutputTokens != 300 || response.Usage.TotalTokens != 305 || response.Usage.OutputTokenDetails["image_tokens"] != 300 {
		t.Fatalf("response = %+v", response)
	}
	if len(adapter.byAccount) != 3 {
		t.Fatalf("images per account = %v, want one on each", adapter.byAccount)
	}
	for _, count := range adapter.counts {
		if count != 1 {
			t.Fatalf("upstream counts = %v", adapter.counts)
		}
	}
	checkFannedAudits(t, fixture, "req-fan", 3, 0)
}

// With one account the extra images find it busy and are drawn in later
// rounds, with no failure recorded.
func TestImagesFanOutReusesABusyAccount(t *testing.T) {
	adapter := &singleImageAdapter{}
	fixture := newFanOutFixture(t, 1, adapter)
	result, err := fixture.service.GenerateImage(context.Background(), ImageGenerationInput{RequestID: "req-one", ClientKey: fixture.key, PublicModel: "img-fan", Prompt: "fox", Count: 3})
	if err != nil {
		t.Fatal(err)
	}
	if response := readFanned(t, result); len(response.Data) != 3 || adapter.byAccount[fixture.accounts[0]] != 3 {
		t.Fatalf("images = %d, per account = %v", len(response.Data), adapter.byAccount)
	}
	checkFannedAudits(t, fixture, "req-one", 3, 0)
}

// A plain draw fails when a sibling draw holds the only free account, which
// returned fewer images with a false 503. Draws that run side by side are
// therefore all extra; only a round that found no free account is followed
// by one plain draw, failing as the request would without fan-out.
func TestImagesFanOutDrawsPlainOnlyAfterABusyRound(t *testing.T) {
	for _, free := range []bool{true, false} {
		var mu sync.Mutex
		extras, plains := 0, 0
		// An extra draw takes a free account; a plain draw always finds it
		// taken by a sibling, the race's losing interleaving.
		draw := func(_ context.Context, _ int, extra bool) (*Result, error) {
			mu.Lock()
			defer mu.Unlock()
			if !extra {
				plains++
				return nil, ErrNoAvailableAccount
			}
			extras++
			if !free {
				return nil, errImageAccountsBusy
			}
			return &Result{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"data":[{}]}`)), Finalize: func(Usage, string, string) {}, imageBatchSize: 1, imageOutputCount: func() int { return 1 }}, nil
		}
		result, err := new(Service).drawImageBatches(context.Background(), 3, true, draw)
		if free {
			if err != nil || len(readFanned(t, result).Data) != 3 || plains != 0 {
				t.Fatalf("free account: err = %v, plain draws = %d", err, plains)
			}
			continue
		}
		if !errors.Is(err, ErrNoAvailableAccount) || result != nil || extras != 3 || plains != 1 {
			t.Fatalf("no account: result = %v, err = %v, extra draws = %d, plain draws = %d", result, err, extras, plains)
		}
	}
}

func TestImagesFanOutReturnsTheImagesDrawnBeforeAFailure(t *testing.T) {
	adapter := &singleImageAdapter{delay: 50 * time.Millisecond}
	fixture := newFanOutFixture(t, 2, adapter)
	adapter.failFor = fixture.accounts[1]
	result, err := fixture.service.GenerateImage(context.Background(), ImageGenerationInput{RequestID: "req-fail", ClientKey: fixture.key, PublicModel: "img-fan", Prompt: "fox", Count: 2})
	if err != nil {
		t.Fatal(err)
	}
	if response := readFanned(t, result); len(response.Data) != 1 {
		t.Fatalf("images = %d", len(response.Data))
	}
	checkFannedAudits(t, fixture, "req-fail", 1, 1)
}

func TestImageEditsFanOutAcrossAccounts(t *testing.T) {
	adapter := &singleImageAdapter{delay: 50 * time.Millisecond}
	fixture := newFanOutFixture(t, 2, adapter)
	result, err := fixture.service.EditImage(context.Background(), ImageEditInput{RequestID: "req-edit", ClientKey: fixture.key, PublicModel: "img-fan-edit", Prompt: "fox", ImageURLs: []string{"data:image/png;base64,AA=="}, Count: 2})
	if err != nil {
		t.Fatal(err)
	}
	if response := readFanned(t, result); len(response.Data) != 2 || len(adapter.byAccount) != 2 {
		t.Fatalf("images = %d, per account = %v", len(response.Data), adapter.byAccount)
	}
	records, _, err := fixture.audits.List(context.Background(), 0, 5)
	if err != nil || len(records) != 2 || records[0].Operation != audit.OperationImageEdit {
		t.Fatalf("audits = %+v, err = %v", records, err)
	}
}
