package dashboard

import (
	"errors"
	"net/http"
	"time"

	dashboardapp "github.com/chenyme/grok2api/backend/internal/application/dashboard"
	dashboarddomain "github.com/chenyme/grok2api/backend/internal/domain/dashboard"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

type Handler struct{ service *dashboardapp.Service }

func NewHandler(service *dashboardapp.Service) *Handler { return &Handler{service: service} }

func (h *Handler) Register(router *gin.RouterGroup) { router.GET("/dashboard", h.get) }

type responseDTO struct {
	Period      string             `json:"period"`
	GeneratedAt time.Time          `json:"generatedAt"`
	Range       rangeDTO           `json:"range"`
	Resources   resourcesDTO       `json:"resources"`
	Usage       usageDTO           `json:"usage"`
	Series      []seriesDTO        `json:"series"`
	Activity    []activityDTO      `json:"activity"`
	TopModels   []modelUsageDTO    `json:"topModels"`
	Providers   []providerUsageDTO `json:"providers"`
	MediaQuota  []mediaQuotaDTO    `json:"mediaQuota"`
}

type rangeDTO struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

type resourcesDTO struct {
	ActiveAccounts     int64            `json:"activeAccounts"`
	TotalAccounts      int64            `json:"totalAccounts"`
	AccountsByProvider map[string]int64 `json:"accountsByProvider"`
	EnabledModels      int64            `json:"enabledModels"`
	TotalModels        int64            `json:"totalModels"`
	ActiveClientKeys   int64            `json:"activeClientKeys"`
	TotalClientKeys    int64            `json:"totalClientKeys"`
}

type usageDTO struct {
	Requests              int64   `json:"requests"`
	SuccessfulRequests    int64   `json:"successfulRequests"`
	FailedRequests        int64   `json:"failedRequests"`
	InputTokens           int64   `json:"inputTokens"`
	CachedInputTokens     int64   `json:"cachedInputTokens"`
	OutputTokens          int64   `json:"outputTokens"`
	ReasoningTokens       int64   `json:"reasoningTokens"`
	Tokens                int64   `json:"tokens"`
	BilledCostUSDTicks    int64   `json:"billedCostUsdTicks"`
	SuccessRate           float64 `json:"successRate"`
	AverageFirstTokenMS   float64 `json:"averageFirstTokenMs"`
	OutputTokensPerSecond float64 `json:"outputTokensPerSecond"`
	FirstTokenSamples     int64   `json:"firstTokenSamples"`
	ThroughputSamples     int64   `json:"throughputSamples"`
}

type seriesDTO struct {
	Start              time.Time `json:"start"`
	End                time.Time `json:"end"`
	Requests           int64     `json:"requests"`
	InputTokens        int64     `json:"inputTokens"`
	CachedInputTokens  int64     `json:"cachedInputTokens"`
	OutputTokens       int64     `json:"outputTokens"`
	ReasoningTokens    int64     `json:"reasoningTokens"`
	Tokens             int64     `json:"tokens"`
	BilledCostUSDTicks int64     `json:"billedCostUsdTicks"`
}

type activityDTO struct {
	Start    time.Time `json:"start"`
	Requests int64     `json:"requests"`
}

type providerUsageDTO struct {
	Provider           string `json:"provider"`
	Requests           int64  `json:"requests"`
	SuccessfulRequests int64  `json:"successfulRequests"`
	Tokens             int64  `json:"tokens"`
}

// mediaQuotaDTO is one provider's image, video or credits quota summed over
// its usable accounts.
type mediaQuotaDTO struct {
	Provider  string `json:"provider"`
	Kind      string `json:"kind"`
	Remaining int64  `json:"remaining"`
	Total     int64  `json:"total"`
	Accounts  int64  `json:"accounts"`
}

type modelUsageDTO struct {
	Model              string `json:"model"`
	Requests           int64  `json:"requests"`
	InputTokens        int64  `json:"inputTokens"`
	CachedInputTokens  int64  `json:"cachedInputTokens"`
	OutputTokens       int64  `json:"outputTokens"`
	ReasoningTokens    int64  `json:"reasoningTokens"`
	Tokens             int64  `json:"tokens"`
	BilledCostUSDTicks int64  `json:"billedCostUsdTicks"`
}

func (h *Handler) get(c *gin.Context) {
	load := h.service.Get
	if c.Query("refresh") == "1" {
		load = h.service.Refresh
	}
	result, err := load(c.Request.Context(), c.Query("period"), c.Query("timezone"))
	if errors.Is(err, dashboardapp.ErrInvalidPeriod) {
		response.Error(c, http.StatusBadRequest, "invalidDashboardPeriod", "period 仅支持 24h、7d、30d、90d")
		return
	}
	if errors.Is(err, dashboardapp.ErrInvalidTimezone) {
		response.Error(c, http.StatusBadRequest, "invalidDashboardTimezone", "timezone 必须是有效的 IANA 时区")
		return
	}
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "dashboardLoadFailed", "读取 Dashboard 失败")
		return
	}
	series := make([]seriesDTO, 0, len(result.Series))
	for _, point := range result.Series {
		series = append(series, seriesDTO{Start: point.Start, End: point.End, Requests: point.Requests, InputTokens: point.InputTokens, CachedInputTokens: point.CachedInputTokens, OutputTokens: point.OutputTokens, ReasoningTokens: point.ReasoningTokens, Tokens: point.Tokens, BilledCostUSDTicks: point.BilledCostUSDTicks})
	}
	activity := make([]activityDTO, 0, len(result.Activity))
	for _, point := range result.Activity {
		activity = append(activity, activityDTO{Start: point.Start, Requests: point.Requests})
	}
	topModels := make([]modelUsageDTO, 0, len(result.TopModels))
	for _, item := range result.TopModels {
		topModels = append(topModels, modelUsageDTO{Model: item.Model, Requests: item.Requests, InputTokens: item.InputTokens, CachedInputTokens: item.CachedInputTokens, OutputTokens: item.OutputTokens, ReasoningTokens: item.ReasoningTokens, Tokens: item.Tokens, BilledCostUSDTicks: item.BilledCostUSDTicks})
	}
	providers := make([]providerUsageDTO, 0, len(result.Providers))
	for _, item := range result.Providers {
		providers = append(providers, providerUsageDTO{Provider: item.Provider, Requests: item.Requests, SuccessfulRequests: item.SuccessfulRequests, Tokens: item.Tokens})
	}
	mediaQuota := make([]mediaQuotaDTO, 0, len(result.MediaQuota))
	for _, item := range result.MediaQuota {
		mediaQuota = append(mediaQuota, mediaQuotaDTO{Provider: item.Provider, Kind: item.Kind, Remaining: item.Remaining, Total: item.Total, Accounts: item.Accounts})
	}
	response.Success(c, http.StatusOK, responseDTO{
		Period:      string(result.Period),
		GeneratedAt: result.GeneratedAt,
		Range:       rangeDTO{Start: result.Range.Start, End: result.Range.End},
		Resources: resourcesDTO{
			ActiveAccounts:     result.Resources.ActiveAccounts,
			TotalAccounts:      result.Resources.TotalAccounts,
			AccountsByProvider: nonNilCounts(result.Resources.AccountsByProvider),
			EnabledModels:      result.Resources.EnabledModels,
			TotalModels:        result.Resources.TotalModels,
			ActiveClientKeys:   result.Resources.ActiveClientKeys,
			TotalClientKeys:    result.Resources.TotalClientKeys,
		},
		Usage:      toUsageDTO(result.Usage),
		Series:     series,
		Activity:   activity,
		TopModels:  topModels,
		Providers:  providers,
		MediaQuota: mediaQuota,
	})
}

func toUsageDTO(usage dashboarddomain.Usage) usageDTO {
	successRate := 0.0
	if usage.Requests > 0 {
		successRate = float64(usage.SuccessfulRequests) / float64(usage.Requests) * 100
	}
	return usageDTO{
		Requests:              usage.Requests,
		SuccessfulRequests:    usage.SuccessfulRequests,
		FailedRequests:        usage.FailedRequests,
		InputTokens:           usage.InputTokens,
		CachedInputTokens:     usage.CachedInputTokens,
		OutputTokens:          usage.OutputTokens,
		ReasoningTokens:       usage.ReasoningTokens,
		Tokens:                usage.Tokens,
		BilledCostUSDTicks:    usage.BilledCostUSDTicks,
		SuccessRate:           successRate,
		AverageFirstTokenMS:   averageFirstTokenMS(usage),
		OutputTokensPerSecond: outputTokensPerSecond(usage),
		FirstTokenSamples:     usage.FirstTokenSamples,
		ThroughputSamples:     usage.ThroughputSamples,
	}
}

func averageFirstTokenMS(usage dashboarddomain.Usage) float64 {
	if usage.FirstTokenSamples <= 0 {
		return 0
	}
	return float64(usage.FirstTokenTotalMS) / float64(usage.FirstTokenSamples)
}

func outputTokensPerSecond(usage dashboarddomain.Usage) float64 {
	if usage.ThroughputTokens <= 0 || usage.GenerationTotalMS <= 0 {
		return 0
	}
	return float64(usage.ThroughputTokens) * 1000 / float64(usage.GenerationTotalMS)
}

// nonNilCounts keeps the JSON field an object even when no account exists.
func nonNilCounts(values map[string]int64) map[string]int64 {
	if values == nil {
		return map[string]int64{}
	}
	return values
}
