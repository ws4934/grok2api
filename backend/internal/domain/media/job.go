package media

import (
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/pkg/requestparams"
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

// MaxInputImages is the input-image ceiling for image jobs and Build video references.
const MaxInputImages = 8

// MaxVideoReferenceImages admits the largest supported video reference set.
const MaxVideoReferenceImages = 14

// MaxVideoInputImages includes a pinned first frame alongside all references.
const MaxVideoInputImages = MaxVideoReferenceImages + 1

// MaxReferenceVideos is the maximum number of reference videos accepted for a video job.
const MaxReferenceVideos = 10

// MaxInputAssetBytes limits each temporary image or video input to 20 MiB.
const MaxInputAssetBytes = 20 << 20

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
	ID                string
	Kind              Kind
	RequestID         string
	ClientKeyID       uint64
	ClientKeyName     string
	ClientIP          string
	AccountID         uint64
	AccountName       string
	EgressNodeID      *uint64
	EgressNodeName    string
	EgressScope       string
	EgressMode        string
	Provider          string
	Model             string
	ModelRouteID      uint64
	UpstreamModel     string
	Operation         VideoOperation
	Prompt            string
	Seconds           int
	Size              string
	Quality           string
	Status            Status
	Progress          int
	InputJSON         string
	InputImageCount   int
	RequestParameters *requestparams.Parameters
	UpstreamURL       string
	// ResultAssetID is the job's result in local media storage: the video, or
	// the image once delivered.
	ResultAssetID string
	// Checkpoint is provider-owned JSON of the upstream task in flight, saved before
	// and after irreversible upstream sends so recovery resumes instead of resubmitting.
	Checkpoint string
	// Submitted marks that the task in flight may have reached the upstream.
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

	// ResumeGeneration is the generation the latest administrator resume of an
	// image job started from (0: never resumed).
	ResumeGeneration int
}

// UsagePending keeps terminal jobs available for durable audit settlement.
// A canceled video was never submitted and has no billable outcome.
func (j Job) UsagePending() bool {
	return j.Status.Terminal() && j.UsageRecordedAt == nil && !(j.Status == StatusCanceled && j.Kind != KindImage)
}
