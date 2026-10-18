package providercatalog

import (
	"net/http"

	"github.com/chenyme/grok2api/backend/internal/application/providercatalog"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

type Handler struct{ catalog *providercatalog.Catalog }

func NewHandler(catalog *providercatalog.Catalog) *Handler { return &Handler{catalog: catalog} }

func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/providers", func(c *gin.Context) { response.Success(c, http.StatusOK, h.catalog.Entries()) })
}
