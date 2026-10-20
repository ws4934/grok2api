package egress

import (
	"errors"
	"net/http"
	"time"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/chenyme/grok2api/backend/internal/transport/http/params"
	"github.com/gin-gonic/gin"
)

type sourceRequest struct {
	Name                   string  `json:"name"`
	Scope                  string  `json:"scope"`
	Enabled                bool    `json:"enabled"`
	URL                    *string `json:"url"`
	ClearURL               bool    `json:"clearUrl"`
	ProxyURL               *string `json:"proxyURL"`
	ClearProxyURL          bool    `json:"clearProxyURL"`
	RefreshIntervalSeconds *int    `json:"refreshIntervalSeconds"`
	DefaultAccountCapacity *int    `json:"defaultAccountCapacity"`
}

type sourceResponse struct {
	ID                     uint64     `json:"id,string"`
	Name                   string     `json:"name"`
	Scope                  string     `json:"scope"`
	Enabled                bool       `json:"enabled"`
	URLConfigured          bool       `json:"urlConfigured"`
	ProxyConfigured        bool       `json:"proxyConfigured"`
	RefreshIntervalSeconds int        `json:"refreshIntervalSeconds"`
	DefaultAccountCapacity int        `json:"defaultAccountCapacity"`
	LastSyncedAt           *time.Time `json:"lastSyncedAt,omitempty"`
	NextSyncAt             *time.Time `json:"nextSyncAt,omitempty"`
	LastSyncImported       int        `json:"lastSyncImported"`
	LastSyncError          string     `json:"lastSyncError,omitempty"`
}

type importRequest struct {
	Name            string `json:"name"`
	Scope           string `json:"scope"`
	AccountCapacity int    `json:"accountCapacity"`
	Content         string `json:"content"`
}

func (value sourceRequest) input() egressapp.SubscriptionSourceInput {
	return egressapp.SubscriptionSourceInput{
		Name: value.Name, Scope: egressdomain.Scope(value.Scope), Enabled: value.Enabled, URL: value.URL, ClearURL: value.ClearURL,
		ProxyURL: value.ProxyURL, ClearProxyURL: value.ClearProxyURL,
		RefreshIntervalSeconds: value.RefreshIntervalSeconds, DefaultAccountCapacity: value.DefaultAccountCapacity,
	}
}

func newSourceResponse(value egressdomain.PublicSubscriptionSource) sourceResponse {
	return sourceResponse{
		ID: value.ID, Name: value.Name, Scope: string(value.Scope), Enabled: value.Enabled, URLConfigured: value.URLConfigured,
		ProxyConfigured:        value.ProxyConfigured,
		RefreshIntervalSeconds: value.RefreshIntervalSeconds, DefaultAccountCapacity: value.DefaultAccountCapacity,
		LastSyncedAt: value.LastSyncedAt, NextSyncAt: value.NextSyncAt, LastSyncImported: value.LastSyncImported, LastSyncError: value.LastSyncError,
	}
}

func (h *Handler) listSources(c *gin.Context) {
	if !legacyEgressSourceListRequest(c) {
		page, pageSize := params.Page(c)
		values, total, err := h.service.ListSourcePage(c.Request.Context(), page, pageSize, c.Query("search"), egressapp.SourceListFilter{
			Scope: egressdomain.Scope(c.Query("scope")),
		})
		if h.writeSourceListError(c, err) {
			return
		}
		items := make([]sourceResponse, 0, len(values))
		for _, value := range values {
			items = append(items, newSourceResponse(value))
		}
		response.Success(c, http.StatusOK, gin.H{"items": items, "page": page, "pageSize": pageSize, "total": total})
		return
	}
	values, err := h.service.ListSources(c.Request.Context())
	if err != nil {
		h.writeError(c, err)
		return
	}
	items := make([]sourceResponse, 0, len(values))
	for _, value := range values {
		items = append(items, newSourceResponse(value))
	}
	response.Success(c, http.StatusOK, gin.H{"items": items})
}

func legacyEgressSourceListRequest(c *gin.Context) bool {
	if _, exists := c.GetQuery("page"); exists {
		return false
	}
	if _, exists := c.GetQuery("pageSize"); exists {
		return false
	}
	return c.Query("search") == "" && c.Query("scope") == ""
}

func (h *Handler) writeSourceListError(c *gin.Context, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, egressapp.ErrInvalidFilter):
		response.Error(c, http.StatusBadRequest, "invalidFilter", err.Error())
	default:
		response.Error(c, http.StatusInternalServerError, "egressSourceListFailed", "读取代理订阅来源失败")
	}
	return true
}

func (h *Handler) createSource(c *gin.Context) {
	var request sourceRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	value, err := h.service.CreateSource(c.Request.Context(), request.input())
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusCreated, newSourceResponse(value))
}

func (h *Handler) updateSource(c *gin.Context) {
	id, ok := params.PathID(c)
	if !ok {
		return
	}
	var request sourceRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	value, err := h.service.UpdateSource(c.Request.Context(), id, request.input())
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, newSourceResponse(value))
}

func (h *Handler) deleteSource(c *gin.Context) {
	id, ok := params.PathID(c)
	if !ok {
		return
	}
	if err := h.service.DeleteSource(c.Request.Context(), id); err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"deleted": true})
}

func (h *Handler) syncSource(c *gin.Context) {
	id, ok := params.PathID(c)
	if !ok {
		return
	}
	value, err := h.service.SyncSource(c.Request.Context(), id)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"imported": value.Imported, "skipped": value.Skipped})
}

func (h *Handler) importText(c *gin.Context) {
	var request importRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	value, err := h.service.ImportText(c.Request.Context(), egressapp.ImportInput{
		Name: request.Name, Scope: egressdomain.Scope(request.Scope), AccountCapacity: request.AccountCapacity, Content: request.Content,
	})
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusCreated, gin.H{"imported": value.Imported, "skipped": value.Skipped})
}
