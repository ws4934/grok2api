package inference

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	gateway          *gateway.Service
	models           *modelapp.Service
	maxBodyBytes     int64
	publicAPIBaseURL string
	publicBaseURL    func() string
}

const (
	responseCopyBufferBytes         = 32 << 10
	maxJSONMetadataInspectionBytes  = 8 << 20
	maxStreamEventInspectionBytes   = 8 << 20
	maxStreamFailureDiagnosticBytes = 64 << 10
	maxCredentialErrorInspectBytes  = 64 << 10
	maxJSONResponseTransferBytes    = 128 << 20
	maxStreamResponseTransferBytes  = 256 << 20
	maxMediaResponseTransferBytes   = int64(2) << 30
	responseWriteTimeout            = 30 * time.Second
)

var (
	errResponseTransferLimit    = errors.New("响应超过代理安全上限")
	errUpstreamStreamIncomplete = errors.New("上游流在终止事件前结束")
	errUpstreamStreamFailed     = errors.New("上游流返回失败终止事件")
	errUpstreamStreamRead       = errors.New("读取上游流失败")
)

type streamProtocol uint8

const (
	streamProtocolResponses streamProtocol = iota
	streamProtocolChat
	streamProtocolAnthropic
	streamProtocolImage
)

const mediaTransferErrorTrailer = "X-Grok2API-Transfer-Error"

func NewHandler(gatewayService *gateway.Service, models *modelapp.Service, maxBodyBytes int64, publicAPIBaseURL ...string) *Handler {
	baseURL := ""
	if len(publicAPIBaseURL) > 0 {
		baseURL = strings.TrimRight(strings.TrimSpace(publicAPIBaseURL[0]), "/")
	}
	return &Handler{gateway: gatewayService, models: models, maxBodyBytes: maxBodyBytes, publicAPIBaseURL: baseURL}
}

// SetPublicAPIBaseURLResolver makes video content URLs follow hot-updated runtime settings.
// Set it before Register; request handling only reads the resolver.
func (h *Handler) SetPublicAPIBaseURLResolver(resolve func() string) *Handler {
	h.publicBaseURL = resolve
	return h
}

func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/models", h.listModels)
	router.POST("/responses", h.createResponse)
	router.POST("/chat/completions", h.createChatCompletion)
	router.POST("/messages", h.createMessage)
	router.POST("/images/generations", h.generateImage)
	router.POST("/images/edits", h.editImage)
	router.POST("/videos/generations", h.generateVideo)
	router.POST("/videos/edits", h.editVideo)
	router.POST("/videos/extensions", h.extendVideo)
	router.GET("/videos/:requestId", h.getVideo)
	router.GET("/videos/:requestId/content", h.getVideoContent)
	router.POST("/tts", h.synthesizeSpeech)
	router.GET("/tts/voices", h.listTTSVoices)
	router.GET("/tts/voices/:voiceId", h.getTTSVoice)
	router.POST("/stt", h.transcribeSpeech)
	router.GET("/stt", h.proxySTTWebSocket)
	// OpenAI-compatible audio aliases for common client SDKs.
	router.POST("/audio/speech", h.synthesizeOpenAISpeech)
	router.POST("/audio/tasks", h.synthesizeOpenAIAudioTask)
	router.POST("/audio/transcriptions", h.transcribeOpenAIAudio)
	router.GET("/realtime", h.proxyRealtimeWebSocket)
	router.POST("/responses/compact", h.compactResponse)
	router.GET("/responses/:responseId", h.getResponse)
	router.DELETE("/responses/:responseId", h.deleteResponse)
	router.POST("/responses/:responseId/cancel", h.cancelResponse)
}

type responsesRequest struct {
	Model              string `json:"model"`
	Stream             bool   `json:"stream"`
	PromptCacheKey     string `json:"prompt_cache_key"`
	PreviousResponseID string `json:"previous_response_id"`
	promptCacheSignals
}

type chatCompletionRequest struct {
	Model          string `json:"model"`
	Stream         bool   `json:"stream"`
	PromptCacheKey string `json:"prompt_cache_key"`
	promptCacheSignals
}

type messagesRequest struct {
	Model          string          `json:"model"`
	MaxTokens      *int            `json:"max_tokens"`
	Messages       json.RawMessage `json:"messages"`
	Stream         bool            `json:"stream"`
	PromptCacheKey string          `json:"prompt_cache_key"`
	promptCacheSignals
}

type modelListItem struct {
	ID         string                 `json:"id"`
	Object     string                 `json:"object"`
	Created    int64                  `json:"created"`
	OwnedBy    string                 `json:"owned_by"`
	Provider   account.Provider       `json:"-"`
	Capability modeldomain.Capability `json:"-"`
}

func (h *Handler) listModels(c *gin.Context) {
	allowAliases := false
	var clientKey clientkeydomain.Key
	hasClientKey := false
	if clientValue, exists := c.Get(middleware.ClientKey); exists {
		if value, ok := clientValue.(clientkeydomain.Key); ok {
			clientKey = value
			hasClientKey = true
			allowAliases = clientKey.AllowModelAliases
		}
	}
	var values []modeldomain.Route
	var err error
	if hasClientKey {
		values, err = h.models.ListEnabledForClientKey(c.Request.Context(), clientKey)
	} else {
		values, err = h.models.ListEnabled(c.Request.Context())
	}
	if err != nil {
		writeOpenAIError(c, http.StatusInternalServerError, "model_list_failed", "读取模型列表失败")
		return
	}
	if hasClientKey {
		values = filterModelRoutesForClientKey(values, clientKey)
	}
	items := newModelListItems(values)
	if allowAliases {
		items = appendReasoningModelAliases(items)
	}
	if clientVersion := strings.TrimSpace(c.Query("client_version")); clientVersion != "" {
		writeCodexModelCatalog(c, newCodexModelCatalog(items, h.gateway.ProviderDefinition))
		return
	}
	c.JSON(http.StatusOK, gin.H{"object": "list", "data": items})
}

func filterModelRoutesForClientKey(values []modeldomain.Route, key clientkeydomain.Key) []modeldomain.Route {
	filtered := make([]modeldomain.Route, 0, len(values))
	scope := key.AccountScope()
	for _, value := range values {
		if scope.AllowsProvider(value.Provider) && key.AllowsModel(value.ID) {
			filtered = append(filtered, value)
		}
	}
	return filtered
}

// newModelListItems deduplicates by downstream public name and hides Provider prefixes used only for internal routing.
func newModelListItems(values []modeldomain.Route) []modelListItem {
	data := make([]modelListItem, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		publicID := modeldomain.ExternalPublicID(value.Provider, value.PublicID)
		if seen[publicID] {
			continue
		}
		seen[publicID] = true
		data = append(data, modelListItem{ID: publicID, Object: "model", Created: value.CreatedAt.Unix(), OwnedBy: "grok2api", Provider: value.Provider, Capability: value.Capability})
	}
	return data
}

// appendReasoningModelAliases expands base models into effort-suffixed aliases using only
// levels each model actually supports (never a blanket none/low/medium/high/xhigh/max template).
func appendReasoningModelAliases(items []modelListItem) []modelListItem {
	if len(items) == 0 {
		return items
	}
	seen := make(map[string]bool, len(items)*2)
	result := make([]modelListItem, 0, len(items)*2)
	for _, item := range items {
		seen[item.ID] = true
		result = append(result, item)
	}
	for _, item := range items {
		for _, aliasID := range modeldomain.ReasoningAliasPublicIDsForProvider(item.Provider, item.ID) {
			if seen[aliasID] {
				continue
			}
			seen[aliasID] = true
			result = append(result, modelListItem{
				ID: aliasID, Object: "model", Created: item.Created, OwnedBy: item.OwnedBy,
				Provider: item.Provider, Capability: item.Capability,
			})
		}
	}
	return result
}

func (h *Handler) createResponse(c *gin.Context) {
	h.handleCreate(c, false)
}

func (h *Handler) compactResponse(c *gin.Context) {
	h.handleCreate(c, true)
}

func (h *Handler) createChatCompletion(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxBodyBytes)
	if !isJSONRequest(c) {
		writeOpenAIError(c, http.StatusUnsupportedMediaType, "invalid_request", "Chat Completions only supports application/json")
		return
	}
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		writeOpenAIError(c, http.StatusRequestEntityTooLarge, "request_too_large", "请求体超过限制")
		return
	}
	var request chatCompletionRequest
	if json.Unmarshal(body, &request) != nil || strings.TrimSpace(request.Model) == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "Chat Completions 请求缺少有效 model")
		return
	}
	clientValue, exists := c.Get(middleware.ClientKey)
	clientKey, ok := clientValue.(clientkeydomain.Key)
	if !exists || !ok {
		writeOpenAIError(c, http.StatusUnauthorized, "invalid_api_key", "客户端 API Key 无效")
		return
	}
	requestID, _ := c.Get(middleware.RequestIDKey)
	requestIDValue, _ := requestID.(string)
	result, err := h.gateway.CreateChatCompletion(c.Request.Context(), gateway.Input{
		RequestID: requestIDValue, ClientKey: clientKey, PublicModel: request.Model,
		Body: body, Streaming: request.Stream, PromptCacheKey: request.PromptCacheKey,
		PromptCacheSeed:           extractPromptCacheSeed(c.Request.Header, request.PromptCacheKey, request.promptCacheSignals),
		AllowClientToolCacheRoute: allowBuildClientToolCacheRoute(c.Request.Header),
		GrokTurnIndex:             c.GetHeader("x-grok-turn-idx"),
		Method:                    c.Request.Method,
		Path:                      c.Request.URL.Path,
		Headers:                   c.Request.Header.Clone(),
	})
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	h.writeResult(c, result, request.Stream, streamProtocolChat)
}

func (h *Handler) createMessage(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxBodyBytes)
	if !isJSONRequest(c) {
		writeAnthropicError(c, http.StatusUnsupportedMediaType, "invalid_request_error", "Messages only supports application/json")
		return
	}
	if strings.TrimSpace(c.GetHeader("anthropic-version")) == "" {
		writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "anthropic-version header is required")
		return
	}
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		writeAnthropicError(c, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body exceeds the configured limit")
		return
	}
	var request messagesRequest
	if json.Unmarshal(body, &request) != nil || strings.TrimSpace(request.Model) == "" || request.MaxTokens == nil || *request.MaxTokens <= 0 || len(bytes.TrimSpace(request.Messages)) == 0 {
		writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "model, max_tokens, and messages are required")
		return
	}
	clientValue, exists := c.Get(middleware.ClientKey)
	clientKey, ok := clientValue.(clientkeydomain.Key)
	if !exists || !ok {
		writeAnthropicError(c, http.StatusUnauthorized, "authentication_error", "invalid API key")
		return
	}
	requestID, _ := c.Get(middleware.RequestIDKey)
	requestIDValue, _ := requestID.(string)
	result, err := h.gateway.CreateMessage(c.Request.Context(), gateway.Input{
		RequestID: requestIDValue, ClientKey: clientKey, PublicModel: request.Model,
		Body: body, Streaming: request.Stream, PromptCacheKey: request.PromptCacheKey,
		PromptCacheSeed:           extractPromptCacheSeed(c.Request.Header, request.PromptCacheKey, request.promptCacheSignals),
		AllowClientToolCacheRoute: allowBuildClientToolCacheRoute(c.Request.Header),
		GrokTurnIndex:             c.GetHeader("x-grok-turn-idx"),
		Method:                    c.Request.Method,
		Path:                      c.Request.URL.Path,
		Headers:                   c.Request.Header.Clone(),
	})
	if err != nil {
		writeGatewayAnthropicError(c, err)
		return
	}
	h.writeAnthropicResult(c, result, request.Stream)
}

func requestIdentity(c *gin.Context) (clientkeydomain.Key, string, bool) {
	clientValue, exists := c.Get(middleware.ClientKey)
	clientKey, ok := clientValue.(clientkeydomain.Key)
	if !exists || !ok {
		writeOpenAIError(c, http.StatusUnauthorized, "invalid_api_key", "客户端 API Key 无效")
		return clientkeydomain.Key{}, "", false
	}
	requestID, _ := c.Get(middleware.RequestIDKey)
	requestIDValue, _ := requestID.(string)
	return clientKey, requestIDValue, true
}

func (h *Handler) handleCreate(c *gin.Context, compact bool) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxBodyBytes)
	if !isJSONRequest(c) {
		writeOpenAIError(c, http.StatusUnsupportedMediaType, "invalid_request", "Responses only supports application/json")
		return
	}
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		writeOpenAIError(c, http.StatusRequestEntityTooLarge, "request_too_large", "请求体超过限制")
		return
	}
	var request responsesRequest
	if err := json.Unmarshal(body, &request); err != nil || strings.TrimSpace(request.Model) == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "Responses 请求缺少有效 model")
		return
	}
	if compact {
		body, err = forceJSONBoolean(body, "stream", false)
		if err != nil {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "Compact 请求格式无效")
			return
		}
		request.Stream = false
	}
	clientValue, exists := c.Get(middleware.ClientKey)
	clientKey, ok := clientValue.(clientkeydomain.Key)
	if !exists || !ok {
		writeOpenAIError(c, http.StatusUnauthorized, "invalid_api_key", "客户端 API Key 无效")
		return
	}
	requestID, _ := c.Get(middleware.RequestIDKey)
	requestIDValue, _ := requestID.(string)
	if !compact && h.handleImageResponse(c, body, clientKey, requestIDValue) {
		return
	}
	input := gateway.Input{
		RequestID: requestIDValue, ClientKey: clientKey, PublicModel: request.Model,
		Body: body, Streaming: request.Stream, PromptCacheKey: request.PromptCacheKey,
		PromptCacheSeed: extractPromptCacheSeed(c.Request.Header, request.PromptCacheKey, request.promptCacheSignals), PreviousResponseID: request.PreviousResponseID,
		AllowClientToolCacheRoute: allowBuildClientToolCacheRoute(c.Request.Header),
		GrokTurnIndex:             c.GetHeader("x-grok-turn-idx"),
		Method:                    c.Request.Method,
		Path:                      c.Request.URL.Path,
		Headers:                   c.Request.Header.Clone(),
	}
	var result *gateway.Result
	if compact {
		result, err = h.gateway.CompactResponse(c.Request.Context(), input)
	} else {
		result, err = h.gateway.CreateResponse(c.Request.Context(), input)
	}
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	h.writeResponsesResult(c, result, request.Stream && !compact, request.Model)
}

func isJSONRequest(c *gin.Context) bool {
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	return err == nil && strings.EqualFold(mediaType, "application/json")
}

func decodeSingleJSON(reader io.Reader, target any, disallowUnknown bool) error {
	decoder := json.NewDecoder(reader)
	if disallowUnknown {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("请求体只能包含一个 JSON 对象")
		}
		return err
	}
	return nil
}

func (h *Handler) getResponse(c *gin.Context) {
	// Image jobs carry their own prefix and never reach the stored-response lookup.
	if id := strings.TrimSpace(c.Param("responseId")); strings.HasPrefix(id, gateway.ImageJobIDPrefix) {
		h.getImageResponse(c, id)
		return
	}
	h.handleOwnedResource(c, false)
}

func (h *Handler) deleteResponse(c *gin.Context) {
	h.handleOwnedResource(c, true)
}

func (h *Handler) handleOwnedResource(c *gin.Context, deleteResource bool) {
	clientValue, exists := c.Get(middleware.ClientKey)
	clientKey, ok := clientValue.(clientkeydomain.Key)
	if !exists || !ok {
		writeOpenAIError(c, http.StatusUnauthorized, "invalid_api_key", "客户端 API Key 无效")
		return
	}
	input := gateway.ResourceInput{ClientKey: clientKey, ResponseID: strings.TrimSpace(c.Param("responseId")), RawQuery: c.Request.URL.RawQuery}
	if input.ResponseID == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "response_id 不能为空")
		return
	}
	var result *gateway.Result
	var err error
	if deleteResource {
		result, err = h.gateway.DeleteResponse(c.Request.Context(), input)
	} else {
		result, err = h.gateway.GetResponse(c.Request.Context(), input)
	}
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	h.writeResult(c, result, false, streamProtocolResponses)
}

func forceJSONBoolean(body []byte, key string, value bool) ([]byte, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	payload[key] = json.RawMessage("false")
	if value {
		payload[key] = json.RawMessage("true")
	}
	return json.Marshal(payload)
}
