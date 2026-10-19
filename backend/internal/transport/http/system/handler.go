package system

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	updatecheckapp "github.com/chenyme/grok2api/backend/internal/application/updatecheck"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	publicAPIBaseURL func() string
	updates          *updatecheckapp.Service
}

func NewHandler(publicAPIBaseURL func() string, updates *updatecheckapp.Service) *Handler {
	if publicAPIBaseURL == nil {
		publicAPIBaseURL = func() string { return "" }
	}
	if updates == nil {
		updates = updatecheckapp.NewService("dev", "", nil)
	}
	return &Handler{publicAPIBaseURL: publicAPIBaseURL, updates: updates}
}

func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/system", h.get)
	router.GET("/system/version", h.version)
	router.POST("/system/update/check", h.checkUpdate)
	router.POST("/system/update/apply", h.applyUpdate)
	router.POST("/system/update/rollback", h.rollbackUpdate)
}

func (h *Handler) version(c *gin.Context) {
	response.Success(c, http.StatusOK, h.updates.Snapshot())
}

func (h *Handler) checkUpdate(c *gin.Context) {
	response.Success(c, http.StatusOK, h.updates.Check(c.Request.Context()))
}

// applyUpdate 的下载与替换不随请求超时或客户端断开而中止。
func (h *Handler) applyUpdate(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 15*time.Minute)
	defer cancel()
	result, err := h.updates.Apply(ctx)
	respondInstall(c, result, err, "updateFailed")
}

func (h *Handler) rollbackUpdate(c *gin.Context) {
	result, err := h.updates.Rollback()
	respondInstall(c, result, err, "rollbackFailed")
}

func respondInstall(c *gin.Context, result updatecheckapp.InstallResult, err error, failureCode string) {
	if err == nil {
		response.Success(c, http.StatusOK, result)
		return
	}
	for _, conflict := range []struct {
		err  error
		code string
	}{
		{updatecheckapp.ErrBusy, "updateInProgress"},
		{updatecheckapp.ErrNoUpdate, "updateNotAvailable"},
		{updatecheckapp.ErrBlocked, "updateBlocked"},
		{updatecheckapp.ErrNoBackup, "rollbackUnavailable"},
	} {
		if errors.Is(err, conflict.err) {
			response.Error(c, http.StatusConflict, conflict.code, err.Error())
			return
		}
	}
	response.Error(c, http.StatusInternalServerError, failureCode, err.Error())
}

func (h *Handler) get(c *gin.Context) {
	response.Success(c, http.StatusOK, gin.H{"publicApiBaseURL": strings.TrimRight(strings.TrimSpace(h.publicAPIBaseURL()), "/")})
}
