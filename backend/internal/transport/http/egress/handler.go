package egress

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	service         *egressapp.Service
	guardStatePath  string
	guardConfigPath string
	guardProbe      egressapp.QualityProbeInput
	guardStateMu    sync.Mutex
	guardStateCache qualityGuardStateCache
	profilesMu      sync.Mutex
}

func NewHandler(service *egressapp.Service, guardStatePath ...string) *Handler {
	path := ""
	if len(guardStatePath) > 0 {
		path = strings.TrimSpace(guardStatePath[0])
	}
	configPath := ""
	if len(guardStatePath) > 1 {
		configPath = strings.TrimSpace(guardStatePath[1])
	}
	return &Handler{service: service, guardStatePath: path, guardConfigPath: configPath}
}

// WithQualityGuardProbe pins the sidecar probe to server-owned credentials and
// payload. The internal caller can select only the physical egress node.
func (h *Handler) WithQualityGuardProbe(input egressapp.QualityProbeInput) *Handler {
	h.guardProbe = input
	return h
}

func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/egress-proxy-profiles", h.listProxyProfiles)
	router.GET("/egress-proxy-profiles/:id", h.getProxyProfile)
	router.POST("/egress-proxy-profiles", h.createProxyProfile)
	router.PUT("/egress-proxy-profiles/:id", h.updateProxyProfile)
	router.DELETE("/egress-proxy-profiles/:id", h.deleteProxyProfile)
	router.POST("/egress-proxy-profiles/:id/proxy-url/reveal", h.proxyProfileURL)
	router.GET("/egress-nodes", h.list)
	router.POST("/egress-nodes", h.create)
	router.PATCH("/egress-nodes/batch", h.updateMany)
	router.DELETE("/egress-nodes", h.deleteMany)
	router.GET("/egress-nodes/cleanup-preview", h.cleanupPreview)
	router.POST("/egress-nodes/cleanup", h.cleanup)
	router.POST("/egress-nodes/test", h.testNodes)
	router.POST("/egress-nodes/:id/test", h.testNode)
	router.POST("/egress-nodes/:id/proxy-url/reveal", h.proxyURL)
	router.POST("/egress-nodes/:id/quality-test", h.testQuality)
	router.GET("/egress-quality-guard", h.qualityGuardStatus)
	router.PUT("/egress-quality-guard/config", h.updateQualityGuardConfig)
	router.GET("/egress-quality-guard/profiles", h.listQualityGuardProfiles)
	router.POST("/egress-quality-guard/profiles", h.createQualityGuardProfile)
	router.PUT("/egress-quality-guard/profiles/:id", h.updateQualityGuardProfile)
	router.DELETE("/egress-quality-guard/profiles/:id", h.deleteQualityGuardProfile)
	router.POST("/egress-quality-guard/nodes/:id/test", h.testQualityGuardNode)
	router.POST("/egress-nodes/:id/accounts", h.assignAccounts)
	router.DELETE("/egress-nodes/accounts", h.unassignAccounts)
	router.PUT("/egress-nodes/:id", h.update)
	router.POST("/egress-nodes/:id/refresh-clearance", h.refreshClearance)
	router.DELETE("/egress-nodes/:id", h.delete)
	router.POST("/egress-imports", h.importText)
	router.GET("/egress-sources", h.listSources)
	router.POST("/egress-sources", h.createSource)
	router.POST("/egress-sources/:id/sync", h.syncSource)
	router.PUT("/egress-sources/:id", h.updateSource)
	router.DELETE("/egress-sources/:id", h.deleteSource)
	router.GET("/egress-operations", h.operationsConfig)
	router.PUT("/egress-operations", h.updateOperationsConfig)
	router.POST("/egress-operations/rebalance", h.rebalance)
}

// RegisterQualityGuard exposes the minimum egress surface required by the
// sidecar. Destructive node, source, binding, and credential operations remain
// available only through administrator authentication.
func (h *Handler) RegisterQualityGuard(router *gin.RouterGroup) {
	router.GET("/egress-nodes", h.list)
	router.PATCH("/egress-nodes/batch", h.updateMany)
	router.POST("/egress-nodes/:id/test", h.testNode)
	router.POST("/egress-nodes/:id/quality-test", h.testQualityGuardNode)
	router.GET("/egress-leases", h.listQualityGuardLeases)
	router.POST("/egress-leases/quarantine", h.quarantineQualityGuardLease)
	router.POST("/egress-leases/restore", h.restoreQualityGuardLease)
	router.GET("/egress-operations", h.operationsConfig)
}

func nodePagination(c *gin.Context) (int, int) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("pageSize", "20"))
	return repository.NormalizePage(page, pageSize, repository.DefaultPageSize)
}

func (h *Handler) writeError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, egressapp.ErrInvalidInput):
		response.Error(c, http.StatusBadRequest, "invalidEgressNode", err.Error())
	case errors.Is(err, egressapp.ErrNotFound):
		response.Error(c, http.StatusNotFound, "egressNodeNotFound", err.Error())
	case errors.Is(err, egressapp.ErrProxyProfileNotFound):
		response.Error(c, http.StatusNotFound, "egressProxyProfileNotFound", err.Error())
	case errors.Is(err, egressapp.ErrProxyProfileInUse):
		response.Error(c, http.StatusConflict, "egressProxyProfileInUse", err.Error())
	case errors.Is(err, egressapp.ErrProxyProfileUnavailable):
		response.Error(c, http.StatusServiceUnavailable, "egressProxyProfilesUnavailable", err.Error())
	case errors.Is(err, egressapp.ErrProbeStale):
		response.Error(c, http.StatusConflict, "egressProbeStale", err.Error())
	case errors.Is(err, repository.ErrConflict):
		response.Error(c, http.StatusConflict, "egressConflict", "名称已存在")
	case errors.Is(err, egressapp.ErrOperationsUnavailable):
		response.Error(c, http.StatusServiceUnavailable, "egressOperationsUnavailable", "代理运营功能暂不可用")
	case errors.Is(err, egressapp.ErrSubscriptionSync):
		response.Error(c, http.StatusBadGateway, "egressSubscriptionSyncFailed", "代理订阅同步失败")
	case errors.Is(err, egressapp.ErrClearanceUnavailable):
		response.Error(c, http.StatusConflict, "clearanceRefreshUnavailable", err.Error())
	case errors.Is(err, egressapp.ErrQualityProbeUnavailable):
		response.Error(c, http.StatusServiceUnavailable, "egressQualityProbeUnavailable", err.Error())
	case strings.Contains(err.Error(), "FlareSolverr") || strings.Contains(err.Error(), "Clearance"):
		response.Error(c, http.StatusBadGateway, "clearanceRefreshFailed", err.Error())
	default:
		response.Error(c, http.StatusInternalServerError, "egressNodeOperationFailed", "代理节点操作失败")
	}
}

func (h *Handler) writeQualityProbeError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, egressapp.ErrQualityProbeNoAccount):
		response.Error(c, http.StatusServiceUnavailable, "egressQualityProbeNoAccount", "质量检测暂无可调度账号，请稍后重试")
	case errors.Is(err, egressapp.ErrInvalidInput),
		errors.Is(err, egressapp.ErrNotFound),
		errors.Is(err, egressapp.ErrQualityProbeUnavailable):
		h.writeError(c, err)
	default:
		response.Error(c, http.StatusBadGateway, "egressQualityProbeFailed", "质量检测暂不可用，请稍后重试")
	}
}

func pathID(c *gin.Context) (uint64, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 63)
	if err != nil || id == 0 {
		response.Error(c, http.StatusBadRequest, "invalidId", "ID 无效")
		return 0, false
	}
	return id, true
}
