package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/requestmeta"
	"github.com/chenyme/grok2api/backend/internal/repository"
	_ "golang.org/x/image/webp"
)

const (
	// ImageJobIDPrefix marks image jobs among Responses IDs.
	ImageJobIDPrefix = "resp_img_"
	imageJobLease    = 2 * time.Minute
	imageJobRenewal  = 30 * time.Second
	imageJobTimeout  = time.Hour
	// imageJobMaxClaims stops a job whose workers keep dying mid-run.
	imageJobMaxClaims    = 5
	imageJobPollInterval = time.Second
	imageResultMaxBytes  = 64 << 20
)

var (
	ErrIdempotencyConflict    = errors.New("Idempotency-Key 已用于不同的请求")
	ErrMediaJobNotCancellable = errors.New("任务已提交上游，无法取消")
	ErrMediaJobNotRetryable   = errors.New("只有结果不确定的任务或上游已生成结果的失败图片任务可以重试")
	ErrImageInputTooLarge     = errors.New("图片输入编码后超过 32 MiB")
	// errJobLost means the claim was canceled or recovered elsewhere.
	errJobLost = errors.New("media job claim lost")
)

// ImageJobInput is a validated asynchronous image request.
type ImageJobInput struct {
	RequestID   string
	ClientKey   clientkey.Key
	PublicModel string
	Prompt      string
	// ImageURLs and MaskURL are data URIs or https URLs; images make it an edit.
	ImageURLs    []string
	MaskURL      string
	Count        int
	Size         string
	Quality      string
	OutputFormat string
	Background   string
	// ResponseModel and ResponseBackground echo the Responses request.
	ResponseModel      string
	ResponseBackground bool
	IdempotencyKey     string
}

// imageJobInputs is the input_json of an image job.
type imageJobInputs struct {
	ImageURLs []string `json:"image_urls,omitempty"`
	MaskURL   string   `json:"mask_url,omitempty"`
}

// ImageJobImage is one delivered image of an image job. Missing marks an
// image whose stored result was removed (capacity cleanup, gallery deletion).
type ImageJobImage struct {
	Data          []byte
	MIMEType      string
	Width, Height int
	Missing       bool
}

type imageItemOutcome struct {
	status         media.Status
	code           string
	err            error
	upstreamStatus int
	// attempts are the failed upstream calls shown in the request diagnostics.
	attempts []audit.Attempt
}

func imageUsageEventID(jobID string, index int) string {
	return fmt.Sprintf("image_usage_%s_%d", jobID, index)
}

// imageItemEventID is the usage event (audit and reservation) of image index.
// The image a resume re-fetches gets a fresh event per resume, since the
// attempt that failed it already wrote the earlier one.
func imageItemEventID(job media.Job, index int) string {
	if job.ResumeGeneration > 0 && index == job.ResumeItem {
		return fmt.Sprintf("%s_r%d", imageUsageEventID(job.ID, index), job.ResumeGeneration)
	}
	return imageUsageEventID(job.ID, index)
}

// resumeDone reports that a resumed job delivered the image it re-fetches. A
// resume never draws new images, so the job ends there.
func resumeDone(job media.Job) bool {
	return job.ResumeGeneration > 0 && len(job.ResultAssetIDs) > job.ResumeItem
}

// ImageJobResumable reports whether a failed image job can re-fetch, on its
// account, the image the upstream already accepted instead of drawing it again.
func ImageJobResumable(job media.Job) bool {
	if job.Kind != media.KindImage || job.Status != media.StatusFailed || !job.Submitted || job.AccountID == 0 {
		return false
	}
	var cp provider.ImageCheckpoint
	return json.Unmarshal([]byte(job.Checkpoint), &cp) == nil && cp.Submitted && cp.UpstreamID != ""
}

// itemShare splits the reserved total so the per-image charges sum to it exactly.
func itemShare(total int64, count, index int) int64 {
	return total*int64(index+1)/int64(count) - total*int64(index)/int64(count)
}

func payloadHash(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func imageJobOperation(job media.Job) audit.Operation {
	if job.Operation == media.VideoOperationEdit {
		return audit.OperationImageEdit
	}
	return audit.OperationImage
}

// UpdateDefaultImageModel sets the image route of a Responses image_generation
// tool that names no model.
func (s *Service) UpdateDefaultImageModel(model string) {
	model = strings.TrimSpace(model)
	s.defaultImageModel.Store(&model)
}

// ImageToolModel returns the public image model that serves a Responses
// image_generation tool through image jobs, or "" when the request stays on
// the conversation path. A tool without a model uses the top-level model, else
// the configured default when the top-level model belongs to a provider with
// resumable images.
func (s *Service) ImageToolModel(ctx context.Context, toolModel, topModel string) string {
	if toolModel != "" {
		if s.hasAsyncImageRoute(ctx, toolModel, true) {
			return toolModel
		}
		return ""
	}
	if s.hasAsyncImageRoute(ctx, topModel, true) {
		return topModel
	}
	fallback := ""
	if value := s.defaultImageModel.Load(); value != nil {
		fallback = *value
	}
	if fallback == "" || !s.hasAsyncImageRoute(ctx, topModel, false) || !s.hasAsyncImageRoute(ctx, fallback, true) {
		return ""
	}
	return fallback
}

// IsImageJobModel reports whether name is an image route of a provider that
// draws images as jobs.
func (s *Service) IsImageJobModel(ctx context.Context, name string) bool {
	return s.hasAsyncImageRoute(ctx, name, true)
}

// hasAsyncImageRoute reports whether name has a route of a provider with
// resumable images, limited to image routes when imageOnly is set.
func (s *Service) hasAsyncImageRoute(ctx context.Context, name string, imageOnly bool) bool {
	if strings.TrimSpace(name) == "" {
		return false
	}
	routes, err := s.models.GetByPublicIDCandidates(ctx, name)
	if err != nil {
		return false
	}
	for _, route := range routes {
		if imageOnly && route.Capability != modeldomain.CapabilityImage && route.Capability != modeldomain.CapabilityImageEdit {
			continue
		}
		if s.providers.AsyncImageModel(route.Provider, route.UpstreamModel) {
			return true
		}
	}
	return false
}

// idempotentJob returns the job a client key already created under key.
func (s *Service) idempotentJob(ctx context.Context, clientKeyID uint64, key, hash string) (media.Job, bool, error) {
	if key == "" {
		return media.Job{}, false, nil
	}
	job, err := s.mediaJobs.GetMediaJobByIdempotencyKey(ctx, clientKeyID, key)
	if errors.Is(err, repository.ErrNotFound) {
		return media.Job{}, false, nil
	}
	if err != nil {
		return media.Job{}, false, err
	}
	if job.PayloadHash != hash {
		return media.Job{}, false, ErrIdempotencyConflict
	}
	return job, true, nil
}

// CreateImageJob admits a durable image job on a route whose provider draws
// resumable images, reserves each image's share of the price and queues it.
func (s *Service) CreateImageJob(ctx context.Context, input ImageJobInput) (media.Job, error) {
	if s.mediaJobs == nil || s.mediaQueue == nil {
		return media.Job{}, errors.New("图片任务服务未配置")
	}
	count := max(1, input.Count)
	edit := len(input.ImageURLs) > 0
	operation, capability := media.VideoOperationGenerate, modeldomain.CapabilityImage
	if edit {
		operation, capability = media.VideoOperationEdit, modeldomain.CapabilityImageEdit
	}
	switch {
	case strings.TrimSpace(input.Prompt) == "":
		return media.Job{}, &ImageRequestRejectedError{Err: errors.New("prompt 不能为空")}
	case len(input.Prompt) > 100000:
		return media.Job{}, &ImageRequestRejectedError{Err: errors.New("prompt 过长")}
	case count > media.MaxImageJobItems:
		return media.Job{}, &ImageRequestRejectedError{Err: fmt.Errorf("n 最多为 %d", media.MaxImageJobItems)}
	case len(input.ImageURLs) > media.MaxInputImages:
		return media.Job{}, &ImageRequestRejectedError{Err: fmt.Errorf("最多支持 %d 张输入图片", media.MaxInputImages)}
	}
	inputJSON, err := json.Marshal(imageJobInputs{ImageURLs: input.ImageURLs, MaskURL: input.MaskURL})
	if err != nil {
		return media.Job{}, err
	}
	if len(inputJSON) > media.MaxInputJSONBytes {
		return media.Job{}, ErrImageInputTooLarge
	}
	params := media.ImageParams{OutputFormat: input.OutputFormat, Background: input.Background, ResponseModel: input.ResponseModel, ResponseBackground: input.ResponseBackground}
	hash := payloadHash(struct {
		Model, Prompt, Size, Quality, Input string
		Count                               int
		Params                              media.ImageParams
	}{input.PublicModel, input.Prompt, input.Size, input.Quality, string(inputJSON), count, params})
	if existing, found, err := s.idempotentJob(ctx, input.ClientKey.ID, input.IdempotencyKey, hash); err != nil || found {
		return existing, err
	}
	routes, err := s.models.GetByPublicIDCandidates(ctx, input.PublicModel)
	if err != nil {
		return media.Job{}, ErrModelNotFound
	}
	// Drop routes whose provider draws jobs but answers this model only
	// synchronously (ChatGPT Codex images).
	routes = slices.DeleteFunc(routes, func(route modeldomain.Route) bool {
		_, async := s.providers.AsyncImages(route.Provider)
		return async && !s.providers.AsyncImageModel(route.Provider, route.UpstreamModel)
	})
	if len(routes) == 0 {
		return media.Job{}, &ImageRequestRejectedError{Err: errors.New("该模型不支持异步生图任务，请使用 Images API")}
	}
	options := provider.ImageRequestOptions{
		Edit: edit, Count: count, InputImages: len(input.ImageURLs), HasMask: input.MaskURL != "",
		Size: input.Size, Quality: input.Quality, OutputFormat: input.OutputFormat, Background: input.Background,
	}
	var rejection error
	supports := func(providerValue accountdomain.Provider) bool {
		if _, ok := s.providers.AsyncImages(providerValue); !ok {
			return false
		}
		validator, ok := s.providers.ImageRequestValidator(providerValue)
		if !ok {
			return true
		}
		err := validator.ValidateImageRequest(options)
		if err != nil && rejection == nil {
			rejection = &ImageRequestRejectedError{Err: err}
		}
		return err == nil
	}
	route, selection, err := s.selectSchedulableMediaRoute(ctx, routes, input.ClientKey, capability, true, supports)
	if err != nil {
		if rejection != nil && errors.Is(err, ErrNoAvailableAccount) {
			return media.Job{}, rejection
		}
		return media.Job{}, err
	}
	if err := s.checkLedgerReady(); err != nil {
		return media.Job{}, err
	}
	pricingModel := s.providers.PricingModel(route.Provider, route.UpstreamModel)
	var price audit.PricingResult
	if edit {
		price, _ = s.pricing.QuoteImageEdit(route.Provider, pricingModel, "", input.Quality, count, len(input.ImageURLs))
	} else {
		price, _ = s.pricing.QuoteImage(route.Provider, pricingModel, "", input.Quality, count)
	}
	lease, err := selection.Acquire(ctx, nil, false)
	if err != nil {
		return media.Job{}, fmt.Errorf("%w: %w", ErrNoAvailableAccount, err)
	}
	accountID, accountName := lease.Credential.ID, lease.Credential.Name
	lease.Release()
	token, err := security.NewHexToken(16)
	if err != nil {
		return media.Job{}, err
	}
	now := time.Now().UTC()
	job := media.Job{
		ID: ImageJobIDPrefix + token, Kind: media.KindImage, ItemCount: count, RequestID: input.RequestID,
		ClientKeyID: input.ClientKey.ID, ClientKeyName: input.ClientKey.Name, ClientIP: requestmeta.ClientIP(ctx),
		AccountID: accountID, AccountName: accountName,
		Provider: string(route.Provider), Model: modeldomain.ExternalPublicID(route.Provider, route.PublicID), ModelRouteID: route.ID,
		UpstreamModel: modeldomain.DisplayUpstreamModel(route.Provider, route.UpstreamModel), Operation: operation, Prompt: input.Prompt,
		Size: input.Size, Quality: input.Quality, Status: media.StatusQueued, InputJSON: string(inputJSON), InputImageCount: len(input.ImageURLs),
		Params: params, IdempotencyKey: input.IdempotencyKey, PayloadHash: hash, Price: &price, CreatedAt: now, UpdatedAt: now,
	}
	if job.RequestID == "" {
		job.RequestID = job.ID
	}
	var reserved []string
	releaseReservations := func() {
		for _, eventID := range reserved {
			s.cancelBillingReservation(eventID)
		}
	}
	for index := range count {
		eventID := imageUsageEventID(job.ID, index)
		ok, err := s.clientKeys.ReserveBilling(ctx, input.ClientKey, eventID, itemShare(price.CostInUSDTicks, count, index), mediaBillingReservationTTL)
		if err != nil {
			releaseReservations()
			return media.Job{}, err
		}
		if ok {
			reserved = append(reserved, eventID)
		}
	}
	if err := s.mediaJobs.CreateMediaJob(ctx, job); err != nil {
		releaseReservations()
		if errors.Is(err, repository.ErrConflict) {
			if existing, found, findErr := s.idempotentJob(ctx, input.ClientKey.ID, input.IdempotencyKey, hash); findErr != nil || found {
				return existing, findErr
			}
		}
		return media.Job{}, err
	}
	if !s.enqueueMediaJob(job.ID) {
		s.logger.Warn("image_job_queue_full", "job_id", job.ID)
	}
	return job, nil
}

// runImageJob draws the job's images one after another on one account,
// resuming the image in flight from its checkpoint after a crash.
func (s *Service) runImageJob(parent context.Context, job media.Job, route modeldomain.Route) {
	adapter, ok := s.providers.AsyncImages(route.Provider)
	if !ok {
		s.endImageJob(parent, &job, media.StatusFailed, "provider_unavailable", ErrNoAvailableAccount, 0)
		return
	}
	if job.Generation-job.ResumeGeneration > imageJobMaxClaims {
		status := media.StatusFailed
		if job.Submitted {
			status = media.StatusUncertain
		}
		s.endImageJob(parent, &job, status, "recovery_exhausted", errors.New("任务多次恢复仍未完成"), 0)
		return
	}
	ctx, lose := context.WithCancelCause(parent)
	defer lose(nil)
	ctx, cancel := context.WithTimeout(ctx, imageJobTimeout)
	defer cancel()
	ctx, egressTrace := infraegress.WithTrace(ctx)
	// The job's images share inputs loaded and checked once.
	if scoped, ok := adapter.(interface {
		WithImageJobScope(context.Context) context.Context
	}); ok {
		ctx = scoped.WithImageJobScope(ctx)
	}
	// The claim carries the long video lease; image jobs hold a short one
	// and renew it, so a dead worker's job is recovered within minutes.
	if err := s.renewImageJobLease(ctx, job); errors.Is(err, repository.ErrNotFound) {
		return
	}
	heartbeat, claim := make(chan struct{}), job
	go func() {
		defer close(heartbeat)
		s.keepImageJobLease(ctx, lose, claim)
	}()
	defer func() {
		cancel()
		<-heartbeat
	}()
	var inputs imageJobInputs
	_ = json.Unmarshal([]byte(job.InputJSON), &inputs)
	request := provider.ImageJobRequest{
		Model: route.UpstreamModel, Prompt: job.Prompt, ImageURLs: inputs.ImageURLs, MaskURL: inputs.MaskURL,
		Size: job.Size, Quality: job.Quality, OutputFormat: job.Params.OutputFormat, Background: job.Params.Background,
	}
	for len(job.ResultAssetIDs) < job.ItemCount && !resumeDone(job) {
		outcome := s.runImageItem(ctx, &job, route, adapter, request, egressTrace)
		if errors.Is(outcome.err, errJobLost) || errors.Is(context.Cause(ctx), errJobLost) {
			return
		}
		if parent.Err() != nil {
			// Shutdown: the lease expires and recovery resumes from the checkpoint.
			return
		}
		if outcome.status == "" && outcome.err != nil {
			// The job deadline or a job write interrupted the image.
			outcome.status, outcome.code = media.StatusFailed, "interrupted"
			if job.Submitted {
				outcome.status = media.StatusUncertain
			}
		}
		if outcome.status != "" {
			s.endImageJob(parent, &job, outcome.status, outcome.code, outcome.err, outcome.upstreamStatus, outcome.attempts...)
			return
		}
	}
	if missing := job.ItemCount - len(job.ResultAssetIDs); missing > 0 {
		s.endImageJob(parent, &job, media.StatusFailed, "partial_result", fmt.Errorf("已重新获取上游生成的图片，其余 %d 张未生成", missing), 0)
		return
	}
	s.endImageJob(parent, &job, media.StatusCompleted, "", nil, 0)
}

func (s *Service) renewImageJobLease(ctx context.Context, job media.Job) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	return s.mediaJobs.RenewMediaJobLease(writeCtx, job, time.Now().UTC().Add(imageJobLease))
}

// keepImageJobLease renews the claim until ctx ends; a lost claim stops the
// worker through lose.
func (s *Service) keepImageJobLease(ctx context.Context, lose context.CancelCauseFunc, job media.Job) {
	ticker := time.NewTicker(imageJobRenewal)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.renewImageJobLease(ctx, job); errors.Is(err, repository.ErrNotFound) {
				lose(errJobLost)
				return
			}
		}
	}
}

// saveImageJob writes job under its claim fence.
func (s *Service) saveImageJob(ctx context.Context, job *media.Job) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	job.UpdatedAt = time.Now().UTC()
	err := s.mediaJobs.UpdateMediaJob(writeCtx, *job)
	if errors.Is(err, repository.ErrNotFound) {
		return errJobLost
	}
	return err
}

// runImageItem draws the next image. A zero outcome means it was delivered.
func (s *Service) runImageItem(ctx context.Context, job *media.Job, route modeldomain.Route, adapter provider.AsyncImageAdapter, request provider.ImageJobRequest, egressTrace *infraegress.Trace) imageItemOutcome {
	startedAt := time.Now()
	index := len(job.ResultAssetIDs)
	attempts := newFailureAttemptRecorder(http.MethodPost, "/v1/responses")
	var cp provider.ImageCheckpoint
	_ = json.Unmarshal([]byte(job.Checkpoint), &cp)
	save := func(next provider.ImageCheckpoint) error {
		data, err := json.Marshal(next)
		if err != nil {
			return err
		}
		job.Checkpoint, job.Submitted = string(data), next.Submitted
		return s.saveImageJob(ctx, job)
	}
	quotaMode := s.providers.QuotaMode(route.Provider, route.UpstreamModel)
	var lease *accountLease
	switch {
	case cp.Submitted && cp.UpstreamID == "":
		return imageItemOutcome{status: media.StatusUncertain, code: "submission_uncertain", err: errors.New("上游可能已接收请求，但未记录上游 ID")}
	case cp.Submitted:
		// Resume on the account that submitted the image.
		credential, err := s.jobCredential(ctx, *job)
		if err != nil {
			return imageItemOutcome{status: media.StatusUncertain, code: "account_unavailable", err: errors.New("提交任务的账号已不可用，无法恢复上游任务")}
		}
		request.Credential = credential
	default:
		var outcome *imageItemOutcome
		lease, cp, outcome = s.submitImageItem(ctx, job, route, adapter, request, save, quotaMode, attempts)
		if outcome != nil {
			return *outcome
		}
		request.Credential = lease.Credential
		quotaMode = lease.QuotaMode
	}
	succeeded := false
	defer func() {
		lease.completeSelectorObservation(succeeded)
		lease.Release()
	}()
	stageStarted := time.Now()
	fail := func(code string, err error) imageItemOutcome {
		if ctx.Err() != nil {
			return imageItemOutcome{err: context.Cause(ctx)}
		}
		_ = attempts.captureResponse(request.Credential, stageStarted, nil, err)
		return imageItemOutcome{status: media.StatusFailed, code: code, err: err, upstreamStatus: provider.ErrorHTTPStatusOrZero(err), attempts: attempts.snapshot()}
	}
	cp, err := adapter.PollImage(ctx, request, cp)
	if err != nil {
		return fail("generation_failed", err)
	}
	if err := save(cp); err != nil {
		return imageItemOutcome{err: err}
	}
	var data []byte
	stageStarted = time.Now()
	for attempt := 0; attempt < videoOutputAttempts; attempt++ {
		if data, err = adapter.FetchImage(ctx, request, cp); err == nil || ctx.Err() != nil || attempt+1 == videoOutputAttempts || isImageOutputFailure(err) {
			break
		}
		if waitErr := waitVideoOutputRetry(ctx, attempt); waitErr != nil {
			break
		}
	}
	if err != nil {
		return fail("media_download_failed", err)
	}
	operation := imageJobOperation(*job)
	asset, err := s.mediaAssets.SaveImage(media.WithOrigin(ctx, media.JobOrigin(*job, string(operation))), data)
	if err != nil {
		return fail("media_storage_failed", err)
	}
	applyMediaJobEgress(job, egressTrace, route.Provider)
	job.ResultAssetIDs = append(job.ResultAssetIDs, asset.ID)
	job.Checkpoint, job.Submitted = "", false
	job.Progress = min(99, len(job.ResultAssetIDs)*100/job.ItemCount)
	if err := s.saveImageJob(ctx, job); err != nil {
		return imageItemOutcome{err: err}
	}
	succeeded = true
	s.consumeImageQuota(route, job.AccountID, quotaMode)
	if err := s.recordImageItemAudit(context.WithoutCancel(ctx), *job, index, http.StatusOK, "", time.Since(startedAt).Milliseconds()); err != nil {
		s.logger.Error("image_usage_record_failed", "job_id", job.ID, "event_id", imageItemEventID(*job, index), "error", err)
	}
	return imageItemOutcome{}
}

// isImageOutputFailure reports a fetched image that fails the requested output
// (chatgpt.IsImageOutputError, e.g. not transparent): fetching it again
// returns the same image.
func isImageOutputFailure(err error) bool {
	var output interface{ ImageOutputFailure() bool }
	return errors.As(err, &output) && output.ImageOutputFailure()
}

// submitImageItem sends the image from the job's account, failing over to
// others while nothing has reached the upstream.
func (s *Service) submitImageItem(ctx context.Context, job *media.Job, route modeldomain.Route, adapter provider.AsyncImageAdapter, request provider.ImageJobRequest, save func(provider.ImageCheckpoint) error, quotaMode string, attempts *failureAttemptRecorder) (*accountLease, provider.ImageCheckpoint, *imageItemOutcome) {
	var cp provider.ImageCheckpoint
	stop := func(status media.Status, code string, err error) (*accountLease, provider.ImageCheckpoint, *imageItemOutcome) {
		if ctx.Err() != nil {
			err = context.Cause(ctx)
			status = ""
		}
		return nil, cp, &imageItemOutcome{status: status, code: code, err: err, upstreamStatus: provider.ErrorHTTPStatusOrZero(err), attempts: attempts.snapshot()}
	}
	scope := clientkey.AccountScope{}
	if job.ClientKeyID > 0 {
		key, err := s.clientKeys.Get(ctx, job.ClientKeyID)
		if err != nil {
			return stop(media.StatusFailed, "client_key_unavailable", err)
		}
		scope = key.AccountScope()
	}
	attemptPolicy := newRoutingAttemptPolicy(int(s.maxAttempts.Load()))
	excluded := map[uint64]bool{}
	retried := map[uint64]bool{}
	pinned := job.AccountID
	var selection *selectionSession
	var lastErr error
	for attempt := 0; attemptPolicy.allows(attempt); attempt++ {
		var lease *accountLease
		var err error
		if pinned > 0 && !excluded[pinned] {
			if lease, err = s.selector.AcquirePinnedForKey(ctx, route.Provider, pinned, route.ID, route.UpstreamModel, quotaMode, true, scope); err != nil {
				excluded[pinned], lease, err = true, nil, nil
			}
		}
		pinned = 0
		if lease == nil {
			if selection == nil {
				selection, err = s.selector.beginSelectionSessionForKey(ctx, route.Provider, route.ID, route.UpstreamModel, quotaMode, "", excluded, false, scope)
			}
			if err == nil {
				lease, err = selection.Acquire(ctx, excluded, false)
			}
		}
		if err != nil {
			if lastErr == nil {
				lastErr = err
			}
			return stop(media.StatusFailed, "account_unavailable", lastErr)
		}
		excluded[lease.Credential.ID] = true
		attemptStarted := time.Now()
		credential, err := s.accounts.EnsureCredential(ctx, lease.Credential, false)
		if err != nil {
			attempts.captureCredentialFailure(lease.Credential, attemptStarted, false, err)
			lastErr = err
			lease.Release()
			continue
		}
		lease.Credential = credential
		job.AccountID, job.AccountName = credential.ID, credential.Name
		request.Credential = credential
		lease.markSelectorUpstreamStarted()
		attemptStarted = time.Now()
		cp, err = adapter.SubmitImage(ctx, request, save)
		if err == nil {
			return lease, cp, nil
		}
		_ = attempts.captureResponse(credential, attemptStarted, nil, err)
		status := provider.ErrorHTTPStatusOrZero(err)
		if errors.Is(err, errJobLost) || ctx.Err() != nil {
			lease.Release()
			return stop("", "", err)
		}
		if cp.Submitted {
			switch {
			case status >= http.StatusBadRequest && status < http.StatusInternalServerError:
				lease.completeSelectorObservation(false)
				lease.Release()
				return stop(media.StatusFailed, "upstream_rejected", err)
			case cp.UpstreamID != "":
				// The upstream accepted it before the stream broke: poll it.
				return lease, cp, nil
			default:
				lease.completeSelectorObservation(false)
				lease.Release()
				return stop(media.StatusUncertain, "submission_uncertain", err)
			}
		}
		// Nothing reached the upstream: clear a persisted marker before retrying.
		if job.Submitted {
			if saveErr := save(cp); saveErr != nil {
				lease.Release()
				return stop("", "", saveErr)
			}
		}
		lastErr = err
		lease.completeSelectorObservation(false)
		lease.Release()
		switch {
		case provider.IsRequestScopedError(err):
			return stop(media.StatusFailed, "invalid_request", err)
		case status == http.StatusUnauthorized && credential.AuthType == accountdomain.AuthTypeSSO:
			s.markSSOCredentialRejected(ctx, credential, fmt.Sprintf("%s SSO credential rejected", credential.Provider))
		case status == http.StatusUnauthorized && s.providers.SupportsCredentialRefresh(credential.Provider):
			// Rejected before submission: one retry on the same account after a forced refresh.
			if s.markPermanentlyUnrefreshableCredentialRejected(ctx, credential) || retried[credential.ID] {
				continue
			}
			retried[credential.ID] = true
			if _, refreshErr := s.accounts.RenewRejectedCredential(ctx, credential); refreshErr != nil {
				if errors.Is(refreshErr, accountapp.ErrCredentialRefreshPermanent) {
					s.markCredentialRejectedAfterPermanentRefresh(ctx, credential)
				}
				s.selector.MarkRefreshDeferred(ctx, credential, refreshErr)
				continue
			}
			delete(excluded, credential.ID)
			pinned = credential.ID
		case status == http.StatusForbidden && s.providers.RetryForbiddenAsEgress(credential.Provider) && !retried[credential.ID]:
			// An egress-scoped 403: the same account once more on a fresh egress.
			retried[credential.ID] = true
			delete(excluded, credential.ID)
			pinned = credential.ID
		case status == http.StatusTooManyRequests && lease.QuotaMode != "":
			if quotaKind, _ := s.providers.QuotaKind(credential.Provider); quotaKind == provider.QuotaRemoteWindow {
				exhausted, reconcileErr := s.accounts.ReconcileWebRateLimit(ctx, credential.ID, lease.QuotaMode, provider.ErrorRetryAfter(err))
				s.selector.MarkQuotaStateChanged(credential.Provider, credential.ID)
				if reconcileErr != nil || !exhausted {
					s.selector.MarkFailure(ctx, credential, status, provider.ErrorRetryAfter(err))
				}
				continue
			}
			s.selector.MarkFailure(ctx, credential, status, provider.ErrorRetryAfter(err))
		default:
			s.selector.MarkFailure(ctx, credential, status, 0)
		}
	}
	if lastErr == nil {
		lastErr = ErrNoAvailableAccount
	}
	return stop(media.StatusFailed, "generation_failed", lastErr)
}

// consumeImageQuota charges one delivered image to the account's remote
// quota window and queues a refresh, like the synchronous image path.
func (s *Service) consumeImageQuota(route modeldomain.Route, accountID uint64, quotaMode string) {
	quotaKind, _ := s.providers.QuotaKind(route.Provider)
	refreshMode, decrementMode, availabilityMode := quotaFinalizationModes(quotaMode, s.providers.QuotaRefreshGroup(route.Provider, route.UpstreamModel))
	if quotaKind != provider.QuotaRemoteWindow || refreshMode == "" || accountID == 0 {
		return
	}
	if decrementMode != "" && decrementMode != "weekly" {
		ctx, cancel := context.WithTimeout(context.Background(), accountStateWriteTimeout)
		updated, err := s.accounts.DecrementWebQuota(ctx, accountID, decrementMode, 1)
		cancel()
		if err != nil {
			s.logger.Warn("image_quota_decrement_failed", "account_id", accountID, "mode", decrementMode, "error", err)
		} else if updated {
			s.selector.ConsumeQuota(route.Provider, accountID, decrementMode, 1)
		}
	}
	s.accounts.QueueQuotaRefresh(accountID, refreshMode)
	if availabilityMode != "" && availabilityMode != refreshMode {
		s.accounts.QueueQuotaRefresh(accountID, availabilityMode)
	}
}

// endImageJob writes the terminal state, releases the reservations of
// undelivered images and records the remaining audits.
func (s *Service) endImageJob(ctx context.Context, job *media.Job, status media.Status, code string, err error, upstreamStatus int, attempts ...audit.Attempt) {
	if status == media.StatusUncertain && job.ResumeGeneration > 0 {
		// A resumed job still names its upstream result: it stays resumable,
		// never resubmitted (its inputs were compacted when it first failed).
		status = media.StatusFailed
	}
	now := time.Now().UTC()
	job.Status, job.ErrorCode, job.ErrorMessage = status, code, jobErrorMessage(err)
	if status == media.StatusCompleted {
		job.Progress = 100
	}
	job.LeaseUntil, job.UpdatedAt, job.CompletedAt = nil, now, &now
	if writeErr := s.persistVideoJobWithRetry(ctx, *job); writeErr != nil {
		s.logger.Error("image_job_terminal_write_failed", "job_id", job.ID, "error", writeErr)
		return
	}
	s.releaseJobReservations(*job)
	if auditErr := s.recordImageJobAudits(context.Background(), *job, upstreamStatus, attempts...); auditErr != nil {
		s.logger.Error("image_usage_record_failed", "job_id", job.ID, "error", auditErr)
	}
}

// jobErrorMessage is err's text as valid UTF-8 cut to the 512-byte column on
// a rune boundary; PostgreSQL rejects a write that splits a rune.
func jobErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	return truncateDiagnosticText(strings.ToValidUTF8(err.Error(), "�"), 512)
}

// recordImageJobAudits writes the audit of every delivered image and of the
// image that ended the job; repeated calls are idempotent per event ID.
func (s *Service) recordImageJobAudits(ctx context.Context, job media.Job, upstreamStatus int, attempts ...audit.Attempt) error {
	durationMS := int64(0)
	if job.CompletedAt != nil {
		durationMS = max(int64(0), job.CompletedAt.Sub(job.CreatedAt).Milliseconds())
	}
	for index := range job.ResultAssetIDs {
		if err := s.recordImageItemAudit(ctx, job, index, http.StatusOK, "", 0); err != nil {
			return err
		}
	}
	// A resumed job that delivered its image ends on images it never drew:
	// the failure that stopped them is already recorded. A cancel is no failure.
	if job.Status != media.StatusCompleted && job.Status != media.StatusCanceled && len(job.ResultAssetIDs) < job.ItemCount && !resumeDone(job) {
		statusCode := resolveVideoAuditStatusCode(job, upstreamStatus, nil)
		if err := s.recordImageItemAudit(ctx, job, len(job.ResultAssetIDs), statusCode, job.ErrorCode, durationMS, attempts...); err != nil {
			return err
		}
	}
	markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	return s.mediaJobs.MarkMediaJobUsageRecorded(markCtx, job.ID, time.Now().UTC())
}

// recordImageItemAudit bills a delivered image at its share of the reserved
// quote, or records the failure of the image that ended the job.
func (s *Service) recordImageItemAudit(ctx context.Context, job media.Job, index, statusCode int, errorCode string, durationMS int64, attempts ...audit.Attempt) error {
	createdAt := time.Now().UTC()
	if statusCode != http.StatusOK && job.CompletedAt != nil {
		createdAt = job.CompletedAt.UTC()
	}
	record := audit.Record{
		EventID: imageItemEventID(job, index), RequestID: job.RequestID, ClientKeyID: job.ClientKeyID, ClientKeyName: job.ClientKeyName,
		ClientIP: job.ClientIP, ModelRouteID: job.ModelRouteID, ModelPublicID: job.Model, ModelUpstreamModel: job.UpstreamModel,
		Provider: job.Provider, Operation: imageJobOperation(job), UsageSource: audit.UsageSourceNone,
		AccountID: auditAccountID(job.AccountID), AccountName: job.AccountName, StatusCode: statusCode, ErrorCode: errorCode,
		EgressNodeID: job.EgressNodeID, EgressNodeName: job.EgressNodeName, EgressScope: job.EgressScope, EgressMode: audit.EgressMode(job.EgressMode),
		DurationMS: durationMS, CreatedAt: createdAt, RequestMethod: http.MethodPost, RequestPath: "/v1/responses",
		Attempts: attempts,
	}
	if job.Operation == media.VideoOperationEdit {
		record.MediaInputImages = int64(job.InputImageCount)
	}
	if statusCode == http.StatusOK {
		record.MediaOutputImages = 1
		if job.Price != nil && job.Price.Model != "" {
			share := *job.Price
			share.CostInUSDTicks = itemShare(job.Price.CostInUSDTicks, job.ItemCount, index)
			share.Apply(&record)
		}
	}
	if durable, ok := s.audits.(interface {
		CreateDurable(context.Context, audit.Record) error
	}); ok {
		return durable.CreateDurable(ctx, record)
	}
	return s.audits.Create(ctx, record)
}

// releaseJobReservations cancels the billing reservations a job will not settle.
func (s *Service) releaseJobReservations(job media.Job) {
	if job.Kind != media.KindImage {
		s.cancelBillingReservation("video_usage_" + job.ID)
		return
	}
	for index := len(job.ResultAssetIDs); index < job.ItemCount; index++ {
		s.cancelBillingReservation(imageItemEventID(job, index))
	}
}

// GetImageJob returns an image job owned by the client key.
func (s *Service) GetImageJob(ctx context.Context, key clientkey.Key, id string) (media.Job, error) {
	if s.mediaJobs == nil || !strings.HasPrefix(id, ImageJobIDPrefix) {
		return media.Job{}, ErrResponseNotFound
	}
	job, err := s.mediaJobs.GetMediaJob(ctx, id, key.ID)
	if errors.Is(err, repository.ErrNotFound) || (err == nil && job.Kind != media.KindImage) {
		return media.Job{}, ErrResponseNotFound
	}
	return job, err
}

// WaitImageJob polls created until it ends, calling tick after every read.
// A caller that gives up cancels the work that has not reached the upstream,
// unless the job has an Idempotency-Key: the client's retry, or a duplicate
// request waiting on the same job, gets its result instead of a cancel.
func (s *Service) WaitImageJob(ctx context.Context, key clientkey.Key, created media.Job, tick func(media.Job)) (media.Job, error) {
	ticker := time.NewTicker(imageJobPollInterval)
	defer ticker.Stop()
	id := created.ID
	giveUp := func(job media.Job) (media.Job, error) {
		if created.IdempotencyKey == "" {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_, _ = s.CancelImageJob(cleanup, key, id)
		}
		return job, ctx.Err()
	}
	for {
		job, err := s.GetImageJob(ctx, key, id)
		if err != nil {
			if ctx.Err() != nil {
				return giveUp(job)
			}
			return job, err
		}
		if tick != nil {
			tick(job)
		}
		if job.Status.Terminal() {
			return job, nil
		}
		select {
		case <-ctx.Done():
			return giveUp(job)
		case <-ticker.C:
		}
	}
}

// CancelImageJob cancels a client's image job unless its image in flight
// already reached the upstream (ErrMediaJobNotCancellable). Ended jobs are
// returned unchanged.
func (s *Service) CancelImageJob(ctx context.Context, key clientkey.Key, id string) (media.Job, error) {
	if !strings.HasPrefix(id, ImageJobIDPrefix) {
		return media.Job{}, ErrResponseNotFound
	}
	return s.cancelMediaJob(ctx, id, key.ID)
}

// CancelMediaJob is the administrator cancel of any media job.
func (s *Service) CancelMediaJob(ctx context.Context, id string) (media.Job, error) {
	return s.cancelMediaJob(ctx, id, 0)
}

func (s *Service) cancelMediaJob(ctx context.Context, id string, clientKeyID uint64) (media.Job, error) {
	if s.mediaJobs == nil {
		return media.Job{}, ErrResponseNotFound
	}
	job, err := s.mediaJobs.CancelMediaJob(ctx, id, clientKeyID, time.Now().UTC())
	switch {
	case errors.Is(err, repository.ErrNotFound):
		return media.Job{}, ErrResponseNotFound
	case errors.Is(err, repository.ErrConflict):
		return job, ErrMediaJobNotCancellable
	case err != nil:
		return media.Job{}, err
	}
	if job.Status == media.StatusCanceled {
		// ponytail: canceled video inputs are left to the 24h input TTL.
		s.releaseJobReservations(job)
		// Images delivered before the cancel are billed; their audits may not
		// be written yet. Reconciliation retries a failed write.
		if job.Kind == media.KindImage {
			if err := s.recordImageJobAudits(context.WithoutCancel(ctx), job, 0); err != nil {
				s.logger.Error("image_usage_record_failed", "job_id", job.ID, "error", err)
			}
		}
	}
	return job, nil
}

// RetryMediaJob resubmits the undelivered part of an uncertain job as a new
// job of the same client key, deduplicated by idempotencyKey (default: one
// retry per job). A resumable failed image job is instead resumed in place
// and returned with its own ID.
func (s *Service) RetryMediaJob(ctx context.Context, id, idempotencyKey string) (media.Job, error) {
	if s.mediaJobs == nil {
		return media.Job{}, ErrResponseNotFound
	}
	jobs, err := s.mediaJobs.GetMediaJobsByIDs(ctx, []string{id})
	if err != nil {
		return media.Job{}, err
	}
	if len(jobs) == 0 {
		return media.Job{}, ErrResponseNotFound
	}
	job := jobs[0]
	resumable := ImageJobResumable(job)
	if job.Status != media.StatusUncertain && !resumable {
		return media.Job{}, ErrMediaJobNotRetryable
	}
	key, err := s.clientKeys.Get(ctx, job.ClientKeyID)
	if err != nil || !key.IsAvailable(time.Now()) {
		return media.Job{}, fmt.Errorf("%w: 客户端 Key 不可用", ErrMediaJobNotRetryable)
	}
	if resumable {
		return s.resumeImageJob(ctx, job, key)
	}
	inputJSON, err := s.mediaJobs.GetMediaJobInput(ctx, job.ID)
	if err != nil {
		return media.Job{}, err
	}
	if idempotencyKey == "" {
		idempotencyKey = "retry:" + job.ID
	}
	if job.Kind == media.KindImage {
		var inputs imageJobInputs
		_ = json.Unmarshal([]byte(inputJSON), &inputs)
		return s.CreateImageJob(ctx, ImageJobInput{
			RequestID: job.RequestID, ClientKey: key, PublicModel: job.Model, Prompt: job.Prompt,
			ImageURLs: inputs.ImageURLs, MaskURL: inputs.MaskURL, Count: job.ItemCount - len(job.ResultAssetIDs),
			Size: job.Size, Quality: job.Quality, OutputFormat: job.Params.OutputFormat, Background: job.Params.Background,
			ResponseModel: job.Params.ResponseModel, ResponseBackground: job.Params.ResponseBackground, IdempotencyKey: idempotencyKey,
		})
	}
	imageURL, referenceURLs, referenceAudios, videoURL := decodeVideoInputDetailed(inputJSON)
	return s.CreateVideo(ctx, VideoInput{
		RequestID: job.RequestID, ClientKey: key, PublicModel: job.Model, Operation: job.Operation, Prompt: job.Prompt,
		Duration: job.Seconds, AspectRatio: job.Size, Resolution: job.Quality, ImageURL: imageURL, ReferenceURLs: referenceURLs,
		ReferenceAudios: referenceAudios, VideoURL: videoURL, IdempotencyKey: idempotencyKey,
	})
}

// resumeImageJob sends a failed image job back to the workers to poll, fetch
// and save the image in flight from its checkpoint on the job's account; it
// is never resubmitted. The image's share of the job's price is reserved
// again under the resume's event, which its audit settles or its end releases.
func (s *Service) resumeImageJob(ctx context.Context, job media.Job, key clientkey.Key) (media.Job, error) {
	if err := s.checkLedgerReady(); err != nil {
		return media.Job{}, err
	}
	job.ResumeItem, job.ResumeGeneration = len(job.ResultAssetIDs), job.Generation
	eventID := imageItemEventID(job, job.ResumeItem)
	amount := int64(0)
	if job.Price != nil {
		amount = itemShare(job.Price.CostInUSDTicks, job.ItemCount, job.ResumeItem)
	}
	reserved, err := s.clientKeys.ReserveBilling(ctx, key, eventID, amount, mediaBillingReservationTTL)
	if err != nil {
		return media.Job{}, err
	}
	resumed, err := s.mediaJobs.ResumeMediaJob(ctx, job.ID, job.Generation, job.ResumeItem, time.Now().UTC())
	if err != nil {
		// A concurrent resume of the same generation shares the event and its
		// reservation: the repeated request resolves to that resume.
		shared := errors.Is(err, repository.ErrConflict) && resumed.ResumeGeneration == job.Generation && !resumed.Status.Terminal()
		if shared {
			return resumed, nil
		}
		if reserved {
			s.cancelBillingReservation(eventID)
		}
		switch {
		case errors.Is(err, repository.ErrConflict):
			return media.Job{}, fmt.Errorf("%w: 任务状态已变化", ErrMediaJobNotRetryable)
		case errors.Is(err, repository.ErrNotFound):
			return media.Job{}, ErrResponseNotFound
		}
		return media.Job{}, err
	}
	if !s.enqueueMediaJob(resumed.ID) {
		s.logger.Warn("image_job_queue_full", "job_id", resumed.ID)
	}
	return resumed, nil
}

// ImageJobOutput reads the delivered images and the usage estimate of a job.
func (s *Service) ImageJobOutput(ctx context.Context, job media.Job) ([]ImageJobImage, map[string]any, error) {
	if len(job.ResultAssetIDs) == 0 {
		return nil, nil, nil
	}
	if s.mediaAssets == nil {
		return nil, nil, errors.New("媒体存储未配置")
	}
	images := make([]ImageJobImage, 0, len(job.ResultAssetIDs))
	sizes := make([]image.Point, 0, len(job.ResultAssetIDs))
	for _, id := range job.ResultAssetIDs {
		asset, body, err := s.mediaAssets.OpenImage(ctx, id)
		if errors.Is(err, mediaapp.ErrAssetNotFound) {
			// Still delivered and billed: it stays in the usage estimate at the default size.
			images = append(images, ImageJobImage{Missing: true})
			sizes = append(sizes, image.Point{})
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		data, err := io.ReadAll(io.LimitReader(body, imageResultMaxBytes))
		_ = body.Close()
		if err != nil {
			return nil, nil, err
		}
		item := ImageJobImage{Data: data, MIMEType: asset.MIMEType}
		if config, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
			item.Width, item.Height = config.Width, config.Height
		}
		images = append(images, item)
		sizes = append(sizes, image.Pt(item.Width, item.Height))
	}
	var usage map[string]any
	if adapter, ok := s.providers.AsyncImages(accountdomain.Provider(job.Provider)); ok {
		usage = adapter.ImageUsage(job.Prompt, job.InputImageCount, job.Quality, sizes)
	}
	return images, usage, nil
}
