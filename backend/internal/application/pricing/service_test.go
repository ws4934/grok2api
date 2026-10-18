package pricing

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	pricingdomain "github.com/chenyme/grok2api/backend/internal/domain/pricing"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

const testProvider account.Provider = "pricedgpt"

func newTestService(t *testing.T) *Service {
	t.Helper()
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "pricing.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	known := map[account.Provider]bool{testProvider: true, account.ProviderConsole: true, account.ProviderWeb: true}
	service, err := NewService(ctx, relational.NewModelPriceRepository(database), func(value account.Provider) bool { return known[value] }, func(_ account.Provider, upstream string) string {
		if upstream == "web-chat" {
			return "grok-4.5"
		}
		return upstream
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func mustPut(t *testing.T, service *Service, provider account.Provider, model string, operation pricingdomain.Operation, rates map[pricingdomain.Component]string) pricingdomain.Price {
	t.Helper()
	price, err := service.Put(context.Background(), Input{Provider: string(provider), PricingModel: model, Operation: string(operation), Rates: rates})
	if err != nil {
		t.Fatal(err)
	}
	return price
}

func TestResolverPrecedenceCustomOfficialNone(t *testing.T) {
	service := newTestService(t)
	var nilService *Service
	body := []byte(`{"model":"x","input":"hello","max_output_tokens":100}`)

	// No custom rows: identical to the official estimates, also on a nil service.
	for _, s := range []*Service{nilService, service} {
		got, ok := s.QuoteText(account.ProviderConsole, "grok-4.3", 1000, 100, 50, 1000)
		want, _ := audit.EstimateOfficialCost("grok-4.3", 1000, 100, 50, 1000)
		if !ok || got != want {
			t.Fatalf("official text = %#v, want %#v", got, want)
		}
		got, ok = s.QuoteTextReservation(account.ProviderConsole, "grok-4.3", body)
		want, _ = audit.EstimateOfficialTextReservation("grok-4.3", body)
		if !ok || got != want {
			t.Fatalf("official reservation = %#v, want %#v", got, want)
		}
		if _, ok := s.QuoteText(testProvider, "gpt-test", 1000, 0, 50, 1000); ok {
			t.Fatal("unknown model priced without a custom row")
		}
	}

	text := mustPut(t, service, testProvider, "gpt-test", pricingdomain.OperationText, map[pricingdomain.Component]string{
		pricingdomain.InputToken: "2", pricingdomain.CachedInputToken: "0.5", pricingdomain.OutputToken: "8",
	})
	got, ok := service.QuoteText(testProvider, "gpt-test", 1000, 100, 50, 1000)
	if !ok || got.PriceID != text.ID || got.Model != "gpt-test" || got.CostInUSDTicks != 900*20_000+100*5_000+50*80_000 {
		t.Fatalf("custom text = %#v", got)
	}
	inputTokens, outputTokens := audit.EstimateReservationTokens(body)
	if got, ok := service.QuoteTextReservation(testProvider, "gpt-test", body); !ok || got.PriceID != text.ID || got.CostInUSDTicks != inputTokens*20_000+outputTokens*80_000 {
		t.Fatalf("custom reservation = %#v", got)
	}

	// A custom row overrides the official rate for the same provider only.
	override := mustPut(t, service, account.ProviderConsole, "grok-4.3", pricingdomain.OperationText, map[pricingdomain.Component]string{
		pricingdomain.InputToken: "1", pricingdomain.OutputToken: "1",
	})
	if got, ok := service.QuoteText(account.ProviderConsole, "grok-4.3", 10, 0, 10, 10); !ok || got.PriceID != override.ID || got.CostInUSDTicks != 200_000 {
		t.Fatalf("override = %#v", got)
	}
	if got, _ := service.QuoteText(account.ProviderWeb, "grok-4.3", 10, 0, 10, 10); got.PriceID != 0 {
		t.Fatalf("override leaked to another provider: %#v", got)
	}

	mustPut(t, service, testProvider, "img", pricingdomain.OperationImage, map[pricingdomain.Component]string{pricingdomain.OutputImage: "0.04"})
	mustPut(t, service, testProvider, "img", pricingdomain.OperationImageEdit, map[pricingdomain.Component]string{pricingdomain.OutputImage: "0.05", pricingdomain.InputImage: "0.01"})
	mustPut(t, service, testProvider, "vid", pricingdomain.OperationVideo, map[pricingdomain.Component]string{pricingdomain.OutputSecond: "0.1"})
	mustPut(t, service, testProvider, "voice", pricingdomain.OperationTTS, map[pricingdomain.Component]string{pricingdomain.Character: "30"})
	mustPut(t, service, testProvider, "voice", pricingdomain.OperationSTT, map[pricingdomain.Component]string{pricingdomain.OutputSecond: "0.001"})
	cases := []struct {
		name   string
		quote  func(account.Provider, string) (audit.PricingResult, bool)
		model  string
		custom int64
		// official is the expected Grok result for the same call.
		official func() (audit.PricingResult, bool)
	}{
		{"image", func(p account.Provider, m string) (audit.PricingResult, bool) {
			return service.QuoteImage(p, m, "", "", 2)
		}, "img", 800_000_000,
			func() (audit.PricingResult, bool) {
				return audit.EstimateOfficialImageCost("grok-imagine-image", "", "", 2)
			}},
		{"image_edit", func(p account.Provider, m string) (audit.PricingResult, bool) {
			return service.QuoteImageEdit(p, m, "", "", 2, 3)
		}, "img", 1_300_000_000,
			func() (audit.PricingResult, bool) {
				return audit.EstimateOfficialImageEditCost("grok-imagine-image", "", "", 2, 3)
			}},
		{"video", func(p account.Provider, m string) (audit.PricingResult, bool) {
			return service.QuoteVideo(p, m, "720p", 6, 1)
		}, "vid", 6_000_000_000,
			func() (audit.PricingResult, bool) {
				return audit.EstimateOfficialVideoCost("grok-imagine-video", "720p", 6, 1)
			}},
		{"tts", func(p account.Provider, m string) (audit.PricingResult, bool) {
			return service.QuoteTTS(p, m, "héllo")
		}, "voice", 5 * 300_000,
			func() (audit.PricingResult, bool) { return audit.EstimateOfficialTTSCost("héllo") }},
		{"stt", func(p account.Provider, m string) (audit.PricingResult, bool) {
			return service.QuoteSTT(p, m, 2.5, true)
		}, "voice", 25_000_000,
			func() (audit.PricingResult, bool) { return audit.EstimateOfficialSTTCost(2.5, true) }},
	}
	for _, tc := range cases {
		got, ok := tc.quote(testProvider, tc.model)
		if !ok || got.PriceID == 0 || got.CostInUSDTicks != tc.custom || got.Model != tc.model {
			t.Fatalf("%s custom = %#v, %v", tc.name, got, ok)
		}
		officialModel := map[string]string{"img": "grok-imagine-image", "vid": "grok-imagine-video", "voice": "grok-voice"}[tc.model]
		got, ok = tc.quote(account.ProviderConsole, officialModel)
		want, wantOK := tc.official()
		if ok != wantOK || got != want || got.PriceID != 0 {
			t.Fatalf("%s official = %#v/%v, want %#v/%v", tc.name, got, ok, want, wantOK)
		}
		if tc.name != "tts" && tc.name != "stt" {
			if got, ok := tc.quote(testProvider, "unpriced"); ok || got.CostInUSDTicks != 0 {
				t.Fatalf("%s unpriced = %#v", tc.name, got)
			}
		}
	}

	// Display names with the provider namespace share the raw model's price.
	if got, _ := service.QuoteVideo(account.ProviderConsole, "Console/grok-imagine-video", "720p", 6, 0); got.PriceID != 0 {
		t.Fatalf("console video without override = %#v", got)
	}
	consoleVideo := mustPut(t, service, account.ProviderConsole, "Console/grok-imagine-video", pricingdomain.OperationVideo, map[pricingdomain.Component]string{pricingdomain.OutputSecond: "1"})
	if consoleVideo.PricingModel != "grok-imagine-video" {
		t.Fatalf("stored pricing model = %q", consoleVideo.PricingModel)
	}
	if got, _ := service.QuoteVideo(account.ProviderConsole, "Console/grok-imagine-video", "720p", 2, 0); got.PriceID != consoleVideo.ID || got.CostInUSDTicks != 20_000_000_000 {
		t.Fatalf("namespaced video = %#v", got)
	}
}

func TestPutAndDeleteAreAppendOnlyAndInvalidate(t *testing.T) {
	service := newTestService(t)
	ctx := context.Background()
	var events []repository.InvalidationEvent
	service.SetInvalidationObserver(func(_ context.Context, event repository.InvalidationEvent) { events = append(events, event) })

	first := mustPut(t, service, testProvider, "gpt-test", pricingdomain.OperationText, map[pricingdomain.Component]string{pricingdomain.InputToken: "1", pricingdomain.OutputToken: "2"})
	second := mustPut(t, service, testProvider, "gpt-test", pricingdomain.OperationText, map[pricingdomain.Component]string{pricingdomain.InputToken: "3", pricingdomain.OutputToken: "4"})
	if got, _ := service.QuoteText(testProvider, "gpt-test", 1, 0, 0, 1); got.PriceID != second.ID || got.CostInUSDTicks != 30_000 {
		t.Fatalf("quote after edit = %#v", got)
	}
	history, err := service.List(ctx, string(testProvider), "gpt-test", true)
	if err != nil || len(history) != 2 || history[1].ID != first.ID || history[1].SupersededAt == nil {
		t.Fatalf("history = %#v, %v", history, err)
	}
	if err := service.Delete(ctx, first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting a superseded version err = %v", err)
	}
	if err := service.Delete(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := service.QuoteText(testProvider, "gpt-test", 1, 0, 0, 1); ok {
		t.Fatal("deleted price still quoted")
	}
	if len(events) != 3 || events[0].Kind != repository.InvalidationPriceChanged || !events[0].Valid() {
		t.Fatalf("invalidation events = %#v", events)
	}

	// Another replica's write is seen after its invalidation event.
	other, err := NewService(ctx, service.repo, service.isProvider, service.pricingModel)
	if err != nil {
		t.Fatal(err)
	}
	remote := mustPut(t, service, testProvider, "gpt-remote", pricingdomain.OperationImage, map[pricingdomain.Component]string{pricingdomain.OutputImage: "1"})
	if _, ok := other.QuoteImage(testProvider, "gpt-remote", "", "", 1); ok {
		t.Fatal("snapshot changed before invalidation")
	}
	other.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationClientKeyChanged})
	if _, ok := other.QuoteImage(testProvider, "gpt-remote", "", "", 1); ok {
		t.Fatal("unrelated invalidation reloaded prices")
	}
	other.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationPriceChanged})
	if got, ok := other.QuoteImage(testProvider, "gpt-remote", "", "", 1); !ok || got.PriceID != remote.ID {
		t.Fatalf("remote price after invalidation = %#v", got)
	}

	for name, input := range map[string]Input{
		"unknown provider":  {Provider: "grok_build", PricingModel: "m", Operation: "text", Rates: map[pricingdomain.Component]string{pricingdomain.InputToken: "1", pricingdomain.OutputToken: "1"}},
		"empty model":       {Provider: string(testProvider), PricingModel: " ", Operation: "text", Rates: map[pricingdomain.Component]string{pricingdomain.InputToken: "1", pricingdomain.OutputToken: "1"}},
		"unknown operation": {Provider: string(testProvider), PricingModel: "m", Operation: "audio", Rates: map[pricingdomain.Component]string{pricingdomain.Character: "1"}},
		"missing component": {Provider: string(testProvider), PricingModel: "m", Operation: "image_edit", Rates: map[pricingdomain.Component]string{pricingdomain.OutputImage: "1"}},
		"negative":          {Provider: string(testProvider), PricingModel: "m", Operation: "image", Rates: map[pricingdomain.Component]string{pricingdomain.OutputImage: "-1"}},
	} {
		if _, err := service.Put(ctx, input); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

func TestRoutePriceReportsSource(t *testing.T) {
	service := newTestService(t)
	route := func(provider account.Provider, upstream string, capability modeldomain.Capability) modeldomain.Route {
		return modeldomain.Route{Provider: provider, UpstreamModel: upstream, Capability: capability}
	}
	official := service.RoutePrice(route(account.ProviderWeb, "web-chat", modeldomain.CapabilityChat))
	if official.Source != SourceOfficial || official.PricingModel != "grok-4.5" || official.Price == nil ||
		official.Price.InputToken != 20_000 || official.Price.CachedInputToken != 3_000 || official.Price.OutputToken != 60_000 {
		t.Fatalf("official text route = %#v %#v", official, official.Price)
	}
	if got := service.RoutePrice(route(account.ProviderConsole, "grok-imagine-image-quality", modeldomain.CapabilityImage)); got.Source != SourceOfficial || got.Price != nil {
		t.Fatalf("official image route = %#v", got)
	}
	if got := service.RoutePrice(route(account.ProviderConsole, "grok-imagine-video", modeldomain.CapabilityVideo)); got.Source != SourceOfficial {
		t.Fatalf("official video route = %#v", got)
	}
	if got := service.RoutePrice(route(account.ProviderConsole, "grok-voice", modeldomain.CapabilityTTS)); got.Source != SourceOfficial || got.Price.Character != 150_000 {
		t.Fatalf("official tts route = %#v", got)
	}
	if got := service.RoutePrice(route(testProvider, "gpt-test", modeldomain.CapabilityResponses)); got.Source != SourceNone || got.Operation != pricingdomain.OperationText {
		t.Fatalf("unpriced route = %#v", got)
	}
	if got := service.RoutePrice(route(account.ProviderConsole, "grok-voice", modeldomain.CapabilityRealtime)); got.Source != SourceNone || got.Operation != "" {
		t.Fatalf("realtime route = %#v", got)
	}
	price := mustPut(t, service, testProvider, "gpt-test", pricingdomain.OperationText, map[pricingdomain.Component]string{pricingdomain.InputToken: "1", pricingdomain.OutputToken: "2"})
	if got := service.RoutePrice(route(testProvider, "gpt-test", modeldomain.CapabilityChat)); got.Source != SourceCustom || got.Price.ID != price.ID {
		t.Fatalf("custom route = %#v", got)
	}
}
