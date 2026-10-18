// Package pricing holds administrator custom prices. Official xAI rates stay
// code-defined in the audit package; a custom price overrides them per
// provider, pricing model and operation.
package pricing

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
)

// Operation is the billing shape a price applies to.
type Operation string

const (
	OperationText      Operation = "text"
	OperationImage     Operation = "image"
	OperationImageEdit Operation = "image_edit"
	OperationVideo     Operation = "video"
	OperationTTS       Operation = "tts"
	OperationSTT       Operation = "stt"
)

// Component is one rate a price can carry.
type Component string

const (
	InputToken       Component = "input_token"
	CachedInputToken Component = "cached_input_token"
	OutputToken      Component = "output_token"
	InputImage       Component = "input_image"
	OutputImage      Component = "output_image"
	OutputSecond     Component = "output_second"
	Character        Component = "character"
)

// ticksPerUSD is the audit cost unit: 1 USD = 10^10 ticks.
const ticksPerUSD int64 = 10_000_000_000

// Components lists the rates an operation uses. Omitted optional rates
// default to the input token rate (cached input) or zero (video input images).
func (o Operation) Components() (required, optional []Component) {
	switch o {
	case OperationText:
		return []Component{InputToken, OutputToken}, []Component{CachedInputToken}
	case OperationImage:
		return []Component{OutputImage}, nil
	case OperationImageEdit:
		return []Component{OutputImage, InputImage}, nil
	case OperationVideo:
		return []Component{OutputSecond}, []Component{InputImage}
	case OperationTTS:
		return []Component{Character}, nil
	case OperationSTT:
		return []Component{OutputSecond}, nil
	default:
		return nil, nil
	}
}

// PerMillion reports whether API values are USD per million units.
func (c Component) PerMillion() bool {
	return c == InputToken || c == CachedInputToken || c == OutputToken || c == Character
}

// scale is the number of ticks per unit for one API USD value.
func (c Component) scale() int64 {
	if c.PerMillion() {
		return ticksPerUSD / 1_000_000
	}
	return ticksPerUSD
}

// limit keeps rate × realistic quantities inside int64: $10,000 per million
// tokens/characters and $1,000 per image or second.
func (c Component) limit() int64 {
	if c.PerMillion() {
		return 100_000_000
	}
	return 10_000_000_000_000
}

// ParseUSD converts a decimal USD string to exact ticks per unit.
func ParseUSD(c Component, value string) (int64, error) {
	rat, ok := new(big.Rat).SetString(strings.TrimSpace(value))
	if !ok || rat.Sign() < 0 {
		return 0, fmt.Errorf("%s 必须是非负十进制数", c)
	}
	rat.Mul(rat, new(big.Rat).SetInt64(c.scale()))
	if !rat.IsInt() {
		return 0, fmt.Errorf("%s 精度超过 1 tick", c)
	}
	if rat.Num().Cmp(big.NewInt(c.limit())) > 0 {
		return 0, fmt.Errorf("%s 超出上限", c)
	}
	return rat.Num().Int64(), nil
}

// FormatUSD renders ticks per unit as the shortest exact decimal USD string.
func FormatUSD(c Component, ticks int64) string {
	digits := 10
	if c.PerMillion() {
		digits = 4
	}
	value := new(big.Rat).SetFrac64(ticks, c.scale()).FloatString(digits)
	return strings.TrimRight(strings.TrimRight(value, "0"), ".")
}

// Rates are USD ticks per token, image, second or character.
type Rates struct {
	InputToken       int64
	CachedInputToken int64
	OutputToken      int64
	InputImage       int64
	OutputImage      int64
	OutputSecond     int64
	Character        int64
}

// Rate returns a pointer to the field holding one component.
func (r *Rates) Rate(c Component) *int64 {
	switch c {
	case InputToken:
		return &r.InputToken
	case CachedInputToken:
		return &r.CachedInputToken
	case OutputToken:
		return &r.OutputToken
	case InputImage:
		return &r.InputImage
	case OutputImage:
		return &r.OutputImage
	case OutputSecond:
		return &r.OutputSecond
	case Character:
		return &r.Character
	default:
		return nil
	}
}

// Price is one immutable administrator price version; ID is the version.
type Price struct {
	ID           uint64
	Provider     string
	PricingModel string
	Operation    Operation
	Rates
	CreatedAt    time.Time
	SupersededAt *time.Time
}

var ErrUnknownOperation = errors.New("未知的计价操作")

// NewPrice validates the operation's components and fills optional defaults.
func NewPrice(provider, pricingModel string, operation Operation, values map[Component]string) (Price, error) {
	required, optional := operation.Components()
	if len(required) == 0 {
		return Price{}, ErrUnknownOperation
	}
	price := Price{Provider: provider, PricingModel: pricingModel, Operation: operation}
	allowed := map[Component]bool{}
	for _, c := range append(append([]Component{}, required...), optional...) {
		allowed[c] = true
	}
	for c, value := range values {
		if !allowed[c] {
			return Price{}, fmt.Errorf("%s 不适用于 %s", c, operation)
		}
		ticks, err := ParseUSD(c, value)
		if err != nil {
			return Price{}, err
		}
		*price.Rate(c) = ticks
	}
	for _, c := range required {
		if _, ok := values[c]; !ok {
			return Price{}, fmt.Errorf("%s 缺少 %s", operation, c)
		}
	}
	if _, ok := values[CachedInputToken]; operation == OperationText && !ok {
		price.CachedInputToken = price.InputToken
	}
	return price, nil
}

// Breakdown applies the rates to the quantities an audit record stores.
// TTS and STT quantities are not stored, so they cannot be reconstructed.
func (p Price) Breakdown(inputTokens, cachedInputTokens, outputTokens, inputImages, outputImages, outputSeconds int64) (audit.PricingBreakdown, bool) {
	switch p.Operation {
	case OperationText:
		cached := max(int64(0), min(cachedInputTokens, inputTokens))
		return audit.NewPricingBreakdown(p.PricingModel, audit.PricingTierStandard,
			audit.NewPricingComponent(audit.PricingComponentUncachedInput, audit.PricingUnitToken, max(int64(0), inputTokens-cached), p.InputToken),
			audit.NewPricingComponent(audit.PricingComponentOutput, audit.PricingUnitToken, outputTokens, p.OutputToken),
			audit.NewPricingComponent(audit.PricingComponentCachedInput, audit.PricingUnitToken, cached, p.CachedInputToken),
		), true
	case OperationImage:
		if outputImages <= 0 {
			return audit.PricingBreakdown{}, false
		}
		return audit.NewPricingBreakdown(p.PricingModel, audit.PricingTierMedia,
			audit.NewPricingComponent(audit.PricingComponentOutputImage, audit.PricingUnitImage, outputImages, p.OutputImage),
		), true
	case OperationImageEdit:
		if outputImages <= 0 || inputImages < 0 {
			return audit.PricingBreakdown{}, false
		}
		return audit.NewPricingBreakdown(p.PricingModel, audit.PricingTierMedia,
			audit.NewPricingComponent(audit.PricingComponentOutputImage, audit.PricingUnitImage, outputImages, p.OutputImage),
			audit.NewPricingComponent(audit.PricingComponentInputImage, audit.PricingUnitImage, inputImages, p.InputImage),
		), true
	case OperationVideo:
		if outputSeconds <= 0 || inputImages < 0 {
			return audit.PricingBreakdown{}, false
		}
		return audit.NewPricingBreakdown(p.PricingModel, audit.PricingTierMedia,
			audit.NewPricingComponent(audit.PricingComponentOutputSecond, audit.PricingUnitSecond, outputSeconds, p.OutputSecond),
			audit.NewPricingComponent(audit.PricingComponentInputImage, audit.PricingUnitImage, inputImages, p.InputImage),
		), true
	default:
		return audit.PricingBreakdown{}, false
	}
}

// Quote turns a breakdown of this price into a billing estimate.
func (p Price) Quote(breakdown audit.PricingBreakdown, ok bool) (audit.PricingResult, bool) {
	if !ok {
		return audit.PricingResult{}, false
	}
	return audit.PricingResult{Model: p.PricingModel, CostInUSDTicks: breakdown.CostInUSDTicks, PriceID: p.ID}, true
}

// TTSCost bills the exact Unicode character count of the request text.
func (p Price) TTSCost(text string) (audit.PricingResult, bool) {
	characters := int64(utf8.RuneCountInString(text))
	if characters <= 0 {
		return audit.PricingResult{}, false
	}
	return audit.PricingResult{Model: p.PricingModel, CostInUSDTicks: characters * p.Character, PriceID: p.ID}, true
}

// STTCost bills audio seconds, rounding a positive charge up to one tick.
func (p Price) STTCost(seconds float64) (audit.PricingResult, bool) {
	if seconds <= 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return audit.PricingResult{}, false
	}
	cost := int64(math.Ceil(seconds * float64(p.OutputSecond)))
	if p.OutputSecond > 0 {
		cost = max(1, cost)
	}
	return audit.PricingResult{Model: p.PricingModel, CostInUSDTicks: cost, PriceID: p.ID}, true
}
