package systemlog

import (
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	logapp "github.com/chenyme/grok2api/backend/internal/application/systemlog"
	logdomain "github.com/chenyme/grok2api/backend/internal/domain/systemlog"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

type Handler struct{ service *logapp.Service }

func NewHandler(service *logapp.Service) *Handler   { return &Handler{service: service} }
func (h *Handler) Register(router *gin.RouterGroup) { router.GET("/system/logs", h.list) }
func (h *Handler) list(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if h.service == nil {
		response.Error(c, http.StatusServiceUnavailable, "systemLogsUnavailable", "系统日志暂不可用")
		return
	}
	query, err := parseQuery(c)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidSystemLogQuery", "日志筛选参数无效")
		return
	}
	entries, more, err := h.service.List(c.Request.Context(), query)
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "systemLogsFailed", "读取系统日志失败")
		return
	}
	next := ""
	if more && len(entries) > 0 {
		next = strconv.FormatUint(entries[len(entries)-1].ID, 10)
	}
	response.Success(c, http.StatusOK, gin.H{"items": entries, "nextCursor": next, "status": h.service.Status()})
}
func parseQuery(c *gin.Context) (logdomain.Query, error) {
	query := logdomain.Query{Limit: 50, Level: strings.ToUpper(c.Query("level")), Search: strings.TrimSpace(c.Query("search")), Provider: c.Query("provider"), RequestID: c.Query("requestId")}
	for key, target := range map[string]*uint64{"cursor": &query.BeforeID, "accountId": &query.AccountID} {
		if value := c.Query(key); value != "" {
			parsed, err := strconv.ParseUint(value, 10, 64)
			if err != nil || parsed == 0 {
				return query, strconv.ErrSyntax
			}
			*target = parsed
		}
	}
	if value := c.Query("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 200 {
			return query, strconv.ErrSyntax
		}
		query.Limit = parsed
	}
	if value := c.Query("since"); value != "" {
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return query, err
		}
		query.Since = parsed
	}
	switch query.Level {
	case "", "INFO", "WARN", "ERROR":
	default:
		return query, strconv.ErrSyntax
	}
	if !utf8.ValidString(query.Search) || utf8.RuneCountInString(query.Search) > 200 || len(query.RequestID) > 64 || len(query.Provider) > 64 {
		return query, strconv.ErrSyntax
	}
	return query, nil
}
