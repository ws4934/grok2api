package channel

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
)

type ImageJob struct {
	ID              string           `json:"id"`
	ClientKeyID     uint64           `json:"-"`
	Provider        account.Provider `json:"provider"`
	ModelRouteID    uint64           `json:"-"`
	BoundAccountIDs []uint64         `json:"-"`
	Request         ImageRequest     `json:"request"`
	Price           Price            `json:"-"`
	Status          string           `json:"status"`
	Items           []ImageItem      `json:"items"`
	CreatedAt       time.Time        `json:"created_at"`
	StartedAt       *time.Time       `json:"started_at,omitempty"`
	ExpiresAt       time.Time        `json:"expires_at"`
	IdempotencyKey  string           `json:"-"`
	PayloadHash     string           `json:"-"`
}
type ImageItem struct {
	ID         string     `json:"id"`
	JobID      string     `json:"-"`
	Position   int        `json:"index"`
	AccountID  uint64     `json:"-"`
	Status     string     `json:"status"`
	Stage      string     `json:"stage"`
	Submitted  bool       `json:"-"`
	Checkpoint Checkpoint `json:"-"`
	AssetIDs   []string   `json:"asset_ids"`
	Width      int        `json:"width,omitempty"`
	Height     int        `json:"height,omitempty"`
	MIME       string     `json:"mime_type,omitempty"`
	Error      string     `json:"error,omitempty"`
	ClaimToken string     `json:"-"`
	Generation uint64     `json:"-"`
	LeaseUntil *time.Time `json:"-"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
}
type ImageOutputValidator interface {
	ValidateImageOutput(Blob, ImageRequest) (Blob, error)
}
type JobRepository interface {
	AcquireChannelSlot(context.Context, uint64, string, time.Time) error
	ReleaseChannelSlot(context.Context, string) error
	FindImageJob(context.Context, uint64, string) (ImageJob, bool, error)
	CreateImageJob(context.Context, ImageJob) (ImageJob, bool, error)
	GetImageJob(context.Context, string, uint64) (ImageJob, error)
	ListImageJobs(context.Context, uint64, account.Provider, int, int) ([]ImageJob, int64, error)
	CancelImageJob(context.Context, string, uint64) error
	ClaimImageItem(context.Context, account.Provider, time.Time, time.Time, string) (ImageJob, ImageItem, error)
	AssignImageAccount(context.Context, ImageItem, uint64) error
	RenewImageItem(context.Context, ImageItem, time.Time) error
	SaveImageCheckpoint(context.Context, ImageItem, Checkpoint, string) error
	CompleteImageItem(context.Context, ImageItem, []string, string, string, *audit.Record) error
	ReserveChannelQuota(context.Context, uint64, string, []QuotaNeed) error
	FinishChannelQuota(context.Context, string, bool) error
	MarkChannelQuotaSent(context.Context, string, string) error
	CleanupChannelData(context.Context, time.Time) error
}
