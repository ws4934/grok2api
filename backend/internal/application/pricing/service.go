// Package pricing resolves the price of every request: an active
// administrator price first, then official xAI rates, else unpriced.
package pricing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	pricingdomain "github.com/chenyme/grok2api/backend/internal/domain/pricing"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

const (
	// cacheTTL bounds staleness when a remote invalidation is missed.
	cacheTTL      = time.Minute
	reloadTimeout = 3 * time.Second
)

var (
	ErrInvalidInput = errors.New("模型价格参数无效")
	ErrNotFound     = errors.New("模型价格不存在或已失效")
	ErrConflict     = errors.New("模型价格已被并发修改，请重试")
)

type priceKey struct {
	provider  string
	model     string
	operation pricingdomain.Operation
}

// Service owns the price book. A nil *Service quotes official rates only.
type Service struct {
	repo         repository.ModelPriceRepository
	isProvider   func(account.Provider) bool
	pricingModel func(account.Provider, string) string
	notify       func(context.Context, repository.InvalidationEvent)

	// ponytail: one mutex guards the snapshot and its reload (a small query once
	// per TTL or edit); switch to an atomic snapshot if quoting contends.
	mu       sync.Mutex
	active   map[priceKey]pricingdomain.Price
	loadedAt time.Time
}

// NewService loads the active prices so the first request already sees them.
// isProvider validates admin input; pricingModel maps a route's upstream model
// to its billing model (provider.Registry.PricingModel).
func NewService(ctx context.Context, repo repository.ModelPriceRepository, isProvider func(account.Provider) bool, pricingModel func(account.Provider, string) string) (*Service, error) {
	s := &Service{repo: repo, isProvider: isProvider, pricingModel: pricingModel}
	values, err := repo.List(ctx, repository.ModelPriceQuery{})
	if err != nil {
		return nil, fmt.Errorf("加载模型价格: %w", err)
	}
	s.store(values)
	return s, nil
}

// SetInvalidationObserver publishes admin changes to the other replicas.
func (s *Service) SetInvalidationObserver(notify func(context.Context, repository.InvalidationEvent)) {
	s.notify = notify
}

// ApplyInvalidation drops the snapshot after a local or remote price change.
func (s *Service) ApplyInvalidation(event repository.InvalidationEvent) {
	if s == nil || event.Kind != repository.InvalidationPriceChanged {
		return
	}
	s.mu.Lock()
	s.loadedAt = time.Time{}
	s.mu.Unlock()
}

func (s *Service) store(values []pricingdomain.Price) {
	active := make(map[priceKey]pricingdomain.Price, len(values))
	for _, value := range values {
		active[priceKey{value.Provider, value.PricingModel, value.Operation}] = value
	}
	s.active, s.loadedAt = active, time.Now()
}

// custom returns the active administrator price for a quote, if any.
func (s *Service) custom(provider account.Provider, model string, operation pricingdomain.Operation) (pricingdomain.Price, bool) {
	if s == nil {
		return pricingdomain.Price{}, false
	}
	key := priceKey{string(provider), normalizeModel(provider, model), operation}
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.loadedAt) >= cacheTTL {
		ctx, cancel := context.WithTimeout(context.Background(), reloadTimeout)
		values, err := s.repo.List(ctx, repository.ModelPriceQuery{})
		cancel()
		if err != nil {
			// Keep billing on the last snapshot and retry after the next TTL.
			s.loadedAt = time.Now()
			slog.Warn("model_price_reload_failed", "error", err)
		} else {
			s.store(values)
		}
	}
	price, ok := s.active[key]
	return price, ok
}

// normalizeModel drops the provider's own namespace so display upstream names
// ("Console/x") and raw names ("x") share one price.
func normalizeModel(provider account.Provider, model string) string {
	if value, ok := modeldomain.NormalizeUpstreamModel(provider, model); ok {
		return value
	}
	return strings.TrimSpace(model)
}

// The Quote functions mirror audit.EstimateOfficial*; model is what the call
// site passed to the official estimate before custom prices existed.

func (s *Service) QuoteText(provider account.Provider, model string, inputTokens, cachedInputTokens, outputTokens, contextInputTokens int64) (audit.PricingResult, bool) {
	if price, ok := s.custom(provider, model, pricingdomain.OperationText); ok {
		return price.Quote(price.Breakdown(inputTokens, cachedInputTokens, outputTokens, 0, 0, 0))
	}
	return audit.EstimateOfficialCost(model, inputTokens, cachedInputTokens, outputTokens, contextInputTokens)
}

// QuoteTextReservation prices the conservative pre-request token estimate.
func (s *Service) QuoteTextReservation(provider account.Provider, model string, body []byte) (audit.PricingResult, bool) {
	if price, ok := s.custom(provider, model, pricingdomain.OperationText); ok {
		inputTokens, outputTokens := audit.EstimateReservationTokens(body)
		return price.Quote(price.Breakdown(inputTokens, 0, outputTokens, 0, 0, 0))
	}
	return audit.EstimateOfficialTextReservation(model, body)
}

func (s *Service) QuoteImage(provider account.Provider, model, resolution, quality string, count int) (audit.PricingResult, bool) {
	if price, ok := s.custom(provider, model, pricingdomain.OperationImage); ok {
		return price.Quote(price.Breakdown(0, 0, 0, 0, int64(count), 0))
	}
	return audit.EstimateOfficialImageCost(model, resolution, quality, count)
}

func (s *Service) QuoteImageEdit(provider account.Provider, model, resolution, quality string, outputCount, inputCount int) (audit.PricingResult, bool) {
	if price, ok := s.custom(provider, model, pricingdomain.OperationImageEdit); ok {
		return price.Quote(price.Breakdown(0, 0, 0, int64(inputCount), int64(outputCount), 0))
	}
	return audit.EstimateOfficialImageEditCost(model, resolution, quality, outputCount, inputCount)
}

func (s *Service) QuoteVideo(provider account.Provider, model, resolution string, seconds, inputImages int) (audit.PricingResult, bool) {
	if price, ok := s.custom(provider, model, pricingdomain.OperationVideo); ok {
		return price.Quote(price.Breakdown(0, 0, 0, int64(inputImages), 0, int64(seconds)))
	}
	return audit.EstimateOfficialVideoCost(model, resolution, seconds, inputImages)
}

func (s *Service) QuoteTTS(provider account.Provider, model, text string) (audit.PricingResult, bool) {
	if price, ok := s.custom(provider, model, pricingdomain.OperationTTS); ok {
		return price.TTSCost(text)
	}
	return audit.EstimateOfficialTTSCost(text)
}

func (s *Service) QuoteSTT(provider account.Provider, model string, durationSeconds float64, streaming bool) (audit.PricingResult, bool) {
	if price, ok := s.custom(provider, model, pricingdomain.OperationSTT); ok {
		return price.STTCost(durationSeconds)
	}
	return audit.EstimateOfficialSTTCost(durationSeconds, streaming)
}

// Input is one admin price edit; Rates holds decimal USD strings (per
// million tokens/characters, per image or second).
type Input struct {
	Provider     string
	PricingModel string
	Operation    string
	Rates        map[pricingdomain.Component]string
}

// List returns active prices, or every version with includeHistory.
func (s *Service) List(ctx context.Context, provider, model string, includeHistory bool) ([]pricingdomain.Price, error) {
	provider = strings.TrimSpace(provider)
	if model = strings.TrimSpace(model); model != "" && provider != "" {
		model = normalizeModel(account.Provider(provider), model)
	}
	return s.repo.List(ctx, repository.ModelPriceQuery{Provider: provider, PricingModel: model, IncludeHistory: includeHistory})
}

// Lookup loads price rows by ID, including superseded versions.
func (s *Service) Lookup(ctx context.Context, ids []uint64) (map[uint64]pricingdomain.Price, error) {
	values, err := s.repo.ListByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	result := make(map[uint64]pricingdomain.Price, len(values))
	for _, value := range values {
		result[value.ID] = value
	}
	return result, nil
}

// Put appends a new active version and supersedes the previous one.
func (s *Service) Put(ctx context.Context, input Input) (pricingdomain.Price, error) {
	provider := account.Provider(strings.TrimSpace(input.Provider))
	if !provider.IsValid() || s.isProvider == nil || !s.isProvider(provider) {
		return pricingdomain.Price{}, fmt.Errorf("%w: 未知 Provider %q", ErrInvalidInput, input.Provider)
	}
	model := normalizeModel(provider, input.PricingModel)
	if model == "" || len([]rune(model)) > modeldomain.MaxUpstreamModelLength {
		return pricingdomain.Price{}, fmt.Errorf("%w: pricingModel 不能为空且不超过 %d 字符", ErrInvalidInput, modeldomain.MaxUpstreamModelLength)
	}
	price, err := pricingdomain.NewPrice(string(provider), model, pricingdomain.Operation(strings.TrimSpace(input.Operation)), input.Rates)
	if err != nil {
		return pricingdomain.Price{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	saved, err := s.repo.Replace(ctx, price, time.Now().UTC())
	if errors.Is(err, repository.ErrConflict) {
		return pricingdomain.Price{}, ErrConflict
	}
	if err != nil {
		return pricingdomain.Price{}, err
	}
	s.changed(ctx)
	return saved, nil
}

// Delete supersedes an active price; quotes fall back to official or unpriced.
func (s *Service) Delete(ctx context.Context, id uint64) error {
	err := s.repo.Supersede(ctx, id, time.Now().UTC())
	if errors.Is(err, repository.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	s.changed(ctx)
	return nil
}

func (s *Service) changed(ctx context.Context) {
	event := repository.InvalidationEvent{Kind: repository.InvalidationPriceChanged}
	s.ApplyInvalidation(event)
	if s.notify != nil {
		s.notify(ctx, event)
	}
}

// Price sources shown on admin model routes.
const (
	SourceOfficial = "official"
	SourceCustom   = "custom"
	SourceNone     = "none"
)

// RoutePrice is the price the gateway applies to one model route.
type RoutePrice struct {
	Source       string
	PricingModel string
	Operation    pricingdomain.Operation
	// Price is the custom row, or official rates (ID 0) where they do not
	// depend on request options (text tokens, TTS characters); nil otherwise.
	Price *pricingdomain.Price
}

// RoutePrice resolves a route the way the gateway quotes it.
func (s *Service) RoutePrice(route modeldomain.Route) RoutePrice {
	operation, ok := routeOperation(route.Capability)
	if !ok {
		return RoutePrice{Source: SourceNone}
	}
	// Video quotes use the upstream model; every other path uses the
	// provider's billing model.
	model := route.UpstreamModel
	if operation != pricingdomain.OperationVideo && s.pricingModel != nil {
		model = s.pricingModel(route.Provider, route.UpstreamModel)
	}
	result := RoutePrice{Source: SourceNone, PricingModel: normalizeModel(route.Provider, model), Operation: operation}
	if price, ok := s.custom(route.Provider, model, operation); ok {
		result.Source, result.Price = SourceCustom, &price
		return result
	}
	official := pricingdomain.Price{Provider: string(route.Provider), PricingModel: result.PricingModel, Operation: operation}
	switch operation {
	case pricingdomain.OperationText:
		// One token of each kind at standard context reads the per-token rates.
		input, priced := audit.EstimateOfficialCost(model, 1, 0, 0, 1)
		cached, _ := audit.EstimateOfficialCost(model, 1, 1, 0, 1)
		output, _ := audit.EstimateOfficialCost(model, 0, 0, 1, 0)
		if priced {
			official.InputToken, official.CachedInputToken, official.OutputToken = input.CostInUSDTicks, cached.CostInUSDTicks, output.CostInUSDTicks
			result.Source, result.Price = SourceOfficial, &official
		}
	case pricingdomain.OperationTTS:
		character, _ := audit.EstimateOfficialTTSCost("a")
		official.Character = character.CostInUSDTicks
		result.Source, result.Price = SourceOfficial, &official
	case pricingdomain.OperationImage:
		if _, ok := audit.EstimateOfficialImageCost(model, "", "", 1); ok {
			result.Source = SourceOfficial
		}
	case pricingdomain.OperationImageEdit:
		if _, ok := audit.EstimateOfficialImageEditCost(model, "", "", 1, 1); ok {
			result.Source = SourceOfficial
		}
	case pricingdomain.OperationVideo:
		if _, ok := audit.EstimateOfficialVideoCost(model, "480p", 1, 0); ok {
			result.Source = SourceOfficial
		}
	case pricingdomain.OperationSTT:
		result.Source = SourceOfficial
	}
	return result
}

func routeOperation(capability modeldomain.Capability) (pricingdomain.Operation, bool) {
	switch capability {
	case modeldomain.CapabilityResponses, modeldomain.CapabilityChat:
		return pricingdomain.OperationText, true
	case modeldomain.CapabilityImage:
		return pricingdomain.OperationImage, true
	case modeldomain.CapabilityImageEdit:
		return pricingdomain.OperationImageEdit, true
	case modeldomain.CapabilityVideo:
		return pricingdomain.OperationVideo, true
	case modeldomain.CapabilityTTS:
		return pricingdomain.OperationTTS, true
	case modeldomain.CapabilitySTT:
		return pricingdomain.OperationSTT, true
	default:
		return "", false
	}
}
