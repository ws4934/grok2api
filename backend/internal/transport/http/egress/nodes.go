package egress

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

func (h *Handler) cleanupPreview(c *gin.Context) {
	value, err := h.service.PreviewUnhealthyCleanup(c.Request.Context())
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{
		"nodes": value.Nodes, "boundAccounts": value.BoundAccounts, "subscriptionManaged": value.SubscriptionManaged,
	})
}

func (h *Handler) cleanup(c *gin.Context) {
	deleted, err := h.service.DeleteUnhealthy(c.Request.Context())
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"deleted": deleted})
}

func (h *Handler) refreshClearance(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	if err := h.service.RefreshClearance(c.Request.Context(), id); err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"refreshed": true})
}

type nodeRequest struct {
	Name              string  `json:"name"`
	Scope             string  `json:"scope"`
	Enabled           bool    `json:"enabled"`
	ProxyPool         *bool   `json:"proxyPool"`
	AccountCapacity   *int    `json:"accountCapacity"`
	ProxyURL          *string `json:"proxyURL"`
	ProxyProfileID    *uint64 `json:"proxyProfileId,string"`
	ClearProxyURL     bool    `json:"clearProxyURL"`
	UserAgent         string  `json:"userAgent"`
	CloudflareCookies *string `json:"cloudflareCookies"`
	ClearCookies      bool    `json:"clearCookies"`
}

type nodeResponse struct {
	ID                   uint64              `json:"id,string"`
	Name                 string              `json:"name"`
	Scope                string              `json:"scope"`
	Enabled              bool                `json:"enabled"`
	ProxyConfigured      bool                `json:"proxyConfigured"`
	ProxyDisplay         string              `json:"proxyDisplay,omitempty"`
	ProxyFingerprint     string              `json:"proxyFingerprint,omitempty"`
	ProxyPool            bool                `json:"proxyPool"`
	SourceID             uint64              `json:"sourceId,omitempty,string"`
	ProxyProfileID       uint64              `json:"proxyProfileId,omitempty,string"`
	ProxyProfileName     string              `json:"proxyProfileName,omitempty"`
	AccountCapacity      int                 `json:"accountCapacity"`
	UserAgent            string              `json:"userAgent"`
	CookieConfigured     bool                `json:"cookieConfigured"`
	AccountBoundProxy    bool                `json:"accountBoundProxy"`
	Health               float64             `json:"health"`
	FailureCount         int                 `json:"failureCount"`
	CooldownUntil        *time.Time          `json:"cooldownUntil,omitempty"`
	LastError            string              `json:"lastError,omitempty"`
	ProbeStatus          string              `json:"probeStatus"`
	LastProbedAt         *time.Time          `json:"lastProbedAt,omitempty"`
	ProbeLatencyMS       int                 `json:"probeLatencyMs"`
	ExitIP               string              `json:"exitIp,omitempty"`
	ProbeError           string              `json:"probeError,omitempty"`
	ProbeProvider        string              `json:"probeProvider,omitempty"`
	IPv4Probe            probeFamilyResponse `json:"ipv4Probe"`
	IPv6Probe            probeFamilyResponse `json:"ipv6Probe"`
	AssignedAccountCount int                 `json:"assignedAccountCount"`
}

type probeFamilyResponse struct {
	Status    string     `json:"status"`
	TestedAt  *time.Time `json:"testedAt,omitempty"`
	LatencyMS int        `json:"latencyMs"`
	ExitIP    string     `json:"exitIp,omitempty"`
	Error     string     `json:"error,omitempty"`
}

type accountAssignmentRequest struct {
	Provider string   `json:"provider" binding:"required"`
	IDs      []string `json:"ids" binding:"required"`
	Mode     string   `json:"mode"`
}

type batchNodeDeleteRequest struct {
	IDs []string `json:"ids" binding:"required"`
}

type batchNodeUpdateRequest struct {
	IDs     []string `json:"ids" binding:"required"`
	Enabled *bool    `json:"enabled" binding:"required"`
}

type qualityProbeRequest struct {
	ClientKeyID     string `json:"clientKeyId" binding:"required"`
	Model           string `json:"model" binding:"required"`
	Prompt          string `json:"prompt"`
	Expected        string `json:"expected"`
	MaxOutputTokens int    `json:"maxOutputTokens"`
}

func (h *Handler) testQuality(c *gin.Context) {
	nodeID, ok := pathID(c)
	if !ok {
		return
	}
	var request qualityProbeRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	clientKeyID, err := strconv.ParseUint(request.ClientKeyID, 10, 63)
	if err != nil || clientKeyID == 0 {
		response.Error(c, http.StatusBadRequest, "invalidClientKeyId", "Client Key ID 无效")
		return
	}
	value, err := h.service.ProbeQuality(c.Request.Context(), nodeID, egressapp.QualityProbeInput{
		ClientKeyID: clientKeyID, Model: request.Model, Prompt: request.Prompt,
		Expected: request.Expected, MaxOutputTokens: request.MaxOutputTokens,
	})
	if err != nil {
		h.writeQualityProbeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{
		"requestId": value.RequestID, "nodeId": strconv.FormatUint(value.NodeID, 10), "model": value.Model,
		"statusCode": value.StatusCode, "firstTokenMs": value.FirstTokenMS, "durationMs": value.DurationMS,
		"generationMs": value.GenerationMS, "chunkCount": value.ChunkCount,
		"outputTokens": value.OutputTokens, "reasoningTokens": value.ReasoningTokens,
		"visibleTokens": value.VisibleTokens, "visibleCharacters": value.VisibleCharacters,
		"outputTokensPerSecond":  value.OutputTokensPerSecond,
		"visibleTokensPerSecond": value.OutputTokensPerSecond, "expectedMatched": value.ExpectedMatched,
		"thinkingRequired": value.ThinkingRequired,
		"responseSha256":   value.ResponseSHA256,
	})
}

func (h *Handler) updateMany(c *gin.Context) {
	var request batchNodeUpdateRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	ids, err := parseBoundedEgressNodeIDs(request.IDs, 5000)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", "代理节点 ID 无效")
		return
	}
	updated, err := h.service.UpdateManyEnabled(c.Request.Context(), ids, *request.Enabled)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"updated": updated})
}

func (h *Handler) deleteMany(c *gin.Context) {
	var request batchNodeDeleteRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	ids, err := parseBoundedEgressNodeIDs(request.IDs, 5000)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", "代理节点 ID 无效")
		return
	}
	deleted, err := h.service.DeleteMany(c.Request.Context(), ids)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"deleted": deleted})
}

func (h *Handler) assignAccounts(c *gin.Context) {
	nodeID, ok := pathID(c)
	if !ok {
		return
	}
	var request accountAssignmentRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	ids, err := parseAccountIDs(request.IDs)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", "账号 ID 无效")
		return
	}
	mode := accountdomain.EgressAssignmentMode(request.Mode)
	if mode == "" {
		mode = accountdomain.EgressAssignmentManual
	}
	result, err := h.service.AssignAccounts(c.Request.Context(), nodeID, accountdomain.Provider(request.Provider), ids, mode)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"assigned": result.Assigned})
}

func (h *Handler) unassignAccounts(c *gin.Context) {
	var request accountAssignmentRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	ids, err := parseAccountIDs(request.IDs)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", "账号 ID 无效")
		return
	}
	result, err := h.service.UnassignAccounts(c.Request.Context(), accountdomain.Provider(request.Provider), ids)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"assigned": result.Assigned})
}

func (value nodeRequest) input() egressapp.Input {
	return egressapp.Input{
		Name: value.Name, Scope: egressdomain.Scope(value.Scope), Enabled: value.Enabled, ProxyPool: value.ProxyPool,
		AccountCapacity: value.AccountCapacity,
		ProxyURL:        value.ProxyURL, ProxyProfileID: value.ProxyProfileID,
		ClearProxyURL: value.ClearProxyURL, UserAgent: value.UserAgent,
		CloudflareCookies: value.CloudflareCookies, ClearCookies: value.ClearCookies,
	}
}

func (h *Handler) list(c *gin.Context) {
	scope := egressdomain.Scope(c.Query("scope"))
	sort := repository.SortQuery{Field: c.Query("sortBy"), Direction: repository.SortDirection(c.Query("sortOrder"))}
	if legacyEgressListRequest(c) {
		values, err := h.service.ListAll(c.Request.Context(), scope, sort)
		if h.writeListError(c, err) {
			return
		}
		items := make([]nodeResponse, 0, len(values))
		for _, value := range values {
			items = append(items, newNodeResponse(value))
		}
		pageSize := len(items)
		if pageSize == 0 {
			pageSize = repository.DefaultPageSize
		}
		response.Success(c, http.StatusOK, gin.H{"items": items, "page": 1, "pageSize": pageSize, "total": len(items), "defaultUserAgents": h.service.DefaultUserAgents()})
		return
	}
	page, pageSize := nodePagination(c)
	values, total, err := h.service.List(c.Request.Context(), page, pageSize, c.Query("search"), egressapp.ListFilter{
		Scope: scope, Enabled: c.Query("enabled"), ProbeStatus: c.Query("probe"), Assignment: c.Query("assignment"),
		Sort: sort,
	})
	if h.writeListError(c, err) {
		return
	}
	items := make([]nodeResponse, 0, len(values))
	for _, value := range values {
		items = append(items, newNodeResponse(value))
	}
	response.Success(c, http.StatusOK, gin.H{"items": items, "page": page, "pageSize": pageSize, "total": total, "defaultUserAgents": h.service.DefaultUserAgents()})
}

func legacyEgressListRequest(c *gin.Context) bool {
	if _, exists := c.GetQuery("page"); exists {
		return false
	}
	if _, exists := c.GetQuery("pageSize"); exists {
		return false
	}
	return c.Query("search") == "" && c.Query("enabled") == "" && c.Query("probe") == "" && c.Query("assignment") == ""
}

func (h *Handler) writeListError(c *gin.Context, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, egressapp.ErrInvalidFilter):
		response.Error(c, http.StatusBadRequest, "invalidFilter", err.Error())
	case errors.Is(err, egressapp.ErrInvalidSort):
		response.Error(c, http.StatusBadRequest, "invalidSort", err.Error())
	default:
		response.Error(c, http.StatusInternalServerError, "egressNodeListFailed", "读取代理节点失败")
	}
	return true
}

func (h *Handler) create(c *gin.Context) {
	var request nodeRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	value, err := h.service.Create(c.Request.Context(), request.input())
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusCreated, newNodeResponse(value))
}

func (h *Handler) update(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	var request nodeRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	value, err := h.service.Update(c.Request.Context(), id, request.input())
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, newNodeResponse(value))
}

func (h *Handler) proxyURL(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	c.Header("Pragma", "no-cache")
	id, ok := pathID(c)
	if !ok {
		return
	}
	value, err := h.service.ProxyURL(c.Request.Context(), id)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"proxyURL": value})
}

func newNodeResponse(value egressdomain.PublicNode) nodeResponse {
	return nodeResponse{
		ID: value.ID, Name: value.Name, Scope: string(value.Scope), Enabled: value.Enabled,
		ProxyConfigured: value.ProxyConfigured, ProxyDisplay: value.ProxyDisplay, ProxyFingerprint: value.ProxyFingerprint,
		ProxyPool: value.ProxyPool, UserAgent: value.UserAgent, CookieConfigured: value.CookieConfigured,
		AccountBoundProxy: value.AccountBoundProxy,
		SourceID:          value.SourceID, AccountCapacity: value.AccountCapacity,
		ProxyProfileID: value.ProxyProfileID, ProxyProfileName: value.ProxyProfileName,
		Health: value.Health, FailureCount: value.FailureCount, CooldownUntil: value.CooldownUntil, LastError: value.LastError,
		ProbeStatus: string(value.ProbeStatus), LastProbedAt: value.LastProbedAt, ProbeLatencyMS: value.ProbeLatencyMS, ExitIP: value.ExitIP, ProbeError: value.ProbeError,
		ProbeProvider: string(value.ProbeProvider),
		IPv4Probe:     newProbeFamilyResponse(value.IPv4Probe), IPv6Probe: newProbeFamilyResponse(value.IPv6Probe),
		AssignedAccountCount: value.AssignedAccountCount,
	}
}

func newProbeFamilyResponse(value egressdomain.ProbeFamilyResult) probeFamilyResponse {
	status := value.Status
	if !status.IsValid() {
		status = egressdomain.ProbeStatusUnknown
	}
	var testedAt *time.Time
	if !value.TestedAt.IsZero() {
		canonical := value.TestedAt.UTC()
		testedAt = &canonical
	}
	return probeFamilyResponse{
		Status: string(status), TestedAt: testedAt, LatencyMS: value.LatencyMS, ExitIP: value.ExitIP, Error: value.Error,
	}
}

func parseAccountIDs(values []string) ([]uint64, error) {
	result := make([]uint64, 0, len(values))
	seen := make(map[uint64]struct{}, len(values))
	for _, value := range values {
		id, err := strconv.ParseUint(value, 10, 63)
		if err != nil || id == 0 {
			return nil, errors.New("invalid id")
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	if len(result) == 0 {
		return nil, errors.New("no ids")
	}
	return result, nil
}

func parseEgressNodeIDs(values []string) ([]uint64, error) {
	result := make([]uint64, 0, len(values))
	seen := make(map[uint64]struct{}, len(values))
	for _, value := range values {
		id, err := strconv.ParseUint(value, 10, 63)
		if err != nil || id == 0 {
			return nil, errors.New("invalid id")
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	if len(result) == 0 {
		return nil, errors.New("no ids")
	}
	return result, nil
}

func parseBoundedEgressNodeIDs(values []string, limit int) ([]uint64, error) {
	if len(values) == 0 || limit < 1 || len(values) > limit {
		return nil, errors.New("invalid id count")
	}
	return parseEgressNodeIDs(values)
}

func (h *Handler) delete(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	if err := h.service.Delete(c.Request.Context(), id); err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"deleted": true})
}

type probeBatchRequest struct {
	IDs []string `json:"ids"`
}

func (h *Handler) testNode(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	value, err := h.service.TestNode(c.Request.Context(), id)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{
		"status": value.Status, "testedAt": value.TestedAt, "latencyMs": value.LatencyMS, "exitIp": value.ExitIP, "error": value.Error,
		"probeProvider": value.Provider,
		"ipv4":          newProbeFamilyResponse(value.IPv4), "ipv6": newProbeFamilyResponse(value.IPv6),
	})
}

func (h *Handler) testNodes(c *gin.Context) {
	var request probeBatchRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	ids, err := parseOptionalAccountIDs(request.IDs)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", "账号 ID 无效")
		return
	}
	value, err := h.service.TestNodes(c.Request.Context(), ids)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"requested": value.Requested, "healthy": value.Healthy, "unhealthy": value.Unhealthy})
}

func parseOptionalAccountIDs(values []string) ([]uint64, error) {
	if len(values) == 0 {
		return nil, nil
	}
	return parseAccountIDs(values)
}
