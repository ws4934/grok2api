package egress

import (
	"net/http"
	"time"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

type proxyProfileRequest struct {
	Name     string  `json:"name"`
	ProxyURL *string `json:"proxyURL"`
}

type proxyProfileResponse struct {
	ID               uint64    `json:"id,string"`
	Name             string    `json:"name"`
	ProxyDisplay     string    `json:"proxyDisplay,omitempty"`
	ProxyFingerprint string    `json:"proxyFingerprint,omitempty"`
	BoundNodeCount   int       `json:"boundNodeCount"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

func (h *Handler) listProxyProfiles(c *gin.Context) {
	page, pageSize := nodePagination(c)
	values, total, err := h.service.ListProxyProfiles(c.Request.Context(), page, pageSize, c.Query("search"))
	if err != nil {
		h.writeError(c, err)
		return
	}
	items := make([]proxyProfileResponse, 0, len(values))
	for _, value := range values {
		items = append(items, newProxyProfileResponse(value))
	}
	response.Success(c, http.StatusOK, gin.H{"items": items, "page": page, "pageSize": pageSize, "total": total})
}

func (h *Handler) createProxyProfile(c *gin.Context) {
	var request proxyProfileRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	value, err := h.service.CreateProxyProfile(c.Request.Context(), egressapp.ProxyProfileInput{Name: request.Name, ProxyURL: request.ProxyURL})
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusCreated, newProxyProfileResponse(value))
}

func (h *Handler) getProxyProfile(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	value, err := h.service.GetProxyProfile(c.Request.Context(), id)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, newProxyProfileResponse(value))
}

func (h *Handler) updateProxyProfile(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	var request proxyProfileRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	value, err := h.service.UpdateProxyProfile(c.Request.Context(), id, egressapp.ProxyProfileInput{Name: request.Name, ProxyURL: request.ProxyURL})
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, newProxyProfileResponse(value))
}

func (h *Handler) deleteProxyProfile(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	if err := h.service.DeleteProxyProfile(c.Request.Context(), id); err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"deleted": true})
}

func (h *Handler) proxyProfileURL(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	c.Header("Pragma", "no-cache")
	id, ok := pathID(c)
	if !ok {
		return
	}
	value, err := h.service.ProxyProfileURL(c.Request.Context(), id)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"proxyURL": value})
}

func newProxyProfileResponse(value egressdomain.PublicProxyProfile) proxyProfileResponse {
	return proxyProfileResponse{
		ID: value.ID, Name: value.Name, ProxyDisplay: value.ProxyDisplay, ProxyFingerprint: value.ProxyFingerprint,
		BoundNodeCount: value.BoundNodeCount, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
}
