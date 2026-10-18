package pricing

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	pricingapp "github.com/chenyme/grok2api/backend/internal/application/pricing"
	pricingdomain "github.com/chenyme/grok2api/backend/internal/domain/pricing"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

type Handler struct{ service *pricingapp.Service }

func NewHandler(service *pricingapp.Service) *Handler { return &Handler{service: service} }

func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/model-prices", h.list)
	router.PUT("/model-prices", h.put)
	router.DELETE("/model-prices/:id", h.delete)
}

// usdValue accepts a decimal string or a JSON number without float rounding.
type usdValue string

func (v *usdValue) UnmarshalJSON(data []byte) error {
	if strings.HasPrefix(strings.TrimSpace(string(data)), `"`) {
		return json.Unmarshal(data, (*string)(v))
	}
	*v = usdValue(data)
	return nil
}

type putRequest struct {
	Provider                      string    `json:"provider"`
	PricingModel                  string    `json:"pricingModel"`
	Operation                     string    `json:"operation"`
	InputTokenUSDPerMillion       *usdValue `json:"inputTokenUSDPerMillion"`
	CachedInputTokenUSDPerMillion *usdValue `json:"cachedInputTokenUSDPerMillion"`
	OutputTokenUSDPerMillion      *usdValue `json:"outputTokenUSDPerMillion"`
	InputImageUSD                 *usdValue `json:"inputImageUSD"`
	OutputImageUSD                *usdValue `json:"outputImageUSD"`
	OutputSecondUSD               *usdValue `json:"outputSecondUSD"`
	CharacterUSDPerMillion        *usdValue `json:"characterUSDPerMillion"`
}

// Components are decimal USD strings: per million tokens/characters, per
// image or per second. Only the operation's components are present.
type Components struct {
	InputTokenUSDPerMillion       string `json:"inputTokenUSDPerMillion,omitempty"`
	CachedInputTokenUSDPerMillion string `json:"cachedInputTokenUSDPerMillion,omitempty"`
	OutputTokenUSDPerMillion      string `json:"outputTokenUSDPerMillion,omitempty"`
	InputImageUSD                 string `json:"inputImageUSD,omitempty"`
	OutputImageUSD                string `json:"outputImageUSD,omitempty"`
	OutputSecondUSD               string `json:"outputSecondUSD,omitempty"`
	CharacterUSDPerMillion        string `json:"characterUSDPerMillion,omitempty"`
}

func (c *Components) field(component pricingdomain.Component) *string {
	switch component {
	case pricingdomain.InputToken:
		return &c.InputTokenUSDPerMillion
	case pricingdomain.CachedInputToken:
		return &c.CachedInputTokenUSDPerMillion
	case pricingdomain.OutputToken:
		return &c.OutputTokenUSDPerMillion
	case pricingdomain.InputImage:
		return &c.InputImageUSD
	case pricingdomain.OutputImage:
		return &c.OutputImageUSD
	case pricingdomain.OutputSecond:
		return &c.OutputSecondUSD
	default:
		return &c.CharacterUSDPerMillion
	}
}

func newComponents(price pricingdomain.Price) Components {
	var result Components
	required, optional := price.Operation.Components()
	for _, component := range append(required, optional...) {
		*result.field(component) = pricingdomain.FormatUSD(component, *price.Rate(component))
	}
	return result
}

type priceResponse struct {
	ID           uint64 `json:"id,string"`
	Provider     string `json:"provider"`
	PricingModel string `json:"pricingModel"`
	Operation    string `json:"operation"`
	Components
	Active       bool       `json:"active"`
	CreatedAt    time.Time  `json:"createdAt"`
	SupersededAt *time.Time `json:"supersededAt,omitempty"`
}

func newPriceResponse(value pricingdomain.Price) priceResponse {
	return priceResponse{
		ID: value.ID, Provider: value.Provider, PricingModel: value.PricingModel, Operation: string(value.Operation),
		Components: newComponents(value), Active: value.SupersededAt == nil, CreatedAt: value.CreatedAt, SupersededAt: value.SupersededAt,
	}
}

// RoutePriceResponse is the effective price attached to an admin model route.
type RoutePriceResponse struct {
	Source       string `json:"source"`
	ID           uint64 `json:"id,string,omitempty"`
	PricingModel string `json:"pricingModel,omitempty"`
	Operation    string `json:"operation,omitempty"`
	Components
}

func NewRoutePriceResponse(value pricingapp.RoutePrice) *RoutePriceResponse {
	result := &RoutePriceResponse{Source: value.Source, PricingModel: value.PricingModel, Operation: string(value.Operation)}
	if value.Price != nil {
		result.ID = value.Price.ID
		result.Components = newComponents(*value.Price)
	}
	return result
}

func (h *Handler) list(c *gin.Context) {
	includeHistory, _ := strconv.ParseBool(c.Query("includeHistory"))
	values, err := h.service.List(c.Request.Context(), c.Query("provider"), c.Query("model"), includeHistory)
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "modelPriceListFailed", "读取模型价格失败")
		return
	}
	items := make([]priceResponse, 0, len(values))
	for _, value := range values {
		items = append(items, newPriceResponse(value))
	}
	response.Success(c, http.StatusOK, gin.H{"items": items})
}

func (h *Handler) put(c *gin.Context) {
	var request putRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	rates := map[pricingdomain.Component]string{}
	for component, value := range map[pricingdomain.Component]*usdValue{
		pricingdomain.InputToken: request.InputTokenUSDPerMillion, pricingdomain.CachedInputToken: request.CachedInputTokenUSDPerMillion,
		pricingdomain.OutputToken: request.OutputTokenUSDPerMillion, pricingdomain.InputImage: request.InputImageUSD,
		pricingdomain.OutputImage: request.OutputImageUSD, pricingdomain.OutputSecond: request.OutputSecondUSD,
		pricingdomain.Character: request.CharacterUSDPerMillion,
	} {
		if value != nil {
			rates[component] = string(*value)
		}
	}
	value, err := h.service.Put(c.Request.Context(), pricingapp.Input{
		Provider: request.Provider, PricingModel: request.PricingModel, Operation: request.Operation, Rates: rates,
	})
	if err != nil {
		h.writeServiceError(c, "modelPriceSaveFailed", err)
		return
	}
	response.Success(c, http.StatusOK, newPriceResponse(value))
}

func (h *Handler) delete(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		response.Error(c, http.StatusBadRequest, "invalidId", "价格 ID 无效")
		return
	}
	if err := h.service.Delete(c.Request.Context(), id); err != nil {
		h.writeServiceError(c, "modelPriceDeleteFailed", err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"deleted": true})
}

func (h *Handler) writeServiceError(c *gin.Context, code string, err error) {
	switch {
	case errors.Is(err, pricingapp.ErrInvalidInput):
		response.Error(c, http.StatusBadRequest, "invalidModelPrice", err.Error())
	case errors.Is(err, pricingapp.ErrNotFound):
		response.Error(c, http.StatusNotFound, "modelPriceNotFound", err.Error())
	case errors.Is(err, pricingapp.ErrConflict):
		response.Error(c, http.StatusConflict, "modelPriceConflict", err.Error())
	default:
		response.Error(c, http.StatusInternalServerError, code, "模型价格操作失败")
	}
}
