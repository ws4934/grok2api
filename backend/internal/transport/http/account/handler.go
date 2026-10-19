package account

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountsyncapp "github.com/chenyme/grok2api/backend/internal/application/accountsync"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	admindomain "github.com/chenyme/grok2api/backend/internal/domain/admin"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

type accountSynchronizer interface {
	Sync(ctx context.Context, accountIDs ...uint64) accountsyncapp.Result
	SyncStream(ctx context.Context, accountIDs <-chan uint64) accountsyncapp.Result
}

type accountSyncProgressor interface {
	SyncStreamObserved(ctx context.Context, accountIDs <-chan uint64, observer func(completed, total int)) accountsyncapp.Result
}

type accountModelSynchronizer interface {
	SyncModels(ctx context.Context, accountID uint64) error
}

const (
	maxAccountImportBytes         = 30 << 20
	maxAccountImportFiles         = 1000
	maxAuthCompleteBodyBytes      = 64 << 10
	accountSyncQueueCapacity      = 20
	accountEventHeartbeatInterval = 15 * time.Second
	accountEventWriteTimeout      = 30 * time.Second
)

type Handler struct {
	service *accountapp.Service
	sync    accountSynchronizer
}

func NewHandler(service *accountapp.Service, sync accountSynchronizer) *Handler {
	return &Handler{service: service, sync: sync}
}

func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/accounts", h.list)
	router.GET("/accounts/summary", h.summary)
	router.GET("/accounts/export", h.exportCredentials)
	router.POST("/accounts/export", h.exportSelectedCredentials)
	router.GET("/accounts/:id", h.get)
	// Provider-keyed routes live under the catalog resource; /accounts/:provider would clash with /accounts/:id.
	router.POST("/providers/:provider/accounts/import", h.importAuth)
	router.POST("/providers/:provider/accounts/auth/start", h.startAuthorization)
	router.POST("/providers/:provider/accounts/auth/:sessionId/complete", h.completeAuthorization)
	router.POST("/providers/:provider/accounts/refresh-quotas", h.refreshProviderQuotas)
	router.POST("/accounts/web/convert-to-build", h.convertWebToBuild)
	router.POST("/accounts/web/sync-to-console", h.syncWebToConsole)
	router.POST("/accounts/web/run-scripts", h.runWebAccountScripts)
	router.POST("/accounts/web/:id/accept-terms", h.acceptWebTerms)
	router.POST("/accounts/web/:id/birth-date", h.setWebBirthDate)
	router.POST("/accounts/web/:id/nsfw", h.enableWebNSFW)
	router.POST("/accounts/refresh-billing", h.refreshAllBilling)
	router.POST("/accounts/reset-quota", h.resetAllBuildQuota)
	router.POST("/accounts/refresh-tokens", h.refreshAllTokens)
	router.POST("/accounts/cleanup", h.cleanup)
	router.POST("/accounts/cleanup-preview", h.cleanupPreview)
	router.POST("/accounts/batch/refresh-billing", h.batchRefreshBilling)
	router.POST("/accounts/batch/reset-quota", h.batchResetQuota)
	router.POST("/accounts/batch/refresh-quotas", h.batchRefreshQuotas)
	router.POST("/accounts/batch/refresh-tokens", h.batchRefreshTokens)
	router.POST("/accounts/detect", h.detectBuildAccounts)
	router.PATCH("/accounts/batch", h.batchUpdate)
	router.POST("/accounts/deletion-preview", h.previewDeletion)
	router.DELETE("/accounts", h.batchDelete)
	router.PATCH("/accounts/:id", h.update)
	router.DELETE("/accounts/:id", h.delete)
	router.POST("/accounts/:id/clear-cooldown", h.clearCooldown)
	router.POST("/accounts/:id/refresh-token", h.refreshToken)
	router.POST("/accounts/:id/refresh-billing", h.refreshBilling)
	router.POST("/accounts/:id/refresh-quota", h.refreshWebQuota)
}

// writeServiceError 仅暴露明确的账号业务错误，未知内部错误使用稳定文案。
func (h *Handler) writeServiceError(c *gin.Context, code string, err error, fallbackStatus int, fallbackMessage string) {
	switch {
	case errors.Is(err, accountapp.ErrImportLimit):
		response.Error(c, http.StatusBadRequest, "accountImportLimitExceeded", err.Error())
	case errors.Is(err, accountapp.ErrExportLimit):
		response.Error(c, http.StatusBadRequest, "accountExportLimitExceeded", err.Error())
	case errors.Is(err, accountapp.ErrInvalidInput), errors.Is(err, accountapp.ErrInvalidImport):
		response.Error(c, http.StatusBadRequest, code, err.Error())
	case errors.Is(err, accountapp.ErrAccountPoolMismatch):
		response.Error(c, http.StatusConflict, "accountPoolMismatch", err.Error())
	case errors.Is(err, accountapp.ErrConflict):
		response.Error(c, http.StatusConflict, code, err.Error())
	case errors.Is(err, accountapp.ErrNotFound):
		response.Error(c, http.StatusNotFound, "accountNotFound", err.Error())
	case errors.Is(err, accountapp.ErrUnsupported):
		response.Error(c, http.StatusConflict, "accountOperationUnsupported", err.Error())
	case errors.Is(err, accountapp.ErrConversionBusy):
		response.Error(c, http.StatusConflict, "accountConversionBusy", err.Error())
	case errors.Is(err, accountapp.ErrWebAccountScriptBusy):
		response.Error(c, http.StatusConflict, "webAccountScriptBusy", err.Error())
	default:
		response.Error(c, fallbackStatus, code, fallbackMessage)
	}
}

func pagination(c *gin.Context) (int, int) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("pageSize", "20"))
	return repository.NormalizePage(page, size, repository.DefaultPageSize)
}

func pathID(c *gin.Context) (uint64, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		response.Error(c, http.StatusBadRequest, "invalidId", "ID 无效")
		return 0, false
	}
	return id, true
}

func parseIDs(values []string) ([]uint64, error) {
	ids := make([]uint64, 0, len(values))
	for _, value := range values {
		id, err := strconv.ParseUint(value, 10, 64)
		if err != nil || id == 0 {
			return nil, errors.New("ID 无效")
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// pathProvider resolves the provider named in the route; providers missing from the registry are 404.
func (h *Handler) pathProvider(c *gin.Context) (accountdomain.Provider, bool) {
	value := accountdomain.Provider(c.Param("provider"))
	if _, ok := h.service.ProviderDefinition(value); !ok {
		response.Error(c, http.StatusNotFound, "providerNotFound", "账号来源不存在")
		return "", false
	}
	return value, true
}

// registeredProvider rejects request-body providers that are not in the registry.
func (h *Handler) registeredProvider(c *gin.Context, value accountdomain.Provider) bool {
	if _, ok := h.service.ProviderDefinition(value); !ok {
		response.Error(c, http.StatusBadRequest, "invalidProvider", "账号来源无效")
		return false
	}
	return true
}

func currentAdminID(c *gin.Context) (uint64, bool) {
	value, _ := c.Get(middleware.AdminKey)
	adminValue, ok := value.(admindomain.Admin)
	if !ok || adminValue.ID == 0 {
		response.Error(c, http.StatusUnauthorized, "adminUnauthorized", "管理员登录已失效")
		return 0, false
	}
	return adminValue.ID, true
}

func (h *Handler) validateProviderIDs(c *gin.Context, ids []uint64, providerValue string) bool {
	value := accountdomain.Provider(providerValue)
	if !h.registeredProvider(c, value) {
		return false
	}
	valid, err := h.service.AccountsBelongToProvider(c.Request.Context(), ids, value)
	if err != nil {
		h.writeServiceError(c, "accountPoolValidationFailed", err, http.StatusInternalServerError, "校验账号号池失败")
		return false
	}
	if !valid {
		response.Error(c, http.StatusConflict, "accountPoolMismatch", "批量操作包含不属于当前号池的账号")
		return false
	}
	return true
}
