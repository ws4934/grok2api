package account

import (
	"context"
	"net/http"
	"sync/atomic"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountsyncapp "github.com/chenyme/grok2api/backend/internal/application/accountsync"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

type buildConversionRequest struct {
	IDs      []string                           `json:"ids"`
	All      bool                               `json:"all"`
	Strategy accountapp.BuildConversionStrategy `json:"strategy"`
}

type webConsoleSyncRequest struct {
	IDs      []string                          `json:"ids"`
	All      bool                              `json:"all"`
	Strategy accountapp.WebConsoleSyncStrategy `json:"strategy"`
}

type buildConversionResponse struct {
	Created    int `json:"created"`
	Linked     int `json:"linked"`
	Skipped    int `json:"skipped"`
	Failed     int `json:"failed"`
	Synced     int `json:"synced"`
	SyncFailed int `json:"syncFailed"`
}

func (h *Handler) convertWebToBuild(c *gin.Context) {
	var request buildConversionRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "转换请求无效")
		return
	}
	if request.Strategy == "" {
		request.Strategy = accountapp.BuildConversionMissing
	}
	if request.Strategy != accountapp.BuildConversionAll && request.Strategy != accountapp.BuildConversionMissing {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "转换策略无效")
		return
	}
	ids, ok := h.webAccountSelection(c, request.All, request.IDs, "转换")
	if !ok {
		return
	}
	h.streamWebToBuildConversion(c, request.All, ids, request.Strategy)
}

func (h *Handler) syncWebToConsole(c *gin.Context) {
	var request webConsoleSyncRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "同步请求无效")
		return
	}
	if request.Strategy == "" {
		request.Strategy = accountapp.WebConsoleSyncAll
	}
	if request.Strategy != accountapp.WebConsoleSyncAll && request.Strategy != accountapp.WebConsoleSyncMissing {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "同步策略无效")
		return
	}
	ids, ok := h.webAccountSelection(c, request.All, request.IDs, "同步")
	if !ok {
		return
	}
	h.streamWebToConsoleSync(c, request.All, ids, request.Strategy)
}

// webAccountSelection reads the Grok Web accounts a bulk action targets:
// every one (all) or the listed IDs, never both. It writes the error response
// and reports false when the selection is invalid.
func (h *Handler) webAccountSelection(c *gin.Context, all bool, rawIDs []string, verb string) ([]uint64, bool) {
	if all && len(rawIDs) > 0 {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "全部"+verb+"与指定账号不能同时提交")
		return nil, false
	}
	if all {
		return nil, true
	}
	ids, err := parseIDs(rawIDs)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", err.Error())
		return nil, false
	}
	return ids, h.validateProviderIDs(c, ids, string(accountdomain.ProviderWeb))
}

func (h *Handler) runWebToConsoleSync(ctx context.Context, all bool, ids []uint64, strategy accountapp.WebConsoleSyncStrategy, progress accountapp.BatchProgressObserver, syncProgress func(completed, total int)) (accountapp.ImportResult, accountsyncapp.Result, error) {
	pipeline := h.startSyncPipeline(ctx, syncProgress)
	var (
		result accountapp.ImportResult
		err    error
	)
	if all {
		result, err = h.service.SyncAllWebAccountsToConsoleWithStrategy(pipeline.ctx, strategy, pipeline.Observe, progress)
	} else {
		result, err = h.service.SyncWebAccountsToConsoleWithStrategy(pipeline.ctx, ids, strategy, pipeline.Observe, progress)
	}
	syncResult := pipeline.Finish(err != nil)
	return result, syncResult, err
}

func (h *Handler) streamWebToConsoleSync(c *gin.Context, all bool, ids []uint64, strategy accountapp.WebConsoleSyncStrategy) {
	stream := newAccountEventStream(c)
	defer stream.Close()
	var total atomic.Int64
	result, syncResult, err := h.runWebToConsoleSync(c.Request.Context(), all, ids, strategy, stream.PhaseProgressObserver("importing", &total), stream.SyncProgressObserver())
	if err != nil {
		stream.WriteError("accountConsoleSyncFailed", "Grok Web 账号同步到 Console 失败")
		return
	}
	_ = stream.Write("complete", accountImportResponse{Created: result.Created, Updated: result.Updated, Skipped: result.Skipped, Synced: syncResult.Succeeded, SyncFailed: syncResult.Failed})
}

func (h *Handler) runWebToBuildConversion(ctx context.Context, all bool, ids []uint64, strategy accountapp.BuildConversionStrategy, progress accountapp.BatchProgressObserver, syncProgress func(completed, total int)) (accountapp.BuildConversionResult, accountsyncapp.Result, error) {
	pipeline := h.startSyncPipeline(ctx, syncProgress)
	var (
		result accountapp.BuildConversionResult
		err    error
	)
	if all {
		result, err = h.service.ConvertAllWebAccountsToBuildWithStrategy(pipeline.ctx, strategy, pipeline.Observe, progress)
	} else {
		result, err = h.service.ConvertWebAccountsToBuildWithStrategy(pipeline.ctx, ids, strategy, pipeline.Observe, progress)
	}
	syncResult := pipeline.Finish(err != nil)
	return result, syncResult, err
}

func (h *Handler) streamWebToBuildConversion(c *gin.Context, all bool, ids []uint64, strategy accountapp.BuildConversionStrategy) {
	stream := newAccountEventStream(c)
	defer stream.Close()
	var total atomic.Int64
	result, syncResult, err := h.runWebToBuildConversion(c.Request.Context(), all, ids, strategy, stream.PhaseProgressObserver("converting", &total), stream.SyncProgressObserver())
	if err != nil {
		stream.WriteError("accountConversionFailed", "Grok Web 账号转换失败")
		return
	}
	_ = stream.Write("complete", newBuildConversionResponse(result, syncResult))
}

func newBuildConversionResponse(result accountapp.BuildConversionResult, syncResult accountsyncapp.Result) buildConversionResponse {
	return buildConversionResponse{
		Created: result.Created, Linked: result.Linked, Skipped: result.Skipped, Failed: result.Failed,
		Synced: syncResult.Succeeded, SyncFailed: syncResult.Failed,
	}
}
