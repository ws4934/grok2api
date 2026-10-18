package channelhttp

import (
	"encoding/json"
	"errors"
	"io"
	"strconv"

	app "github.com/chenyme/grok2api/backend/internal/application/channel"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/admin"
	domain "github.com/chenyme/grok2api/backend/internal/domain/channel"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

type Handler struct{ service *app.Service }

func NewHandler(s *app.Service) *Handler { return &Handler{service: s} }
func (h *Handler) Register(g *gin.RouterGroup) {
	g.GET("/channels", h.list)
	r := g.Group("/channels/:channel")
	r.GET("", h.settings)
	r.PATCH("", h.saveSettings)
	r.GET("/accounts", h.accounts)
	r.POST("/accounts", h.add)
	r.PATCH("/accounts/:account", h.update)
	r.DELETE("/accounts/:account", h.delete)
	r.POST("/accounts/import", h.importAccounts)
	r.POST("/accounts/export", h.export)
	r.POST("/accounts/actions", h.actions)
	r.POST("/oauth/sessions", h.startOAuth)
	r.POST("/oauth/sessions/:session/complete", h.completeOAuth)
	r.GET("/prices", h.prices)
	r.POST("/prices", h.savePrice)
	r.GET("/client-key-grants", h.grants)
	r.PUT("/client-key-grants/:key", h.grant)
	r.GET("/image-jobs", h.adminImageJobs)
	r.POST("/image-jobs/:job/retry", h.retryImageJob)
	r.GET("/client-models/:key", h.clientModels)
	r.GET("/models", h.models)
	r.POST("/models", h.createModel)
	r.DELETE("/models/:model", h.deleteModel)
	r.PATCH("/models/:model", h.setModelEnabled)
}
func provider(c *gin.Context) account.Provider { return account.Provider(c.Param("channel")) }
func id(c *gin.Context, param string) uint64 {
	n, _ := strconv.ParseUint(c.Param(param), 10, 64)
	return n
}
func adminID(c *gin.Context) uint64 {
	v, _ := c.Get(middleware.AdminKey)
	a, _ := v.(admin.Admin)
	return a.ID
}
func fail(c *gin.Context, e error) {
	status := 400
	code := "channel_request_failed"
	message := "渠道操作失败，请检查参数、凭据状态及上游连接"
	if errors.Is(e, domain.ErrNotFound) {
		status = 404
		code = "channel_not_found"
		message = "渠道资源不存在"
	}
	if errors.Is(e, domain.ErrConflict) {
		status = 409
		code = "channel_conflict"
		message = "资源已变更、重复或授权会话已使用，请刷新后重试"
	}
	response.Error(c, status, code, message)
}
func reply(c *gin.Context, v any, e error) {
	if e != nil {
		fail(c, e)
		return
	}
	response.Success(c, 200, v)
}
func bind(c *gin.Context, v any) bool {
	d := json.NewDecoder(c.Request.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		response.Error(c, 400, "invalid_channel_input", "请求格式不正确或包含不支持的字段")
		return false
	}
	var extra any
	if e := d.Decode(&extra); e != io.EOF {
		response.Error(c, 400, "invalid_channel_input", "请求必须包含一个 JSON 对象")
		return false
	}
	return true
}
func (h *Handler) list(c *gin.Context) { v, e := h.service.List(c.Request.Context()); reply(c, v, e) }
func (h *Handler) settings(c *gin.Context) {
	v, e := h.service.Settings(c.Request.Context(), provider(c))
	reply(c, v, e)
}
func (h *Handler) saveSettings(c *gin.Context) {
	v, e := h.service.Settings(c.Request.Context(), provider(c))
	if e != nil {
		fail(c, e)
		return
	}
	if !bind(c, &v) {
		return
	}
	reply(c, v, h.service.SaveSettings(c.Request.Context(), provider(c), v))
}
func (h *Handler) accounts(c *gin.Context) {
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	v, total, e := h.service.Accounts(c.Request.Context(), provider(c), offset, limit)
	reply(c, gin.H{"items": v, "total": total}, e)
}
func (h *Handler) add(c *gin.Context) {
	var v domain.ImportRecord
	if !bind(c, &v) {
		return
	}
	b, e := json.Marshal(v)
	if e != nil {
		fail(c, e)
		return
	}
	out, e := h.service.Import(c.Request.Context(), provider(c), b, false)
	reply(c, out, e)
}
func (h *Handler) importAccounts(c *gin.Context) {
	var v struct {
		Data    string `json:"data"`
		Replace bool   `json:"replace"`
	}
	if !bind(c, &v) {
		return
	}
	out, e := h.service.Import(c.Request.Context(), provider(c), []byte(v.Data), v.Replace)
	reply(c, out, e)
}
func (h *Handler) export(c *gin.Context) {
	var v struct {
		IDs []uint64 `json:"ids"`
	}
	if !bind(c, &v) {
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	out, e := h.service.Export(c.Request.Context(), provider(c), v.IDs)
	reply(c, gin.H{"version": 1, "accounts": out}, e)
}
func (h *Handler) update(c *gin.Context) {
	var v app.AccountPatch
	if !bind(c, &v) {
		return
	}
	out, e := h.service.UpdateAccount(c.Request.Context(), provider(c), id(c, "account"), v)
	reply(c, out, e)
}
func (h *Handler) delete(c *gin.Context) {
	reply(c, gin.H{"deleted": true}, h.service.DeleteAccount(c.Request.Context(), provider(c), id(c, "account")))
}
func (h *Handler) actions(c *gin.Context) {
	var v struct {
		IDs    []uint64 `json:"ids"`
		Action string   `json:"action"`
	}
	if !bind(c, &v) {
		return
	}
	if len(v.IDs) == 0 || len(v.IDs) > 100 {
		response.Error(c, 400, "invalid_batch", "每次操作 1–100 个账号")
		return
	}
	out := []gin.H{}
	for _, id := range v.IDs {
		var e error
		switch v.Action {
		case "enable", "disable":
			enabled := v.Action == "enable"
			_, e = h.service.UpdateAccount(c.Request.Context(), provider(c), id, app.AccountPatch{Enabled: &enabled})
		case "delete":
			e = h.service.DeleteAccount(c.Request.Context(), provider(c), id)
		default:
			_, e = h.service.AccountAction(c.Request.Context(), provider(c), id, v.Action)
		}
		reason := ""
		if e != nil {
			reason = "操作失败，请检查账户状态或任务引用"
		}
		if errors.Is(e, domain.ErrConflict) {
			reason = "账户正在使用、资源已变更或有未完成任务"
		}
		out = append(out, gin.H{"id": id, "success": e == nil, "reason": reason})
	}
	reply(c, out, nil)
}
func (h *Handler) startOAuth(c *gin.Context) {
	v, e := h.service.StartOAuth(c.Request.Context(), provider(c), adminID(c))
	reply(c, v, e)
}
func (h *Handler) completeOAuth(c *gin.Context) {
	var v struct {
		URL string `json:"callback_url"`
	}
	if !bind(c, &v) {
		return
	}
	out, e := h.service.CompleteOAuth(c.Request.Context(), provider(c), c.Param("session"), adminID(c), v.URL)
	reply(c, out, e)
}
func (h *Handler) prices(c *gin.Context) {
	v, e := h.service.Prices(c.Request.Context(), provider(c))
	reply(c, v, e)
}
func (h *Handler) savePrice(c *gin.Context) {
	var v domain.Price
	if !bind(c, &v) {
		return
	}
	out, e := h.service.SavePrice(c.Request.Context(), provider(c), v)
	reply(c, out, e)
}
func (h *Handler) grants(c *gin.Context) {
	v, e := h.service.Grants(c.Request.Context(), provider(c))
	reply(c, v, e)
}
func (h *Handler) grant(c *gin.Context) {
	var v struct {
		Enabled bool `json:"enabled"`
	}
	if !bind(c, &v) {
		return
	}
	reply(c, gin.H{"enabled": v.Enabled}, h.service.SetGrant(c.Request.Context(), provider(c), id(c, "key"), v.Enabled))
}
func (h *Handler) models(c *gin.Context) {
	v, e := h.service.Models(c.Request.Context(), provider(c))
	reply(c, v, e)
}
func (h *Handler) createModel(c *gin.Context) {
	var v app.ModelInput
	if !bind(c, &v) {
		return
	}
	out, e := h.service.CreateModel(c.Request.Context(), provider(c), v)
	reply(c, out, e)
}
func (h *Handler) deleteModel(c *gin.Context) {
	reply(c, gin.H{"deleted": true}, h.service.DeleteModel(c.Request.Context(), provider(c), id(c, "model")))
}

func (h *Handler) retryImageJob(c *gin.Context) {
	var v struct {
		IdempotencyKey string `json:"idempotency_key"`
	}
	if !bind(c, &v) {
		return
	}
	job, e := h.service.RetryUncertainImages(c.Request.Context(), provider(c), c.Param("job"), v.IdempotencyKey)
	reply(c, job, e)
}
func (h *Handler) clientModels(c *gin.Context) {
	rows, e := h.service.AdminClientModels(c.Request.Context(), provider(c), id(c, "key"))
	reply(c, rows, e)
}

func (h *Handler) setModelEnabled(c *gin.Context) {
	var input struct {
		Enabled bool `json:"enabled"`
	}
	if !bind(c, &input) {
		return
	}
	row, e := h.service.SetModelEnabled(c.Request.Context(), provider(c), id(c, "model"), input.Enabled)
	reply(c, row, e)
}
