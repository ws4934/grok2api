package account

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/chenyme/grok2api/backend/internal/transport/http/params"
	"github.com/gin-gonic/gin"
)

type batchUpdateRequest struct {
	IDs              []string `json:"ids" binding:"required"`
	Provider         string   `json:"provider" binding:"required"`
	Enabled          *bool    `json:"enabled"`
	Priority         *int     `json:"priority"`
	MaxConcurrent    *int     `json:"maxConcurrent"`
	MinimumRemaining *float64 `json:"minimumRemaining"`
}

type batchDeleteRequest struct {
	IDs                 []string `json:"ids" binding:"required"`
	Provider            string   `json:"provider" binding:"required"`
	LinkedDeleteTargets []string `json:"linkedDeleteTargets"`
}

type deletionPreviewRequest struct {
	IDs                 []string `json:"ids" binding:"required"`
	Provider            string   `json:"provider" binding:"required"`
	LinkedDeleteTargets []string `json:"linkedDeleteTargets"`
}

type accountCleanupRequest struct {
	Provider            string                     `json:"provider" binding:"required"`
	Statuses            []accountapp.CleanupStatus `json:"statuses" binding:"required"`
	LinkedDeleteTargets []string                   `json:"linkedDeleteTargets"`
}

// detectBuildAccountsRequest 要求显式选择全部账号或提供非空 id 集合。
type detectBuildAccountsRequest struct {
	IDs      []string `json:"ids"`
	All      bool     `json:"all"`
	Provider string   `json:"provider"`
}

// accountDetectItemResponse 是检测任务的单账号增量事件；全量检测仅推送 invalid。
type accountDetectItemResponse struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Email      string `json:"email,omitempty"`
	Outcome    string `json:"outcome"`
	Reason     string `json:"reason,omitempty"`
	HTTPStatus int    `json:"httpStatus,omitempty"`
}

type accountBatchResponse struct {
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
}

type accountTokenRefreshResponse struct {
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
}

func (h *Handler) batchUpdate(c *gin.Context) {
	var request batchUpdateRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	ids, err := params.IDs(request.IDs)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", err.Error())
		return
	}
	updated, err := h.service.BatchUpdate(c.Request.Context(), accountdomain.Provider(request.Provider), ids, accountapp.UpdateInput{Enabled: request.Enabled, Priority: request.Priority, MaxConcurrent: request.MaxConcurrent, MinimumRemaining: request.MinimumRemaining})
	if err != nil {
		h.writeServiceError(c, "accountBatchUpdateFailed", err, http.StatusInternalServerError, "批量更新账号失败")
		return
	}
	response.Success(c, http.StatusOK, gin.H{"updated": updated})
}

func (h *Handler) batchDelete(c *gin.Context) {
	var request batchDeleteRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	ids, err := params.IDs(request.IDs)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", err.Error())
		return
	}
	if !h.validateProviderIDs(c, ids, request.Provider) {
		return
	}
	targets, err := parseLinkedDeleteTargets(request.LinkedDeleteTargets)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidLinkedDeleteTargets", err.Error())
		return
	}
	result, err := h.service.BatchDeleteWithLinked(c.Request.Context(), accountdomain.Provider(request.Provider), ids, targets)
	if err != nil {
		h.writeServiceError(c, "accountBatchDeleteFailed", err, http.StatusInternalServerError, "批量删除账号失败")
		return
	}
	response.Success(c, http.StatusOK, newAccountDeleteResponse(result))
}

func (h *Handler) previewDeletion(c *gin.Context) {
	var request deletionPreviewRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	ids, err := params.IDs(request.IDs)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", err.Error())
		return
	}
	if !h.validateProviderIDs(c, ids, request.Provider) {
		return
	}
	targets, err := parseLinkedDeleteTargets(request.LinkedDeleteTargets)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidLinkedDeleteTargets", err.Error())
		return
	}
	resolution, err := h.service.PreviewLinkedDelete(c.Request.Context(), accountdomain.Provider(request.Provider), ids, targets)
	if err != nil {
		h.writeServiceError(c, "accountDeletionPreviewFailed", err, http.StatusInternalServerError, "预览删除账号失败")
		return
	}
	linked := gin.H{}
	for provider, count := range resolution.LinkedByProvider {
		linked[string(provider)] = count
	}
	response.Success(c, http.StatusOK, gin.H{
		"rootCount":        len(resolution.RootIDs),
		"linkedByProvider": linked,
		"total":            len(resolution.FinalIDs),
	})
}

func (h *Handler) batchRefreshBilling(c *gin.Context) {
	var request batchDeleteRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	ids, err := params.IDs(request.IDs)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", err.Error())
		return
	}
	if definition, ok := h.service.ProviderDefinition(accountdomain.Provider(request.Provider)); ok && definition.Quota != provider.QuotaBilling {
		response.Error(c, http.StatusBadRequest, "invalidProvider", "该账号来源不支持 Billing 同步")
		return
	}
	if !h.validateProviderIDs(c, ids, request.Provider) {
		return
	}
	succeeded, failed, err := h.service.BatchRefreshBilling(c.Request.Context(), ids)
	if err != nil {
		h.writeServiceError(c, "billingBatchRefreshFailed", err, http.StatusBadGateway, "批量同步 Billing 失败")
		return
	}
	response.Success(c, http.StatusOK, gin.H{"succeeded": succeeded, "failed": failed})
}

func (h *Handler) detectBuildAccounts(c *gin.Context) {
	var request detectBuildAccountsRequest
	if c.Request.Body != nil {
		if err := json.NewDecoder(c.Request.Body).Decode(&request); err != nil && !errors.Is(err, io.EOF) {
			response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
			return
		}
	}
	if request.Provider != "" && request.Provider != string(accountdomain.ProviderBuild) {
		response.Error(c, http.StatusBadRequest, "invalidProvider", "仅 Grok Build 账号支持可用性检测")
		return
	}
	hasIDs := len(request.IDs) > 0
	if request.All == hasIDs {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "必须明确选择全部账号或提供非空账号 ID")
		return
	}
	var ids []uint64
	if hasIDs {
		parsed, err := params.IDs(request.IDs)
		if err != nil {
			response.Error(c, http.StatusBadRequest, "invalidId", err.Error())
			return
		}
		if !h.validateProviderIDs(c, parsed, string(accountdomain.ProviderBuild)) {
			return
		}
		ids = parsed
	}
	stream := newAccountEventStream(c)
	defer stream.Close()
	itemObserver := func(item accountapp.BuildDetectItemResult) error {
		return stream.Write("item", accountDetectItemResponse{
			ID:         strconv.FormatUint(item.AccountID, 10),
			Name:       item.Name,
			Email:      item.Email,
			Outcome:    string(item.Outcome),
			Reason:     item.Reason,
			HTTPStatus: item.HTTPStatus,
		})
	}
	succeeded, failed, err := h.service.DetectBuildAccountsWithProgress(c.Request.Context(), ids, request.All, stream.ProgressObserver(), itemObserver)
	if err != nil {
		stream.WriteError("accountDetectFailed", "检测 Grok Build 账号失败")
		return
	}
	_ = stream.Write("complete", accountBatchResponse{Succeeded: succeeded, Failed: failed})
}

func (h *Handler) batchResetQuota(c *gin.Context) {
	var request batchDeleteRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	ids, err := params.IDs(request.IDs)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", err.Error())
		return
	}
	if request.Provider != string(accountdomain.ProviderBuild) {
		response.Error(c, http.StatusBadRequest, "invalidProvider", "仅 Grok Build 账号支持手动重置额度状态")
		return
	}
	reset, err := h.service.BatchResetQuotaState(c.Request.Context(), ids)
	if err != nil {
		h.writeServiceError(c, "quotaBatchResetFailed", err, http.StatusInternalServerError, "批量重置额度状态失败")
		return
	}
	response.Success(c, http.StatusOK, gin.H{"reset": reset})
}

func (h *Handler) resetAllBuildQuota(c *gin.Context) {
	reset, err := h.service.ResetAllBuildQuotaState(c.Request.Context())
	if err != nil {
		h.writeServiceError(c, "quotaResetFailed", err, http.StatusInternalServerError, "重置全部 Grok Build 额度状态失败")
		return
	}
	response.Success(c, http.StatusOK, gin.H{"reset": reset})
}

func (h *Handler) cleanup(c *gin.Context) {
	var request accountCleanupRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	targets, err := parseLinkedDeleteTargets(request.LinkedDeleteTargets)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidLinkedDeleteTargets", err.Error())
		return
	}
	result, err := h.service.CleanupAccounts(c.Request.Context(), accountdomain.Provider(request.Provider), request.Statuses, targets)
	if err != nil {
		h.writeServiceError(c, "accountCleanupFailed", err, http.StatusInternalServerError, "清理账号失败")
		return
	}
	byProvider := gin.H{}
	for provider, count := range result.DeletedByProvider {
		byProvider[string(provider)] = count
	}
	response.Success(c, http.StatusOK, gin.H{
		"deleted":           result.Deleted,
		"rootsDeleted":      result.RootsDeleted,
		"linkedDeleted":     result.LinkedDeleted,
		"skipped":           result.Skipped,
		"deletedByProvider": byProvider,
	})
}

// cleanupPreview returns root and linked-peer counts for the cleanup dialog.
func (h *Handler) cleanupPreview(c *gin.Context) {
	var request accountCleanupRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	targets, err := parseLinkedDeleteTargets(request.LinkedDeleteTargets)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidLinkedDeleteTargets", err.Error())
		return
	}
	preview, err := h.service.PreviewCleanup(c.Request.Context(), accountdomain.Provider(request.Provider), request.Statuses, targets)
	if err != nil {
		h.writeServiceError(c, "accountCleanupPreviewFailed", err, http.StatusInternalServerError, "预览清理账号失败")
		return
	}
	rootsByStatus := gin.H{}
	for status, count := range preview.RootsByStatus {
		rootsByStatus[status] = count
	}
	linked := gin.H{}
	for provider, count := range preview.LinkedByProvider {
		linked[string(provider)] = count
	}
	response.Success(c, http.StatusOK, gin.H{
		"rootsByStatus":    rootsByStatus,
		"rootCount":        preview.RootCount,
		"linkedByProvider": linked,
		"total":            preview.Total,
	})
}

func (h *Handler) batchRefreshQuotas(c *gin.Context) {
	var request batchDeleteRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	ids, err := params.IDs(request.IDs)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", err.Error())
		return
	}
	if !h.validateProviderIDs(c, ids, request.Provider) {
		return
	}
	definition, _ := h.service.ProviderDefinition(accountdomain.Provider(request.Provider))
	var succeeded, failed int
	if definition.Quota == provider.QuotaBilling {
		succeeded, failed, err = h.service.BatchRefreshBilling(c.Request.Context(), ids)
	} else {
		succeeded, failed, err = h.service.BatchRefreshQuota(c.Request.Context(), ids)
	}
	if err != nil {
		h.writeServiceError(c, "quotaBatchRefreshFailed", err, http.StatusBadGateway, "批量同步账号额度失败")
		return
	}
	response.Success(c, http.StatusOK, gin.H{"succeeded": succeeded, "failed": failed})
}

func (h *Handler) batchRefreshTokens(c *gin.Context) {
	var request batchDeleteRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	ids, err := params.IDs(request.IDs)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", err.Error())
		return
	}
	if definition, ok := h.service.ProviderDefinition(accountdomain.Provider(request.Provider)); ok && !definition.Credential.Refresh {
		response.Error(c, http.StatusBadRequest, "invalidProvider", "该账号来源不支持凭据刷新")
		return
	}
	if !h.validateProviderIDs(c, ids, request.Provider) {
		return
	}
	succeeded, failed, skipped, err := h.service.BatchRefreshTokens(c.Request.Context(), ids)
	if err != nil {
		h.writeServiceError(c, "tokenRefreshFailed", err, http.StatusBadGateway, "批量刷新账号凭据失败")
		return
	}
	response.Success(c, http.StatusOK, gin.H{"succeeded": succeeded, "failed": failed, "skipped": skipped})
}

func (h *Handler) refreshAllBilling(c *gin.Context) {
	stream := newAccountEventStream(c)
	defer stream.Close()
	succeeded, failed, err := h.service.SyncAllBillingWithProgress(c.Request.Context(), stream.ProgressObserver())
	if err != nil {
		stream.WriteError("billingRefreshFailed", "刷新账号额度失败")
		return
	}
	_ = stream.Write("complete", accountBatchResponse{Succeeded: succeeded, Failed: failed})
}

func (h *Handler) refreshAllTokens(c *gin.Context) {
	stream := newAccountEventStream(c)
	defer stream.Close()
	succeeded, failed, skipped, err := h.service.RefreshAllTokensWithProgress(c.Request.Context(), stream.ProgressObserver())
	if err != nil {
		stream.WriteError("tokenRefreshFailed", "续期账号凭据失败")
		return
	}
	_ = stream.Write("complete", accountTokenRefreshResponse{Succeeded: succeeded, Failed: failed, Skipped: skipped})
}

// refreshProviderQuotas syncs every enabled account of the provider: quota windows or billing, per its Definition.
func (h *Handler) refreshProviderQuotas(c *gin.Context) {
	providerValue, ok := h.pathProvider(c)
	if !ok {
		return
	}
	stream := newAccountEventStream(c)
	defer stream.Close()
	succeeded, failed, err := h.service.SyncAllQuotasWithProgress(c.Request.Context(), providerValue, stream.ProgressObserver())
	if err != nil {
		stream.WriteError("quotaRefreshFailed", "同步账号额度失败")
		return
	}
	_ = stream.Write("complete", accountBatchResponse{Succeeded: succeeded, Failed: failed})
}
