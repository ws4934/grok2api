package provider

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
)

// VideoStage identifies which phase of an asynchronous video job failed.
type VideoStage string

const (
	// VideoStagePrepare is local work performed before the create request is sent.
	// It is not account-failover eligible because retrying deterministic local
	// validation or configuration failures against another credential is useless.
	VideoStagePrepare VideoStage = "prepare"
	// VideoStageCreate means the upstream explicitly rejected the create request.
	// Account failover is safe only for the retryable 4xx statuses selected by
	// the gateway; 5xx responses remain indeterminate because work may already
	// have been accepted before the server failed.
	VideoStageCreate VideoStage = "create"
	// VideoStageSubmitted means the create request may have reached upstream but
	// no usable job identifier was obtained. Retrying could duplicate work.
	VideoStageSubmitted VideoStage = "submitted"
	VideoStagePoll      VideoStage = "poll"
)

// VideoStageError records the asynchronous video phase without treating every
// create-path error as safe for account failover.
type VideoStageError struct {
	Stage  VideoStage
	Status int
	Err    error
}

func (e *VideoStageError) Error() string {
	if e == nil {
		return "video request failed"
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	if e.Status > 0 {
		return fmt.Sprintf("video %s failed with status %d", e.Stage, e.Status)
	}
	return fmt.Sprintf("video %s failed", e.Stage)
}

func (e *VideoStageError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *VideoStageError) HTTPStatusCode() int {
	if e == nil {
		return 0
	}
	if e.Status > 0 {
		return e.Status
	}
	return ErrorHTTPStatusOrZero(e.Err)
}

// VideoErrorStage reports the video phase for an error chain.
func VideoErrorStage(err error) (VideoStage, bool) {
	var stageErr *VideoStageError
	if !errors.As(err, &stageErr) || stageErr == nil || stageErr.Stage == "" {
		return "", false
	}
	return stageErr.Stage, true
}

// VideoCreateFailureStage distinguishes an explicit upstream rejection from an
// indeterminate POST result. Explicit 4xx responses (including the 401
// sentinel) are rejections; transport errors and 5xx responses remain
// submitted because the upstream may already have accepted the job.
func VideoCreateFailureStage(err error) VideoStage {
	if errors.Is(err, ErrUnauthorized) {
		return VideoStageCreate
	}
	if status, ok := ErrorHTTPStatus(err); ok && status >= http.StatusBadRequest && status < http.StatusInternalServerError {
		return VideoStageCreate
	}
	return VideoStageSubmitted
}

// VideoPollTransientLimit is the number of consecutive transient status poll
// failures that ends a submitted job: it already runs upstream and has spent
// quota, so one network blip must not lose it.
const VideoPollTransientLimit = 5

// IsTransientVideoPollError reports whether a failed status poll may succeed
// on the next attempt: a transport error (no upstream status), 429 or 5xx.
func IsTransientVideoPollError(err error) bool {
	if status, ok := ErrorHTTPStatus(err); ok {
		return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
	}
	return !errors.Is(err, ErrUnauthorized)
}

// WrapVideoStage annotates err with the video phase and optional HTTP status.
func WrapVideoStage(stage VideoStage, status int, err error) error {
	if err == nil {
		return nil
	}
	var existing *VideoStageError
	if errors.As(err, &existing) {
		return err
	}
	if status <= 0 {
		status = ErrorHTTPStatusOrZero(err)
	}
	return &VideoStageError{Stage: stage, Status: status, Err: err}
}

// VideoOperation selects the official xAI video endpoint family.
type VideoOperation = media.VideoOperation

const (
	VideoOperationGenerate = media.VideoOperationGenerate
	VideoOperationEdit     = media.VideoOperationEdit
	VideoOperationExtend   = media.VideoOperationExtend
)

// ConsoleVideoMaxReferenceImages and ConsoleVideoMaxReferenceDurationSeconds
// describe the Console reference-to-video contract enforced by the upstream.
// They are shared by admission control and the Console adapter so invalid
// asynchronous jobs are rejected before enqueueing without weakening the
// adapter's final request-boundary validation.
const (
	ConsoleVideoMaxReferenceImages          = 7
	ConsoleVideoMaxReferenceDurationSeconds = 10
)

// VideoRequestOptions are the video parameters a route must accept before a job is queued.
// Zero Duration, AspectRatio and Resolution mean the client left them out.
type VideoRequestOptions struct {
	Operation       VideoOperation
	UpstreamModel   string
	AspectRatio     string
	Resolution      string
	HasImage        bool
	HasLastFrame    bool
	ReferenceImages int
	ReferenceVideos int
	ReferenceAudios int
	Duration        int
}

// VideoRequestValidator rejects video requests a route cannot serve, so an
// asynchronous job never queues a request the upstream must fail. Errors wrap
// ErrVideoParameterInvalid or ErrVideoOperationUnsupported. Providers without
// it accept every generation and no edit or extension.
type VideoRequestValidator interface {
	Adapter
	ValidateVideoRequest(options VideoRequestOptions) error
}

// VideoRequestNormalizer picks the duration, aspect ratio and resolution a
// route generates with: defaults for the values left out, and the provider's
// rules for the rest. Providers without it get the options unchanged.
type VideoRequestNormalizer interface {
	Adapter
	NormalizeVideoRequest(options VideoRequestOptions) (VideoRequestOptions, error)
}

// VideoQuotaModeAdapter refines a video route's quota mode by the requested resolution.
type VideoQuotaModeAdapter interface {
	Adapter
	VideoQuotaMode(quotaMode, resolution string) string
}

// ValidateGrokVideoResolution enforces the Grok Imagine resolution tiers: only
// grok-imagine-video-1.5 renders 1080p, and reference images cap it at 720p.
func ValidateGrokVideoResolution(options VideoRequestOptions) error {
	if !strings.EqualFold(strings.TrimSpace(options.Resolution), "1080p") {
		return nil
	}
	if strings.TrimSpace(options.UpstreamModel) != "grok-imagine-video-1.5" {
		return fmt.Errorf("%w: %s 不支持 1080p", ErrVideoOperationUnsupported, options.UpstreamModel)
	}
	if options.ReferenceImages > 0 {
		return fmt.Errorf("%w: reference_images 模式最高支持 720p", ErrVideoOperationUnsupported)
	}
	return nil
}

type VideoRequest struct {
	Credential account.Credential
	// Billing is used only to determine XAI eligibility in Build auto mode; nil means the account tier is unknown.
	Billing *account.Billing
	// JobID binds the local video job to XAI ZDR upload tickets and result assets.
	JobID string
	// Model is the selected upstream video model when the Provider supports more than one.
	Model string
	// Operation defaults to generate when empty.
	Operation   VideoOperation
	Prompt      string
	Duration    int
	AspectRatio string
	Resolution  string
	// ImageURL is the optional first-frame image (official "image" field).
	ImageURL string
	// LastFrameURL is the optional last-frame image; it requires ImageURL.
	LastFrameURL string
	// ReferenceVideoURLs are optional reference videos (data URIs or https URLs).
	ReferenceVideoURLs []string
	// ReferenceURLs are style/content references (official "reference_images").
	// A single reference must stay in reference_images and must not be coerced to image.
	// Official docs forbid combining image with reference_images.
	ReferenceURLs []string
	// ReferenceAudios are preset voice_ids for reference-to-video (official "reference_audios").
	// At most 3 entries; may be used alone or with reference_images.
	ReferenceAudios []string
	// VideoURL is required for edit/extend (official "video" field).
	VideoURL string
	Progress func(int)
	// Checkpoint is the provider state an earlier run of this job saved with
	// SaveCheckpoint. A provider that persists its upstream job ID polls that
	// job on recovery instead of creating another one.
	Checkpoint     string
	SaveCheckpoint func(string) error
}

// SaveVideoCheckpoint stores provider state as JSON when the caller persists
// checkpoints.
func SaveVideoCheckpoint(request VideoRequest, checkpoint any) error {
	if request.SaveCheckpoint == nil {
		return nil
	}
	data, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}
	return request.SaveCheckpoint(string(data))
}

// VideoCreateFailed wraps a failed create with its stage. An explicit refusal
// created nothing, so the submission marker is cleared and another account
// may retry.
func VideoCreateFailed(request VideoRequest, err error) error {
	stage := VideoCreateFailureStage(err)
	if stage == VideoStageCreate && request.SaveCheckpoint != nil {
		_ = request.SaveCheckpoint("")
	}
	return WrapVideoStage(stage, 0, err)
}

type VideoResult struct {
	URL         string
	ContentType string
	// A non-empty AssetID means the result is stored as a local media asset; content reads must use MediaObjectStorage.
	AssetID string
	// Seconds is the generated duration when the provider shortened the
	// request, such as the Web free-tier cap; 0 means as requested.
	Seconds int
}

type VideoAdapter interface {
	Adapter
	GenerateVideo(ctx context.Context, request VideoRequest) (VideoResult, error)
}

// VideoContentDownloader reads completed video content using the credential that created the task.
// Callers must verify task ownership first.
type VideoContentDownloader interface {
	VideoAdapter
	DownloadVideo(ctx context.Context, credential account.Credential, rawURL string) (io.ReadCloser, string, int64, error)
}

// ValidateVideoRequest applies the provider's video validation; providers
// without a validator accept generation only. A last frame or reference videos
// need a provider that declares Media.VideoLastFrame / VideoReferenceVideos.
func (r *Registry) ValidateVideoRequest(value account.Provider, options VideoRequestOptions) error {
	if options.HasLastFrame || options.ReferenceVideos > 0 {
		definition, _ := r.Definition(value)
		if options.HasLastFrame && !definition.Media.VideoLastFrame {
			return fmt.Errorf("%w: 该模型不支持 last_frame", ErrVideoParameterInvalid)
		}
		if options.ReferenceVideos > 0 && !definition.Media.VideoReferenceVideos {
			return fmt.Errorf("%w: 该模型不支持 reference_videos", ErrVideoParameterInvalid)
		}
	}
	if validator, ok := adapterAs[VideoRequestValidator](r, value); ok {
		return validator.ValidateVideoRequest(options)
	}
	if options.Operation != VideoOperationGenerate {
		return ErrVideoOperationUnsupported
	}
	return nil
}

// NormalizeVideoRequest returns the parameters the route generates (and
// bills) with, then applies the provider's validation to them.
func (r *Registry) NormalizeVideoRequest(value account.Provider, options VideoRequestOptions) (VideoRequestOptions, error) {
	if normalizer, ok := adapterAs[VideoRequestNormalizer](r, value); ok {
		var err error
		if options, err = normalizer.NormalizeVideoRequest(options); err != nil {
			return options, err
		}
	}
	return options, r.ValidateVideoRequest(value, options)
}

// NormalizeGrokVideoRequest fills the xAI defaults and enforces the xAI
// ranges: generation defaults to 16:9, 720p and 8 s; an edit takes no
// duration, aspect ratio or resolution; an extension takes no aspect ratio or
// resolution and runs 2-10 s, 6 s by default.
func NormalizeGrokVideoRequest(options VideoRequestOptions) (VideoRequestOptions, error) {
	switch options.Operation {
	case VideoOperationGenerate:
	case VideoOperationEdit:
		switch {
		case options.AspectRatio != "" || options.Resolution != "":
			return options, fmt.Errorf("%w: 视频编辑不支持 aspect_ratio 或 resolution", ErrVideoParameterInvalid)
		case options.Duration != 0:
			return options, fmt.Errorf("%w: 视频编辑不支持 duration", ErrVideoParameterInvalid)
		}
		return options, nil
	case VideoOperationExtend:
		options.Duration = cmp.Or(options.Duration, 6)
		switch {
		case options.AspectRatio != "" || options.Resolution != "":
			return options, fmt.Errorf("%w: 视频延长不支持 aspect_ratio 或 resolution", ErrVideoParameterInvalid)
		case options.Duration < 2 || options.Duration > 10:
			return options, fmt.Errorf("%w: 视频延长 duration 必须在 2 到 10 秒之间", ErrVideoParameterInvalid)
		}
		return options, nil
	default:
		return options, nil
	}
	options.AspectRatio = cmp.Or(options.AspectRatio, "16:9")
	options.Resolution = cmp.Or(strings.ToLower(options.Resolution), "720p")
	options.Duration = cmp.Or(options.Duration, 8)
	switch {
	case options.Duration < 1 || options.Duration > 15:
		return options, fmt.Errorf("%w: duration 必须在 1 到 15 秒之间", ErrVideoParameterInvalid)
	case !slices.Contains([]string{"1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3"}, options.AspectRatio):
		return options, fmt.Errorf("%w: aspect_ratio 必须是 1:1、16:9、9:16、4:3、3:4、3:2 或 2:3", ErrVideoParameterInvalid)
	case !slices.Contains([]string{"480p", "720p", "1080p"}, options.Resolution):
		return options, fmt.Errorf("%w: resolution 必须是 480p、720p 或 1080p", ErrVideoParameterInvalid)
	case options.Resolution == "1080p" && (options.ReferenceImages > 0 || options.ReferenceAudios > 0):
		return options, fmt.Errorf("%w: 参考图视频 resolution 最高 720p", ErrVideoParameterInvalid)
	}
	return options, nil
}

// VideoQuotaMode refines a video route's quota mode by resolution.
func (r *Registry) VideoQuotaMode(value account.Provider, quotaMode, resolution string) string {
	if adapter, ok := adapterAs[VideoQuotaModeAdapter](r, value); ok {
		return adapter.VideoQuotaMode(quotaMode, resolution)
	}
	return quotaMode
}

func (r *Registry) Videos(value account.Provider) (VideoAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(VideoAdapter)
	return result, ok
}
