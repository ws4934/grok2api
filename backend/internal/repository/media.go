package repository

import (
	"context"
	"io"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
)

// MediaAssetListFilter 表示图库按来源筛选的条件；零值字段不参与筛选。
type MediaAssetListFilter struct {
	// Kind 为空时按 image 处理，保持图库默认只列图片。
	Kind        string
	Provider    string
	Model       string
	Operation   string
	ClientKeyID uint64
	AccountID   uint64
	JobID       string
	// CreatedFrom 含边界，CreatedTo 不含边界。
	CreatedFrom time.Time
	CreatedTo   time.Time
}

// MediaAssetListQuery 表示管理端媒体资源列表的查询条件。
type MediaAssetListQuery struct {
	Page   PageQuery
	Filter MediaAssetListFilter
}

// MediaJobListFilter 表示媒体任务列表允许使用的业务筛选条件。
type MediaJobListFilter struct {
	Kind     string
	Status   string
	Provider string
}

// MediaJobListQuery 表示管理端媒体任务列表的查询条件。
type MediaJobListQuery struct {
	Page   PageQuery
	Filter MediaJobListFilter
}

// MediaAssetStats 表示媒体资源的聚合统计结果。
type MediaAssetStats struct {
	TotalImages int64
	TotalBytes  int64
}

// MediaUploadTicket 表示上游视频 PUT 的一次性票据元数据（不含明文 token）。
type MediaUploadTicket struct {
	TokenHash   string
	AssetID     string
	JobID       string
	MaxBytes    int64
	AllowedMIME string
	ExpiresAt   time.Time
	ConsumedAt  *time.Time
	CreatedAt   time.Time
}

// MediaJobStats 表示各状态媒体任务的聚合统计结果。
type MediaJobStats struct {
	TotalJobs  int64
	Completed  int64
	Failed     int64
	InProgress int64
	Queued     int64
	Canceled   int64
	Uncertain  int64
}

type MediaJobRepository interface {
	CreateMediaJob(ctx context.Context, value media.Job) error
	GetMediaJob(ctx context.Context, id string, clientKeyID uint64) (media.Job, error)
	GetMediaJobsByIDs(ctx context.Context, ids []string) ([]media.Job, error)
	UpdateMediaJob(ctx context.Context, value media.Job) error
	DeleteMediaJob(ctx context.Context, id string) error
	ListMediaJobs(ctx context.Context, query MediaJobListQuery) ([]media.Job, int64, error)
	// SummarizeMediaJobs counts jobs per status; an empty kind counts every kind.
	SummarizeMediaJobs(ctx context.Context, kind string) (MediaJobStats, error)
	ListRecoverableMediaJobs(ctx context.Context, limit int) ([]media.Job, error)
	ListUnrecordedTerminalMediaJobs(ctx context.Context, limit int) ([]media.Job, error)
	// TryClaimMediaJob takes a queued or lease-expired job and bumps its generation.
	TryClaimMediaJob(ctx context.Context, id string, now, leaseUntil time.Time, claimToken string) (media.Job, bool, error)
	MarkMediaJobUsageRecorded(ctx context.Context, id string, recordedAt time.Time) error
	GetMediaJobByIdempotencyKey(ctx context.Context, clientKeyID uint64, key string) (media.Job, error)
	// GetMediaJobInput reads the input payload the other reads omit.
	GetMediaJobInput(ctx context.Context, id string) (string, error)
	// RenewMediaJobLease extends the claim held by job; ErrNotFound means it was lost.
	RenewMediaJobLease(ctx context.Context, job media.Job, leaseUntil time.Time) error
	// CancelMediaJob cancels a job that has sent nothing irreversible; clientKeyID 0
	// matches any key. ErrConflict means it is running past that point.
	CancelMediaJob(ctx context.Context, id string, clientKeyID uint64, now time.Time) (media.Job, error)
	// ResumeMediaJob returns a failed, submitted image job read at generation to
	// the workers with its checkpoint, recording item and generation as the
	// resume. ErrConflict (with the current job) means it changed since.
	ResumeMediaJob(ctx context.Context, id string, generation, item int, now time.Time) (media.Job, error)
}

// MediaAssetRepository 定义媒体资源元数据持久化能力。
type MediaAssetRepository interface {
	CreateMediaAsset(ctx context.Context, value media.Asset) error
	GetMediaAsset(ctx context.Context, id string) (media.Asset, error)
	ListMediaAssets(ctx context.Context, query MediaAssetListQuery) ([]media.Asset, int64, error)
	SummarizeMediaAssets(ctx context.Context) (MediaAssetStats, error)
	TotalMediaAssetBytes(ctx context.Context) (int64, error)
	// ListOldestMediaAssets 按 created_at ASC, id ASC 分页；offset 用于跳过已扫描的受保护资产。
	ListOldestMediaAssets(ctx context.Context, offset, limit int) ([]media.Asset, error)
	// ListExpiredMediaAssets 按过期时间稳定分页返回已过期临时输入，持久资产永不包含在结果中。
	ListExpiredMediaAssets(ctx context.Context, before time.Time, offset, limit int) ([]media.Asset, error)
	// ExpireMediaInputIfUnreferenced 在没有活动任务引用时原子地将临时输入标记为过期。
	ExpireMediaInputIfUnreferenced(ctx context.Context, id string, expiresAt time.Time) (bool, error)
	DeleteMediaAsset(ctx context.Context, id string) error
	// ListProtectedMediaAssetIDs 返回活动任务或未消费票据绑定的资产 ID，清理时必须跳过。
	ListProtectedMediaAssetIDs(ctx context.Context) (map[string]struct{}, error)
}

// MediaUploadTicketRepository 定义视频上传票据的持久化能力。
type MediaUploadTicketRepository interface {
	CreateUploadTicket(ctx context.Context, ticket MediaUploadTicket) error
	GetUploadTicketByHash(ctx context.Context, tokenHash string) (MediaUploadTicket, error)
	// ConsumeUploadTicket 原子消费票据；已消费或过期返回 false。
	ConsumeUploadTicket(ctx context.Context, tokenHash string, now time.Time) (MediaUploadTicket, bool, error)
	// ReleaseUploadTicket 在尚未登记资产时撤销消费，允许同票据重试。
	// 仅当票据当前为已消费状态时清除 consumed_at；返回是否成功释放。
	ReleaseUploadTicket(ctx context.Context, tokenHash string) (bool, error)
	// DeleteUploadTicketByHash 按 token_hash 精确删除票据；行不存在时幂等成功。
	// 用于签发过程中 bind 失败后的补偿回滚，不得按 job/asset 批量删除。
	DeleteUploadTicketByHash(ctx context.Context, tokenHash string) error
	// DeleteUploadTicketsByJobID 撤销指定任务尚存的上传入口；行不存在时幂等成功。
	DeleteUploadTicketsByJobID(ctx context.Context, jobID string) error
	DeleteExpiredUploadTickets(ctx context.Context, before time.Time, limit int) (int64, error)
	BindJobResultAsset(ctx context.Context, jobID, assetID string) error
}

// MediaObjectStorage 定义媒体二进制对象的存取边界。
type MediaObjectStorage interface {
	SaveImage(ctx context.Context, id, mimeType string, data []byte) (string, error)
	SaveVideo(ctx context.Context, id, mimeType string, data []byte) (string, error)
	BeginVideoUpload(ctx context.Context, id, mimeType string) (tempPath, storageKey string, err error)
	CommitVideoUpload(ctx context.Context, tempPath, storageKey string) error
	AbortVideoUpload(ctx context.Context, tempPath string) error
	Open(ctx context.Context, storageKey string) (io.ReadCloser, error)
	Delete(ctx context.Context, storageKey string) error
}
