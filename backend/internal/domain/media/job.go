package media

import (
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
)

type Status string

const (
	StatusQueued     Status = "queued"
	StatusInProgress Status = "in_progress"
	StatusCompleted  Status = "completed"
	StatusFailed     Status = "failed"
	StatusCanceled   Status = "canceled"
	// StatusUncertain means a request was sent upstream but no upstream ID was
	// recorded, so it can neither be resumed nor safely resubmitted.
	StatusUncertain Status = "uncertain"
)

// Terminal reports whether no worker will touch the job again.
func (s Status) Terminal() bool {
	return s != StatusQueued && s != StatusInProgress
}

// Kind separates the job executors.
type Kind string

const (
	KindVideo Kind = "video"
	KindImage Kind = "image"
)

// MaxInputJSONBytes is the persisted media_jobs.input_json ceiling (32 MiB).
// Keep the relational CHECK constraint and gateway encode guard aligned with this value.
const MaxInputJSONBytes = 32 << 20

// MaxInputImages is the maximum number of reference images accepted for a video job.
const MaxInputImages = 8

// MaxReferenceVideos is the maximum number of reference videos accepted for a video job.
const MaxReferenceVideos = 10

// MaxInputAssetBytes limits each temporary image or video input to 20 MiB.
const MaxInputAssetBytes = 20 << 20

// MaxImageJobItems bounds the images of one image job (media_jobs.item_count).
const MaxImageJobItems = 10

// VideoOperation is the job operation; image jobs use generate and edit.
type VideoOperation string

const (
	VideoOperationGenerate VideoOperation = "generate"
	VideoOperationEdit     VideoOperation = "edit"
	VideoOperationExtend   VideoOperation = "extend"
)

// ImageParams are the immutable options of an image job.
type ImageParams struct {
	OutputFormat string `json:"output_format,omitempty"`
	Background   string `json:"background,omitempty"`
	// ResponseModel and ResponseBackground echo the Responses request that created the job.
	ResponseModel      string `json:"response_model,omitempty"`
	ResponseBackground bool   `json:"response_background,omitempty"`
}

// Job 表示可跨进程重启恢复的异步媒体任务（视频或图片）。
type Job struct {
	ID              string
	Kind            Kind
	RequestID       string
	ClientKeyID     uint64
	ClientKeyName   string
	ClientIP        string
	AccountID       uint64
	AccountName     string
	EgressNodeID    *uint64
	EgressNodeName  string
	EgressScope     string
	EgressMode      string
	Provider        string
	Model           string
	ModelRouteID    uint64
	UpstreamModel   string
	Operation       VideoOperation
	Prompt          string
	Seconds         int
	Size            string
	Quality         string
	Status          Status
	Progress        int
	InputJSON       string
	InputImageCount int
	UpstreamURL     string
	// ResultAssetID 指向本地媒体资产；XAI ZDR 上传完成后优先从此读取。
	ResultAssetID string
	// ResultAssetIDs are the delivered images of an image job, in order.
	ResultAssetIDs []string
	// ItemCount is the number of images an image job produces (1 for video).
	ItemCount int
	// Checkpoint is provider-owned JSON of the item in flight, saved before
	// and after irreversible upstream sends so recovery resumes instead of resubmitting.
	Checkpoint string
	// Submitted marks that the item in flight may have reached the upstream.
	Submitted bool
	Params    ImageParams
	// IdempotencyKey and PayloadHash deduplicate client retries per client key.
	IdempotencyKey string
	PayloadHash    string
	// Price is the quote reserved at submission for the whole job; settlement
	// bills from it. Nil on jobs created before price snapshots.
	Price *audit.PricingResult
	// Generation counts claims; fenced writes require the current one.
	Generation      int
	ContentType     string
	ErrorCode       string
	ErrorMessage    string
	LeaseUntil      *time.Time
	ClaimToken      string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	CompletedAt     *time.Time
	UsageRecordedAt *time.Time

	// ResumeItem is the image the latest administrator resume re-fetches and
	// ResumeGeneration the generation it resumed from (0: never resumed).
	ResumeItem       int
	ResumeGeneration int
}
