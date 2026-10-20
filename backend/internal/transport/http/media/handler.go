package media

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/pkg/mediafile"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/chenyme/grok2api/backend/internal/transport/http/params"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	service     *mediaapp.Service
	jobs        JobControl
	ingestSlots chan struct{}
}

// JobControl runs the job actions that need the gateway's billing and queue.
type JobControl interface {
	CancelMediaJob(ctx context.Context, id string) (mediadomain.Job, error)
	RetryMediaJob(ctx context.Context, id, idempotencyKey string) (mediadomain.Job, error)
}

func NewHandler(service *mediaapp.Service) *Handler {
	return &Handler{service: service, ingestSlots: make(chan struct{}, ingestConcurrency)}
}

// WithJobControl enables the cancel and retry job actions.
func (h *Handler) WithJobControl(jobs JobControl) *Handler {
	h.jobs = jobs
	return h
}

// RegisterPublic 注册使用不可猜测资源 ID 的公开图片读取与视频上传接收端点。
// 上传 PUT 不使用客户端 API key：xAI 无法携带，票据本身即授权。
func (h *Handler) RegisterPublic(router *gin.Engine) {
	router.GET("/v1/media/images/:assetId", h.getImage)
	router.HEAD("/v1/media/images/:assetId", h.getImage)
	router.GET("/v1/media/videos/:assetId", h.getVideo)
	router.HEAD("/v1/media/videos/:assetId", h.getVideo)
	router.PUT("/v1/media/uploads/:token", h.putVideoUpload)
}

// RegisterAdmin 注册管理端媒体列表和统计端点。
func (h *Handler) RegisterAdmin(router *gin.RouterGroup) {
	router.GET("/media/images", h.listImages)
	router.DELETE("/media/images", h.deleteImages)
	router.GET("/media/images/stats", h.imageStats)
	router.GET("/media/images/:assetId", h.getAsset)
	router.POST("/media/inputs/import", h.importInputImageFromURL)
	router.POST("/media/inputs/upload", h.uploadInputAsset)
	router.GET("/media/jobs", h.listJobs)
	router.DELETE("/media/jobs", h.deleteJobs)
	router.GET("/media/jobs/stats", h.jobStats)
	router.POST("/media/jobs/:jobId/cancel", h.cancelJob)
	router.POST("/media/jobs/:jobId/retry", h.retryJob)
}

type deleteImagesRequest struct {
	IDs []string `json:"ids" binding:"required"`
}

type deleteJobsRequest struct {
	IDs []string `json:"ids" binding:"required"`
}

func (h *Handler) getImage(c *gin.Context) {
	asset, body, err := h.service.OpenImage(c.Request.Context(), c.Param("assetId"))
	if errors.Is(err, mediaapp.ErrAssetNotFound) {
		c.Status(http.StatusNotFound)
		return
	}
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	defer body.Close()
	etag := `"` + asset.SHA256 + `"`
	if strings.TrimSpace(c.GetHeader("If-None-Match")) == etag {
		c.Header("ETag", etag)
		c.Status(http.StatusNotModified)
		return
	}
	c.Header("Content-Type", asset.MIMEType)
	c.Header("Content-Length", strconv.FormatInt(asset.SizeBytes, 10))
	c.Header("Cache-Control", "public, max-age=31536000, immutable")
	c.Header("ETag", etag)
	c.Header("X-Content-Type-Options", "nosniff")
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, body)
}

func (h *Handler) getVideo(c *gin.Context) {
	asset, body, err := h.service.OpenVideo(c.Request.Context(), c.Param("assetId"))
	if errors.Is(err, mediaapp.ErrAssetNotFound) {
		c.Status(http.StatusNotFound)
		return
	}
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	defer body.Close()
	seeker, ok := body.(io.ReadSeeker)
	if !ok {
		c.Status(http.StatusInternalServerError)
		return
	}
	c.Header("Content-Type", asset.MIMEType)
	c.Header("Content-Disposition", mediafile.VideoContentDisposition(asset.ID, asset.MIMEType))
	c.Header("Cache-Control", "public, max-age=31536000, immutable")
	c.Header("ETag", `"`+asset.SHA256+`"`)
	c.Header("X-Content-Type-Options", "nosniff")
	http.ServeContent(c.Writer, c.Request, asset.ID, asset.CreatedAt, seeker)
}

// putVideoUpload 接收 XAI ZDR 视频 PUT。响应与错误不得回显完整票据。
func (h *Handler) putVideoUpload(c *gin.Context) {
	_, err := h.service.ReceiveVideoUpload(c.Request.Context(), c.Param("token"), c.GetHeader("Content-Type"), c.Request.Body)
	switch {
	case err == nil:
		c.Status(http.StatusNoContent)
	case errors.Is(err, mediaapp.ErrUploadTicketNotFound):
		c.Status(http.StatusNotFound)
	case errors.Is(err, mediaapp.ErrUploadTicketExpired):
		c.Status(http.StatusGone)
	case errors.Is(err, mediaapp.ErrUploadTicketConsumed):
		c.Status(http.StatusConflict)
	case errors.Is(err, mediaapp.ErrVideoUploadTooLarge):
		// 体积超限优先于通用无效上传，返回 413。
		c.Status(http.StatusRequestEntityTooLarge)
	case errors.Is(err, mediaapp.ErrInvalidVideoUpload):
		c.Status(http.StatusBadRequest)
	case errors.Is(err, mediaapp.ErrUploadTicketsUnavailable):
		c.Status(http.StatusServiceUnavailable)
	default:
		c.Status(http.StatusInternalServerError)
	}
}

// assetListQuery 是图库来源筛选；from/to 为 RFC3339 时间，from 含边界、to 不含。
type assetListQuery struct {
	Kind        string    `form:"kind"`
	Provider    string    `form:"provider"`
	Model       string    `form:"model"`
	Operation   string    `form:"operation"`
	ClientKeyID uint64    `form:"clientKeyId"`
	AccountID   uint64    `form:"accountId"`
	JobID       string    `form:"jobId"`
	From        time.Time `form:"from" time_format:"2006-01-02T15:04:05Z07:00"`
	To          time.Time `form:"to" time_format:"2006-01-02T15:04:05Z07:00"`
}

func (h *Handler) listImages(c *gin.Context) {
	page, pageSize := params.Page(c)
	var query assetListQuery
	if c.ShouldBindQuery(&query) != nil {
		response.Error(c, http.StatusBadRequest, "invalidFilter", mediaapp.ErrInvalidFilter.Error())
		return
	}
	assets, total, err := h.service.AdminListImages(c.Request.Context(), page, pageSize, c.Query("search"), repository.MediaAssetListFilter{
		Kind: query.Kind, Provider: query.Provider, Model: query.Model, Operation: query.Operation,
		ClientKeyID: query.ClientKeyID, AccountID: query.AccountID, JobID: query.JobID, CreatedFrom: query.From, CreatedTo: query.To,
	})
	if errors.Is(err, mediaapp.ErrInvalidFilter) {
		response.Error(c, http.StatusBadRequest, "invalidFilter", err.Error())
		return
	}
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "mediaListImagesFailed", "读取图片列表失败")
		return
	}
	items := make([]mediaAssetDTO, 0, len(assets))
	for _, a := range assets {
		items = append(items, h.assetDTO(a))
	}
	response.Success(c, http.StatusOK, gin.H{"items": items, "page": page, "pageSize": pageSize, "total": total})
}

func (h *Handler) getAsset(c *gin.Context) {
	asset, err := h.service.AdminGetAsset(c.Request.Context(), c.Param("assetId"))
	if errors.Is(err, mediaapp.ErrAssetNotFound) {
		response.Error(c, http.StatusNotFound, "mediaAssetNotFound", err.Error())
		return
	}
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "mediaGetAssetFailed", "读取媒体资源失败")
		return
	}
	response.Success(c, http.StatusOK, h.assetDTO(asset))
}

func (h *Handler) assetDTO(a mediadomain.Asset) mediaAssetDTO {
	url := h.service.PublicImageURL(a.ID)
	if a.Kind == "video" {
		url = h.service.PublicVideoURL(a.ID)
	}
	o := a.Origin
	return mediaAssetDTO{
		ID: a.ID, Kind: a.Kind, MimeType: a.MIMEType, SizeBytes: a.SizeBytes,
		SHA256: a.SHA256, CreatedAt: a.CreatedAt.Format("2006-01-02T15:04:05Z"), URL: url,
		Provider: o.Provider, Model: o.Model, UpstreamModel: o.UpstreamModel,
		ClientKeyID: o.ClientKeyID, ClientKeyName: o.ClientKeyName, AccountID: o.AccountID, AccountName: o.AccountName,
		Operation: o.Operation, JobID: o.JobID, RequestID: o.RequestID, Prompt: o.Prompt,
	}
}

func (h *Handler) imageStats(c *gin.Context) {
	stats, err := h.service.AdminImageStats(c.Request.Context())
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "mediaImageStatsFailed", "读取图片统计失败")
		return
	}
	response.Success(c, http.StatusOK, imageStatsDTO{TotalImages: stats.TotalImages, TotalBytes: stats.TotalBytes})
}

func (h *Handler) deleteImages(c *gin.Context) {
	var request deleteImagesRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	deleted, err := h.service.AdminDeleteImages(c.Request.Context(), request.IDs)
	if errors.Is(err, mediaapp.ErrInvalidImageSelection) {
		response.Error(c, http.StatusBadRequest, "invalidImageSelection", err.Error())
		return
	}
	if errors.Is(err, mediaapp.ErrAssetInUse) {
		response.Error(c, http.StatusConflict, "mediaAssetInUse", err.Error())
		return
	}
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "mediaDeleteImagesFailed", "删除图片失败")
		return
	}
	response.Success(c, http.StatusOK, gin.H{"deleted": deleted})
}

func (h *Handler) listJobs(c *gin.Context) {
	page, pageSize := params.Page(c)
	filter := repository.MediaJobListFilter{Kind: c.Query("kind"), Status: c.Query("status"), Provider: c.Query("provider")}
	jobs, total, err := h.service.AdminListJobs(c.Request.Context(), page, pageSize, c.Query("search"), filter, repository.SortQuery{Field: c.Query("sortBy"), Direction: repository.SortDirection(c.Query("sortOrder"))})
	if errors.Is(err, mediaapp.ErrInvalidFilter) {
		response.Error(c, http.StatusBadRequest, "invalidFilter", err.Error())
		return
	}
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "mediaListJobsFailed", "读取媒体任务列表失败")
		return
	}
	items := make([]mediaJobDTO, 0, len(jobs))
	for _, job := range jobs {
		items = append(items, h.jobDTO(job))
	}
	response.Success(c, http.StatusOK, gin.H{"items": items, "page": page, "pageSize": pageSize, "total": total})
}

func (h *Handler) jobDTO(job mediadomain.Job) mediaJobDTO {
	var completedAt *string
	if job.CompletedAt != nil {
		formatted := job.CompletedAt.Format("2006-01-02T15:04:05Z")
		completedAt = &formatted
	}
	assetID, assetIDs, assetURLs := "", []string{}, []string{}
	if job.Kind == mediadomain.KindImage && job.ResultAssetID != "" {
		assetIDs, assetURLs = []string{job.ResultAssetID}, []string{h.service.PublicImageURL(job.ResultAssetID)}
	} else if job.Kind != mediadomain.KindImage && job.Status == mediadomain.StatusCompleted && job.ResultAssetID != "" {
		assetID = job.ResultAssetID
		assetIDs, assetURLs = []string{assetID}, []string{h.service.PublicVideoURL(assetID)}
	}
	return mediaJobDTO{
		ID: job.ID, Kind: string(job.Kind), Provider: job.Provider, Model: job.Model, Operation: string(job.Operation),
		Prompt: job.Prompt, Status: string(job.Status), Progress: job.Progress, Seconds: job.Seconds, Size: job.Size, Quality: job.Quality,
		AccountName: job.AccountName, ClientKeyName: job.ClientKeyName,
		CreatedAt: job.CreatedAt.Format("2006-01-02T15:04:05Z"), CompletedAt: completedAt,
		ErrorCode: job.ErrorCode, ErrorMessage: job.ErrorMessage, AssetID: assetID, AssetIDs: assetIDs, AssetURLs: assetURLs,
		Cancellable: job.Status == mediadomain.StatusQueued || (job.Kind == mediadomain.KindImage && job.Status == mediadomain.StatusInProgress && !job.Submitted),
		Retryable:   job.Status == mediadomain.StatusUncertain || gateway.ImageJobResumable(job),
	}
}

func (h *Handler) jobStats(c *gin.Context) {
	stats, err := h.service.AdminJobStats(c.Request.Context(), c.Query("kind"))
	if errors.Is(err, mediaapp.ErrInvalidFilter) {
		response.Error(c, http.StatusBadRequest, "invalidFilter", err.Error())
		return
	}
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "mediaJobStatsFailed", "读取媒体任务统计失败")
		return
	}
	response.Success(c, http.StatusOK, jobStatsDTO{
		TotalJobs: stats.TotalJobs, Queued: stats.Queued, InProgress: stats.InProgress, Completed: stats.Completed,
		Failed: stats.Failed, Canceled: stats.Canceled, Uncertain: stats.Uncertain,
	})
}

func (h *Handler) deleteJobs(c *gin.Context) {
	var request deleteJobsRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	deleted, err := h.service.AdminDeleteJobs(c.Request.Context(), request.IDs)
	if errors.Is(err, mediaapp.ErrInvalidJobSelection) || errors.Is(err, mediaapp.ErrActiveJobSelection) {
		response.Error(c, http.StatusBadRequest, "invalidJobSelection", err.Error())
		return
	}
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "mediaDeleteJobsFailed", "删除媒体任务失败")
		return
	}
	response.Success(c, http.StatusOK, gin.H{"deleted": deleted})
}

// cancelJob cancels a queued job, or an image job whose image in flight has
// not reached the upstream. An ended job is returned unchanged.
func (h *Handler) cancelJob(c *gin.Context) {
	if h.jobs == nil {
		response.Error(c, http.StatusServiceUnavailable, "mediaJobsUnavailable", mediaapp.ErrMediaJobsUnavailable.Error())
		return
	}
	job, err := h.jobs.CancelMediaJob(c.Request.Context(), strings.TrimSpace(c.Param("jobId")))
	if err != nil {
		writeJobActionError(c, err)
		return
	}
	response.Success(c, http.StatusOK, h.jobDTO(job))
}

// retryJob resubmits an uncertain job as a new job; the optional
// Idempotency-Key defaults to one retry per job. A failed image job whose
// image the upstream already drew is resumed in place (resumed: true).
func (h *Handler) retryJob(c *gin.Context) {
	if h.jobs == nil {
		response.Error(c, http.StatusServiceUnavailable, "mediaJobsUnavailable", mediaapp.ErrMediaJobsUnavailable.Error())
		return
	}
	key := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if len(key) > 160 {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "Idempotency-Key 不能超过 160 个字符")
		return
	}
	id := strings.TrimSpace(c.Param("jobId"))
	job, err := h.jobs.RetryMediaJob(c.Request.Context(), id, key)
	if err != nil {
		writeJobActionError(c, err)
		return
	}
	response.Success(c, http.StatusOK, retryJobDTO{mediaJobDTO: h.jobDTO(job), Resumed: job.ID == id})
}

func writeJobActionError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, gateway.ErrResponseNotFound):
		response.Error(c, http.StatusNotFound, "mediaJobNotFound", "媒体任务不存在")
	case errors.Is(err, gateway.ErrMediaJobNotCancellable):
		response.Error(c, http.StatusConflict, "mediaJobNotCancellable", err.Error())
	case errors.Is(err, gateway.ErrMediaJobNotRetryable):
		response.Error(c, http.StatusConflict, "mediaJobNotRetryable", err.Error())
	case errors.Is(err, gateway.ErrIdempotencyConflict):
		response.Error(c, http.StatusConflict, "idempotencyConflict", err.Error())
	default:
		response.Error(c, http.StatusBadGateway, "mediaJobActionFailed", err.Error())
	}
}
