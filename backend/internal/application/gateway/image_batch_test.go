package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/console"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

type nativeImageCall struct {
	account uint64
	options provider.ImageRequestOptions
	inputs  []string
}

type nativeConsoleImageStub struct {
	*console.Adapter
	mu      sync.Mutex
	calls   []nativeImageCall
	bodies  []*trackedBody
	partial int
	failure error
	before  func()
}

func (a *nativeConsoleImageStub) draw(id uint64, o provider.ImageRequestOptions, inputs []string) (*provider.Response, error) {
	a.mu.Lock()
	a.calls = append(a.calls, nativeImageCall{id, o, slices.Clone(inputs)})
	a.mu.Unlock()
	if a.before != nil {
		a.before()
	}
	if a.failure != nil {
		return nil, a.failure
	}
	count := o.Count
	if a.partial > 0 {
		count = min(count, a.partial)
	}
	items := make([]map[string]string, count)
	for i := range items {
		items[i] = map[string]string{"url": "https://example.test/generated.png"}
		if o.ResponseFormat == "b64_json" {
			items[i] = map[string]string{"b64_json": "aW1hZ2U="}
		}
	}
	body, _ := json.Marshal(map[string]any{"created": 1, "data": items})
	reader := &trackedBody{Reader: strings.NewReader(string(body))}
	a.mu.Lock()
	a.bodies = append(a.bodies, reader)
	a.mu.Unlock()
	return &provider.Response{StatusCode: 200, Status: "200 OK", Header: http.Header{"Content-Type": {"application/json"}}, Body: reader}, nil
}

func (a *nativeConsoleImageStub) GenerateImage(_ context.Context, r provider.ImageGenerationRequest) (*provider.Response, error) {
	return a.draw(r.Credential.ID, r.Options(), nil)
}

func (a *nativeConsoleImageStub) EditImage(_ context.Context, r provider.ImageEditRequest) (*provider.Response, error) {
	return a.draw(r.Credential.ID, r.Options(), r.ImageURLs)
}

type nativeImageFixture struct {
	service  *Service
	accounts *relational.AccountRepository
	audits   *relational.AuditRepository
	jobs     *relational.MediaJobRepository
	keys     *relational.ClientKeyRepository
	key      clientkey.Key
	ids      []uint64
	adapter  *nativeConsoleImageStub
}

func newNativeImageFixture(t *testing.T, balances ...int) nativeImageFixture {
	t.Helper()
	ctx := context.Background()
	db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "native-images.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	f := nativeImageFixture{accounts: relational.NewAccountRepository(db), audits: relational.NewAuditRepository(db), keys: relational.NewClientKeyRepository(db), adapter: &nativeConsoleImageStub{Adapter: console.NewAdapter(console.Config{}, nil, nil, nil)}}
	f.jobs = relational.NewMediaJobRepository(db)
	models := relational.NewModelRepository(db)
	routes := console.Routes()
	for i := range routes {
		routes[i].Origin = modeldomain.OriginCatalog
	}
	if err := models.UpsertRoutes(ctx, routes); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i, balance := range balances {
		value, _, err := f.accounts.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderConsole, AuthType: account.AuthTypeSSO, Name: string(rune('A' + i)), SourceKey: string(rune('A' + i)), EncryptedAccessToken: "test-only", Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 4})
		if err != nil {
			t.Fatal(err)
		}
		f.ids = append(f.ids, value.ID)
		if balance < 0 {
			continue // No authoritative quota snapshot yet.
		}
		var windows []account.QuotaWindow
		for _, mode := range []string{console.QuotaMode, console.QuotaModeImage, console.QuotaModeVideo} {
			windows = append(windows, account.QuotaWindow{AccountID: value.ID, Mode: mode, Remaining: balance, Total: max(balance, 100), Source: account.QuotaSourceUpstream, SyncedAt: &now})
		}
		if err := f.accounts.SaveQuotaWindows(ctx, value.ID, "", now, windows); err != nil {
			t.Fatal(err)
		}
	}
	f.key, err = f.keys.Create(ctx, clientkey.Key{Name: "native", Prefix: "native", SecretHash: strings.Repeat("a", 64), EncryptedSecret: "test-only", Enabled: true, RPMLimit: 60, MaxConcurrent: 4, BillingLimitUSDTicks: 1_000_000_000_000})
	if err != nil {
		t.Fatal(err)
	}
	registry := provider.NewRegistry(f.adapter)
	sticky := memory.NewStickyStore()
	accounts := accountapp.NewService(f.accounts, f.audits, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(f.accounts, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	f.service = NewService(models, f.audits, accounts, clientkeyapp.NewService(f.keys, nil, nil, 60, 4, nil), registry, selector, relational.NewResponseRepository(db), 2)
	return f
}

func (f nativeImageFixture) generate(t *testing.T, count int, format string) (*Result, error) {
	t.Helper()
	return f.service.GenerateImage(context.Background(), ImageGenerationInput{RequestID: "native-batch", ClientKey: f.key, PublicModel: "Console/grok-imagine-image-2.0", Prompt: "draw", Count: count, Quality: "low", Resolution: "1k", ResponseFormat: format})
}

func (f nativeImageFixture) checkBilling(t *testing.T, cents int64) {
	t.Helper()
	key, err := f.keys.Get(context.Background(), f.key.ID)
	if err != nil || key.BilledUsageUSDTicks != cents*100_000_000 || key.ReservedUsageUSDTicks != 0 {
		t.Fatalf("billing=%+v err=%v, want %d cents and no hold", key, err, cents)
	}
	f.service.selector.quotaMu.RLock()
	defer f.service.selector.quotaMu.RUnlock()
	if len(f.service.selector.quotaReserved) != 0 {
		t.Fatalf("quota hold leaked: %+v", f.service.selector.quotaReserved)
	}
}

func TestNativeImagesFitAccountCentsAndKeepNativeBatches(t *testing.T) {
	f := newNativeImageFixture(t, 8, 12) // 2 and 3 low/1k images, not 8 and 12 images.
	result, err := f.generate(t, 5, "url")
	if err != nil {
		t.Fatal(err)
	}
	if response := readFanned(t, result); len(response.Data) != 5 {
		t.Fatalf("outputs=%+v", response)
	}
	counts := []int{}
	for _, call := range f.adapter.calls {
		counts = append(counts, call.options.Count)
	}
	slices.Sort(counts)
	if !slices.Equal(counts, []int{2, 3}) {
		t.Fatalf("native batches=%v, want 2+3, not five repeated single calls", counts)
	}
	windows, err := f.accounts.GetQuotaWindows(context.Background(), f.ids)
	if err != nil {
		t.Fatal(err)
	}
	for _, list := range windows {
		for _, window := range list {
			if window.Remaining != 0 {
				t.Fatalf("shared cents budget not consumed: %+v", window)
			}
		}
	}
	f.checkBilling(t, 20)
}

func TestNativeImagesReturnPartialAndDoNotRegenerateMissingOutputs(t *testing.T) {
	for _, format := range []string{"url", "b64_json"} {
		t.Run(format, func(t *testing.T) {
			f := newNativeImageFixture(t, 100)
			f.adapter.partial = 2
			result, err := f.generate(t, 5, format)
			if err != nil {
				t.Fatal(err)
			}
			if response := readFanned(t, result); len(response.Data) != 2 || len(f.adapter.calls) != 1 || f.adapter.calls[0].options.Count != 5 {
				t.Fatalf("partial output was lost or replayed: %+v calls=%+v", response, f.adapter.calls)
			}
			f.checkBilling(t, 8)
		})
	}
}

func TestNativeImagesKeepSuccessWhenPoolRunsOut(t *testing.T) {
	f := newNativeImageFixture(t, 8)
	result, err := f.generate(t, 5, "url")
	if err != nil {
		t.Fatal(err)
	}
	if response := readFanned(t, result); len(response.Data) != 2 || len(f.adapter.calls) != 1 {
		t.Fatalf("quota exhaustion lost delivered images: %+v calls=%+v", response, f.adapter.calls)
	}
	f.checkBilling(t, 8)
}

func TestNativeImageEffectiveEditInputsAndPriceMatchForwardedRequest(t *testing.T) {
	f := newNativeImageFixture(t, 100)
	result, err := f.service.EditImage(context.Background(), ImageEditInput{RequestID: "edit-price", ClientKey: f.key, PublicModel: "Console/grok-imagine-image-2.0", Prompt: "merge", Count: 1, ImageURLs: []string{"a", "b", "c", "d", "e", "f", "g"}, Size: "1536x1024", Resolution: "4K", Quality: "high"})
	if err != nil {
		t.Fatal(err)
	}
	readFanned(t, result)
	call := f.adapter.calls[0]
	if !slices.Equal(call.inputs, []string{"a", "b", "c"}) || call.options.Resolution != "2k" || call.options.Quality != "medium" || call.options.AspectRatio != "3:2" {
		t.Fatalf("effective request mismatch: %+v", call)
	}
	records, _, err := f.audits.List(context.Background(), 0, 10)
	if err != nil || len(records) != 1 || records[0].MediaInputImages != 3 || records[0].MediaOutputImages != 1 || records[0].PricingModel != "grok-imagine-image-2.0-edit-medium-2k" {
		t.Fatalf("audit=%+v err=%v", records, err)
	}
	f.checkBilling(t, 11)
}

func TestNativeImageEditFitsReferenceCostPerBatch(t *testing.T) {
	f := newNativeImageFixture(t, 15, 21) // medium/1k: 6 cents/output + 3 input cents/batch.
	result, err := f.service.EditImage(context.Background(), ImageEditInput{RequestID: "edit-batches", ClientKey: f.key, PublicModel: "Console/grok-imagine-image-2.0", Prompt: "merge", Count: 5, ImageURLs: []string{"a", "b", "c", "ignored"}})
	if err != nil {
		t.Fatal(err)
	}
	if response := readFanned(t, result); len(response.Data) != 5 || len(f.adapter.calls) != 2 {
		t.Fatalf("reference processing was repeated per output or budget ignored: %+v calls=%+v", response, f.adapter.calls)
	}
	counts := []int{}
	for _, call := range f.adapter.calls {
		counts = append(counts, call.options.Count)
		if len(call.inputs) != 3 || call.options.Quality != "medium" {
			t.Fatalf("incorrect effective batch: %+v", call)
		}
	}
	slices.Sort(counts)
	if !slices.Equal(counts, []int{2, 3}) {
		t.Fatalf("edit batches=%v", counts)
	}
	f.checkBilling(t, 36)
}

func TestNativeImageUnknownQuotaUsesSingleBatches(t *testing.T) {
	f := newNativeImageFixture(t, -1)
	result, err := f.generate(t, 3, "b64_json")
	if err != nil {
		t.Fatal(err)
	}
	if response := readFanned(t, result); len(response.Data) != 3 || len(f.adapter.calls) != 3 {
		t.Fatalf("unknown budget request=%+v calls=%+v", response, f.adapter.calls)
	}
	for _, call := range f.adapter.calls {
		if call.options.Count != 1 {
			t.Fatalf("blind batch against an unknown allowance: %+v", call)
		}
	}
	f.checkBilling(t, 12)
}

func TestNativeImageBatchReservationsFitTheClientLimit(t *testing.T) {
	f := newNativeImageFixture(t, 8, 12)
	f.key.BillingLimitUSDTicks = 20 * 100_000_000
	var err error
	f.key, err = f.keys.Update(context.Background(), f.key)
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.generate(t, 5, "b64_json")
	if err != nil {
		t.Fatal(err)
	}
	if response := readFanned(t, result); len(response.Data) != 5 {
		t.Fatalf("reserved the whole request again for each native batch: %+v", response)
	}
	f.checkBilling(t, 20)
}

func TestNativeImageBatchesCloseUpstreamBeforeReusingResources(t *testing.T) {
	f := newNativeImageFixture(t, -1)
	f.adapter.before = func() {
		f.adapter.mu.Lock()
		defer f.adapter.mu.Unlock()
		for _, body := range f.adapter.bodies {
			if !body.closed.Load() {
				t.Error("previous batch still owns its upstream response/egress lease")
			}
		}
	}
	result, err := f.generate(t, 3, "b64_json")
	if err != nil {
		t.Fatal(err)
	}
	key, err := f.keys.Get(context.Background(), f.key.ID)
	if err != nil || key.BilledUsageUSDTicks != 0 || key.ReservedUsageUSDTicks != 12*100_000_000 {
		t.Fatalf("client fees settled before response delivery: %+v err=%v", key, err)
	}
	readFanned(t, result)
	f.checkBilling(t, 12)
}

func TestNativeImageTransferFailureSpendsQuotaButNotClientFees(t *testing.T) {
	f := newNativeImageFixture(t, 8)
	result, err := f.generate(t, 2, "url")
	if err != nil {
		t.Fatal(err)
	}
	// The batch was fully read upstream; the caller disconnects before receiving it.
	result.Finalize(Usage{}, "", "stream_interrupted")
	_ = result.Body.Close()
	f.checkBilling(t, 0)
	windows, err := f.accounts.GetQuotaWindows(context.Background(), f.ids)
	if err != nil {
		t.Fatal(err)
	}
	for _, window := range windows[f.ids[0]] {
		if window.Remaining != 0 {
			t.Fatalf("upstream spend was refunded after a client disconnect: %+v", window)
		}
	}
}

func TestNativeImageUnavailableBudgetsDoNotSubmitAndReleaseHolds(t *testing.T) {
	for _, balance := range []int{0, 3} { // One low/1k image costs 4 cents.
		f := newNativeImageFixture(t, balance, balance)
		result, err := f.generate(t, 3, "url")
		var exhausted *SelectionUnavailableError
		if result != nil || !errors.As(err, &exhausted) || exhausted.Reason != SelectionQuotaExhausted || len(f.adapter.calls) != 0 {
			t.Fatalf("balance=%d result=%v error=%v calls=%+v", balance, result, err, f.adapter.calls)
		}
		f.checkBilling(t, 0)
	}
	f := newNativeImageFixture(t, 8)
	f.adapter.failure = errors.New("test transport failure")
	if result, err := f.generate(t, 2, "url"); result != nil || err == nil {
		t.Fatalf("result=%v err=%v", result, err)
	}
	f.checkBilling(t, 0)
}

func TestNativeImageQuotaHoldPreventsConcurrentOversubscription(t *testing.T) {
	f := newNativeImageFixture(t, 8)
	started, finish := make(chan struct{}), make(chan struct{})
	f.adapter.before = func() { close(started); <-finish }
	done := make(chan *Result, 1)
	errors := make(chan error, 1)
	go func() {
		result, err := f.generate(t, 2, "url")
		if err != nil {
			errors <- err
			return
		}
		done <- result
	}()
	select {
	case <-started:
	case err := <-errors:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("batch never reached the upstream")
	}
	_, err := f.generate(t, 1, "url")
	close(finish)
	if err == nil {
		t.Fatal("the same cents budget was promised to two in-flight requests")
	}
	select {
	case result := <-done:
		if response := readFanned(t, result); len(response.Data) != 2 {
			t.Fatalf("held batch lost images: %+v", response)
		}
	case err := <-errors:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("batch did not finish")
	}
	f.checkBilling(t, 8)
}
