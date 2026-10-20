package account

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

type credentialExportRequest struct {
	IDs      []string `json:"ids" binding:"required"`
	Provider string   `json:"provider" binding:"required"`
}

type accountImportResponse struct {
	Created    int `json:"created"`
	Updated    int `json:"updated"`
	Skipped    int `json:"skipped"`
	Failed     int `json:"failed"`
	Synced     int `json:"synced"`
	SyncFailed int `json:"syncFailed"`
}

type authStartResponse struct {
	SessionID               string    `json:"sessionId"`
	Kind                    string    `json:"kind"`
	ExpiresAt               time.Time `json:"expiresAt"`
	VerificationURI         string    `json:"verificationUri,omitempty"`
	VerificationURIComplete string    `json:"verificationUriComplete,omitempty"`
	UserCode                string    `json:"userCode,omitempty"`
	IntervalSeconds         int       `json:"intervalSeconds,omitempty"`
	AuthorizeURL            string    `json:"authorizeUrl,omitempty"`
}

func (h *Handler) startAuthorization(c *gin.Context) {
	providerValue, ok := h.pathProvider(c)
	if !ok {
		return
	}
	adminID, ok := currentAdminID(c)
	if !ok {
		return
	}
	value, err := h.service.StartAuthorization(c.Request.Context(), providerValue, adminID)
	if err != nil {
		h.writeServiceError(c, "authorizationStartFailed", err, http.StatusBadGateway, "启动账号授权失败")
		return
	}
	response.Success(c, http.StatusCreated, authStartResponse{
		SessionID: value.SessionID, Kind: value.Kind, ExpiresAt: value.ExpiresAt,
		VerificationURI: value.VerificationURI, VerificationURIComplete: value.VerificationURIComplete, UserCode: value.UserCode,
		IntervalSeconds: int(value.Interval.Seconds()), AuthorizeURL: value.AuthorizeURL,
	})
}

// completeAuthorization polls a device login (empty body) or finishes a redirect login with {callbackUrl}.
func (h *Handler) completeAuthorization(c *gin.Context) {
	providerValue, ok := h.pathProvider(c)
	if !ok {
		return
	}
	adminID, ok := currentAdminID(c)
	if !ok {
		return
	}
	var request struct {
		CallbackURL string `json:"callbackUrl"`
	}
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, maxAuthCompleteBodyBytes+1))
	if err != nil || len(raw) > maxAuthCompleteBodyBytes {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	if len(bytes.TrimSpace(raw)) > 0 && json.Unmarshal(raw, &request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	value, err := h.service.CompleteAuthorization(c.Request.Context(), providerValue, c.Param("sessionId"), adminID, strings.TrimSpace(request.CallbackURL))
	switch {
	case errors.Is(err, accountapp.ErrAuthPending):
		response.Success(c, http.StatusAccepted, gin.H{"status": "pending"})
		return
	case errors.Is(err, accountapp.ErrAuthSlowDown):
		response.Error(c, http.StatusTooManyRequests, "devicePollTooFast", "轮询过快，请稍后重试")
		return
	case errors.Is(err, accountapp.ErrAuthExpired):
		response.Error(c, http.StatusGone, "deviceLoginExpired", "账号授权已拒绝或过期")
		return
	case errors.Is(err, accountapp.ErrInvalidInput):
		response.Error(c, http.StatusBadRequest, "invalidRequest", err.Error())
		return
	case err != nil:
		h.writeServiceError(c, "deviceLoginFailed", err, http.StatusBadGateway, "账号授权失败")
		return
	}
	syncResult := h.syncInitial(c.Request.Context(), value.Credential.ID)
	if refreshed, refreshErr := h.service.Get(c.Request.Context(), value.Credential.ID); refreshErr == nil {
		value = refreshed
	}
	response.Success(c, http.StatusOK, gin.H{"status": "completed", "account": newAccountResponse(value), "synced": syncResult.Succeeded, "syncFailed": syncResult.Failed})
}

func (h *Handler) importAuth(c *gin.Context) {
	providerValue, ok := h.pathProvider(c)
	if !ok {
		return
	}
	definition, _ := h.service.ProviderDefinition(providerValue)
	if !definition.Credential.Import {
		response.Error(c, http.StatusConflict, "accountOperationUnsupported", "该账号来源不支持导入")
		return
	}
	documents, ok := readAccountImportDocuments(c, definition.DisplayName()+" 账号凭据（JSON、逐行 JSON 或文本）")
	if !ok {
		return
	}
	stream := newAccountEventStream(c)
	defer stream.Close()
	var total atomic.Int64
	pipeline := h.startSyncPipeline(c.Request.Context(), stream.SyncProgressObserver())
	result, err := h.service.ImportCredentialDocumentsWithProgress(pipeline.ctx, providerValue, documents, pipeline.Observe, stream.PhaseProgressObserver("importing", &total))
	syncResult := pipeline.Finish(err != nil)
	if err != nil {
		stream.WriteError("authImportFailed", "导入账号失败")
		return
	}
	_ = stream.Write("complete", accountImportResponse{Created: result.Created, Updated: result.Updated, Skipped: result.Skipped, Failed: result.Failed, Synced: syncResult.Succeeded, SyncFailed: syncResult.Failed})
}

func readAccountImportDocuments(c *gin.Context, fileDescription string) ([][]byte, bool) {
	form, err := c.MultipartForm()
	if err != nil {
		var sizeError *http.MaxBytesError
		if errors.As(err, &sizeError) {
			response.Error(c, http.StatusRequestEntityTooLarge, "accountImportFileTooLarge", "账号凭据文件总大小不能超过 30 MiB")
			return nil, false
		}
		response.Error(c, http.StatusBadRequest, "invalidAuthFile", "请选择有效的"+fileDescription)
		return nil, false
	}
	defer form.RemoveAll()
	files := append(form.File["files"], form.File["file"]...)
	if len(files) == 0 {
		response.Error(c, http.StatusBadRequest, "invalidAuthFile", "请选择有效的"+fileDescription)
		return nil, false
	}
	if len(files) > maxAccountImportFiles {
		response.Error(c, http.StatusBadRequest, "invalidAuthFile", "单次最多选择 1000 个账号文件")
		return nil, false
	}
	documents := make([][]byte, 0, len(files))
	totalBytes := int64(0)
	for _, file := range files {
		if file.Size < 0 || totalBytes+file.Size > maxAccountImportBytes {
			response.Error(c, http.StatusRequestEntityTooLarge, "accountImportFileTooLarge", "账号凭据文件总大小不能超过 30 MiB")
			return nil, false
		}
		opened, openErr := file.Open()
		if openErr != nil {
			response.Error(c, http.StatusBadRequest, "invalidAuthFile", "无法读取"+fileDescription)
			return nil, false
		}
		data, readErr := io.ReadAll(io.LimitReader(opened, maxAccountImportBytes-totalBytes+1))
		_ = opened.Close()
		if readErr != nil {
			response.Error(c, http.StatusBadRequest, "invalidAuthFile", "无法读取"+fileDescription)
			return nil, false
		}
		totalBytes += int64(len(data))
		if totalBytes > maxAccountImportBytes {
			response.Error(c, http.StatusRequestEntityTooLarge, "accountImportFileTooLarge", "账号凭据文件总大小不能超过 30 MiB")
			return nil, false
		}
		documents = append(documents, data)
	}
	return documents, true
}

func (h *Handler) exportCredentials(c *gin.Context) {
	providerValue := accountdomain.Provider(c.Query("provider"))
	if limitText, pagedExport := c.GetQuery("limit"); pagedExport {
		if _, usesOffset := c.GetQuery("offset"); usesOffset {
			response.Error(c, http.StatusBadRequest, "accountExportFailed", "分批导出不支持 offset，请使用服务端返回的 afterId")
			return
		}
		limit, err := strconv.Atoi(strings.TrimSpace(limitText))
		if err != nil {
			response.Error(c, http.StatusBadRequest, "accountExportFailed", "导出数量必须为整数")
			return
		}
		afterID, err := strconv.ParseUint(strings.TrimSpace(c.DefaultQuery("afterId", "0")), 10, 63)
		if err != nil {
			response.Error(c, http.StatusBadRequest, "accountExportFailed", "导出游标必须为非负整数")
			return
		}
		snapshotMaxID, err := strconv.ParseUint(strings.TrimSpace(c.DefaultQuery("snapshotMaxId", "0")), 10, 63)
		if err != nil {
			response.Error(c, http.StatusBadRequest, "accountExportFailed", "导出快照上界必须为非负整数")
			return
		}
		if !h.registeredProvider(c, providerValue) {
			return
		}
		result, exportErr := h.service.ExportProviderCredentialsCursor(c.Request.Context(), providerValue, afterID, snapshotMaxID, limit)
		if exportErr != nil {
			h.writeServiceError(c, "accountExportFailed", exportErr, http.StatusInternalServerError, "导出账号失败")
			return
		}
		c.Header("X-Export-Next-ID", strconv.FormatUint(result.NextID, 10))
		c.Header("X-Export-Snapshot-Max-ID", strconv.FormatUint(result.SnapshotMaxID, 10))
		c.Header("X-Export-Has-More", strconv.FormatBool(result.HasMore))
		h.writeCredentialExport(c, providerValue, result.ExportResult)
		return
	}
	if !h.registeredProvider(c, providerValue) {
		return
	}
	result, err := h.service.ExportProviderCredentials(c.Request.Context(), providerValue)
	if err != nil {
		h.writeServiceError(c, "accountExportFailed", err, http.StatusInternalServerError, "导出账号失败")
		return
	}
	h.writeCredentialExport(c, providerValue, result)
}

func (h *Handler) exportSelectedCredentials(c *gin.Context) {
	var request credentialExportRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	ids, err := parseIDs(request.IDs)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", err.Error())
		return
	}
	providerValue := accountdomain.Provider(request.Provider)
	if !h.registeredProvider(c, providerValue) {
		return
	}
	result, err := h.service.ExportProviderCredentialsByIDs(c.Request.Context(), providerValue, ids)
	if err != nil {
		h.writeServiceError(c, "accountExportFailed", err, http.StatusInternalServerError, "导出账号失败")
		return
	}
	h.writeCredentialExport(c, providerValue, result)
}

func (h *Handler) writeCredentialExport(c *gin.Context, providerValue accountdomain.Provider, result accountapp.ExportResult) {
	filename := "grok2api-" + string(providerValue) + "-accounts-" + time.Now().UTC().Format("20060102T150405Z") + ".json"
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	c.Header("Access-Control-Expose-Headers", "Content-Disposition, X-Exported-Accounts, X-Export-Next-ID, X-Export-Snapshot-Max-ID, X-Export-Has-More")
	c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("X-Exported-Accounts", strconv.Itoa(result.Count))
	c.Data(http.StatusOK, "application/json; charset=utf-8", result.Data)
}
