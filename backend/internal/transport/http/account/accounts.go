package account

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/chenyme/grok2api/backend/internal/transport/http/params"
	"github.com/gin-gonic/gin"
)

type updateRequest struct {
	Name                   *string                       `json:"name"`
	Enabled                *bool                         `json:"enabled"`
	Priority               *int                          `json:"priority"`
	MaxConcurrent          *int                          `json:"maxConcurrent"`
	MinimumRemaining       *float64                      `json:"minimumRemaining"`
	CloudflareCookies      *string                       `json:"cloudflareCookies"`
	ClearCloudflareCookies bool                          `json:"clearCloudflareCookies"`
	BuildSuperEntitled     *bool                         `json:"buildSuperEntitled"`
	BuildRouteMode         *accountdomain.BuildRouteMode `json:"buildRouteMode"`
}

func (h *Handler) list(c *gin.Context) {
	page, pageSize := params.Page(c)
	values, total, err := h.service.List(c.Request.Context(), page, pageSize, c.Query("search"), accountapp.ListFilter{
		Provider: c.Query("provider"), QuotaType: c.Query("type"), Status: c.Query("status"), Egress: c.Query("egress"),
		Renewal: c.Query("renewal"), Risk: c.Query("risk"), Agreement: c.Query("agreement"), Association: c.Query("association"),
		Sort: repository.SortQuery{Field: c.Query("sortBy"), Direction: repository.SortDirection(c.Query("sortOrder"))},
	})
	if errors.Is(err, accountapp.ErrInvalidFilter) {
		response.Error(c, http.StatusBadRequest, "invalidFilter", err.Error())
		return
	}
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "accountListFailed", "读取账号失败")
		return
	}
	items := make([]accountResponse, 0, len(values))
	for _, value := range values {
		items = append(items, newAccountResponse(value))
	}
	response.Success(c, http.StatusOK, gin.H{"items": items, "page": page, "pageSize": pageSize, "total": total})
}

func (h *Handler) summary(c *gin.Context) {
	value, err := h.service.Summary(c.Request.Context())
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "accountSummaryFailed", "读取账号统计失败")
		return
	}
	providers := gin.H{}
	for name, summary := range value.Providers {
		providers[name] = gin.H{"total": summary.Total, "available": summary.Available}
	}
	response.Success(c, http.StatusOK, gin.H{
		"total": value.Total, "available": value.Available, "recovering": value.Recovering, "attention": value.Attention, "risk": value.Risk,
		"providers": providers,
		"recovery":  gin.H{"cooldown": value.Recovery.Cooldown, "waitingReset": value.Recovery.WaitingReset, "probing": value.Recovery.Probing},
		"issues":    gin.H{"disabled": value.Issues.Disabled, "reauthRequired": value.Issues.ReauthRequired},
	})
}

func (h *Handler) get(c *gin.Context) {
	id, ok := params.PathID(c)
	if !ok {
		return
	}
	value, err := h.service.Get(c.Request.Context(), id)
	if err != nil {
		h.writeServiceError(c, "accountGetFailed", err, http.StatusInternalServerError, "读取账号失败")
		return
	}
	response.Success(c, http.StatusOK, newAccountResponse(value))
}

func (h *Handler) refreshQuota(c *gin.Context) {
	id, ok := params.PathID(c)
	if !ok {
		return
	}
	if _, err := h.service.RefreshQuota(c.Request.Context(), id); err != nil {
		h.writeServiceError(c, "quotaRefreshFailed", err, http.StatusBadGateway, "同步账号额度失败")
		return
	}
	value, err := h.service.Get(c.Request.Context(), id)
	if err != nil {
		h.writeServiceError(c, "accountGetFailed", err, http.StatusInternalServerError, "读取账号失败")
		return
	}
	response.Success(c, http.StatusOK, newAccountResponse(value))
}

func (h *Handler) update(c *gin.Context) {
	id, ok := params.PathID(c)
	if !ok {
		return
	}
	var request updateRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	value, err := h.service.Update(c.Request.Context(), id, accountapp.UpdateInput{
		Name: request.Name, Enabled: request.Enabled, Priority: request.Priority,
		MaxConcurrent: request.MaxConcurrent, MinimumRemaining: request.MinimumRemaining,
		CloudflareCookies: request.CloudflareCookies, ClearCloudflareCookies: request.ClearCloudflareCookies,
		BuildSuperEntitled: request.BuildSuperEntitled, BuildRouteMode: request.BuildRouteMode,
	})
	if err != nil {
		h.writeServiceError(c, "accountUpdateFailed", err, http.StatusInternalServerError, "更新账号失败")
		return
	}
	result := newAccountResponse(value)
	if value.EnabledChanged && result.CooldownUntil != nil && time.Now().UTC().Before(*result.CooldownUntil) {
		result.EnabledDoesNotClearCooldown = true
	}
	if request.BuildSuperEntitled != nil {
		if synchronizer, ok := h.sync.(accountModelSynchronizer); ok {
			result.ModelSyncFailed = synchronizer.SyncModels(c.Request.Context(), id) != nil
		}
	}
	response.Success(c, http.StatusOK, result)
}

func (h *Handler) clearCooldown(c *gin.Context) {
	id, ok := params.PathID(c)
	if !ok {
		return
	}
	value, err := h.service.ClearCooldown(c.Request.Context(), id)
	if err != nil {
		h.writeServiceError(c, "accountClearCooldownFailed", err, http.StatusInternalServerError, "清除账号冷却失败")
		return
	}
	response.Success(c, http.StatusOK, newAccountResponse(value))
}

func (h *Handler) delete(c *gin.Context) {
	id, ok := params.PathID(c)
	if !ok {
		return
	}
	var request struct {
		Provider            string   `json:"provider"`
		LinkedDeleteTargets []string `json:"linkedDeleteTargets"`
	}
	// Empty body = legacy single-account delete. Non-empty body must bind cleanly
	// so a truncated/malformed linked-delete request cannot silently drop targets.
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &request); err != nil {
			response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
			return
		}
	}
	targets, err := parseLinkedDeleteTargets(request.LinkedDeleteTargets)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidLinkedDeleteTargets", err.Error())
		return
	}
	if len(targets) > 0 {
		if request.Provider == "" {
			response.Error(c, http.StatusBadRequest, "invalidProvider", "删除关联账号时必须指定 provider")
			return
		}
		result, err := h.service.DeleteWithLinked(c.Request.Context(), accountdomain.Provider(request.Provider), id, targets)
		if err != nil {
			h.writeServiceError(c, "accountDeleteFailed", err, http.StatusInternalServerError, "删除账号失败")
			return
		}
		response.Success(c, http.StatusOK, newAccountDeleteResponse(result))
		return
	}
	if err := h.service.Delete(c.Request.Context(), id); err != nil {
		h.writeServiceError(c, "accountDeleteFailed", err, http.StatusInternalServerError, "删除账号失败")
		return
	}
	response.Success(c, http.StatusOK, gin.H{"deleted": true})
}

func parseLinkedDeleteTargets(values []string) ([]accountdomain.Provider, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make([]accountdomain.Provider, 0, len(values))
	seen := map[accountdomain.Provider]struct{}{}
	for _, value := range values {
		provider := accountdomain.Provider(strings.TrimSpace(value))
		if !provider.IsLegacy() {
			return nil, fmt.Errorf("关联删除目标无效")
		}
		if _, ok := seen[provider]; ok {
			continue
		}
		seen[provider] = struct{}{}
		out = append(out, provider)
	}
	return out, nil
}

func newAccountDeleteResponse(result accountapp.AccountDeleteResult) gin.H {
	byProvider := gin.H{}
	for provider, count := range result.DeletedByProvider {
		byProvider[string(provider)] = count
	}
	return gin.H{
		"deleted":           result.Deleted,
		"rootsDeleted":      result.RootsDeleted,
		"linkedDeleted":     result.LinkedDeleted,
		"skipped":           result.Skipped,
		"deletedByProvider": byProvider,
	}
}

func (h *Handler) refreshToken(c *gin.Context) {
	id, ok := params.PathID(c)
	if !ok {
		return
	}
	value, err := h.service.RefreshToken(c.Request.Context(), id)
	if err != nil {
		h.writeServiceError(c, "tokenRefreshFailed", err, http.StatusBadGateway, "刷新账号凭据失败")
		return
	}
	response.Success(c, http.StatusOK, newAccountResponse(value))
}

func (h *Handler) acceptWebTerms(c *gin.Context) {
	id, ok := params.PathID(c)
	if !ok {
		return
	}
	if err := h.service.AcceptWebTerms(c.Request.Context(), id); err != nil {
		h.writeServiceError(c, "webTermsAcceptanceFailed", err, http.StatusBadGateway, "接受 Grok Web 服务协议失败")
		return
	}
	response.Success(c, http.StatusOK, gin.H{"completed": true})
}

func (h *Handler) setWebBirthDate(c *gin.Context) {
	id, ok := params.PathID(c)
	if !ok {
		return
	}
	if err := h.service.SetWebBirthDate(c.Request.Context(), id); err != nil {
		h.writeServiceError(c, "webBirthDateUpdateFailed", err, http.StatusBadGateway, "设置 Grok Web 账号生日失败")
		return
	}
	response.Success(c, http.StatusOK, gin.H{"completed": true})
}

func (h *Handler) enableWebNSFW(c *gin.Context) {
	id, ok := params.PathID(c)
	if !ok {
		return
	}
	if err := h.service.EnableWebNSFW(c.Request.Context(), id); err != nil {
		h.writeServiceError(c, "webNSFWEnableFailed", err, http.StatusBadGateway, "开启 Grok Web NSFW 失败")
		return
	}
	response.Success(c, http.StatusOK, gin.H{"completed": true})
}

func (h *Handler) refreshBilling(c *gin.Context) {
	id, ok := params.PathID(c)
	if !ok {
		return
	}
	value, err := h.service.RefreshBilling(c.Request.Context(), id)
	if err != nil {
		h.writeServiceError(c, "billingRefreshFailed", err, http.StatusBadGateway, "刷新账号额度失败")
		return
	}
	response.Success(c, http.StatusOK, newBillingResponse(value))
}
