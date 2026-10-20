package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func (s *Service) runVideoJob(parent context.Context, job media.Job, route model.Route) {
	ctx, lose := context.WithCancelCause(parent)
	defer lose(nil)
	ctx, cancel := context.WithTimeout(ctx, videoJobTimeout)
	ctx, egressTrace := infraegress.WithTrace(ctx)
	heartbeat, claim := make(chan struct{}), job
	go func() {
		defer close(heartbeat)
		s.keepMediaJobLease(ctx, lose, claim)
	}()
	defer func() {
		cancel()
		<-heartbeat
	}()
	startedAt := time.Now()
	job.Progress = max(job.Progress, 1)
	if err := s.saveMediaJob(ctx, &job); err != nil {
		s.logger.Warn("video_job_progress_write_failed", "job_id", job.ID, "error", err)
	}
	inputReferences := decodeVideoInput(job.InputJSON)
	releaseInputSlot, err := s.acquireVideoInputSlot(ctx, inputReferences)
	if err != nil {
		s.deferVideoJob(parent, job)
		return
	}
	defer releaseInputSlot()
	adapter, ok := s.providers.Videos(route.Provider)
	if !ok {
		s.failVideoJob(parent, job, "provider_unavailable", ErrNoAvailableAccount, 0, nil)
		return
	}
	operation := provider.VideoOperation(job.Operation)
	inputs, err := s.resolveVideoJobInputs(ctx, operation, job.InputJSON)
	if err != nil {
		s.failVideoJob(parent, job, "input_unavailable", err, 0, nil)
		return
	}

	quotaMode := s.providers.VideoQuotaMode(route.Provider, s.providers.QuotaMode(route.Provider, route.UpstreamModel), job.Quality)
	// The initial creation path (selectSchedulableEligibleMediaRouteWithQuotaMode,
	// called from CreateVideoJob) always scopes account selection to the
	// requesting client key's AccountScope. A retry here must not silently
	// widen that: falling back to the unscoped account pool would let a key
	// restricted to e.g. free-tier/web-only accounts consume a premium or
	// console/build-only account's quota purely because its originally
	// pinned account had a transient failure.
	accountScope := clientkey.AccountScope{}
	if job.ClientKeyID > 0 {
		key, keyErr := s.clientKeys.Get(ctx, job.ClientKeyID)
		if keyErr != nil {
			if parent.Err() != nil {
				s.deferVideoJob(parent, job)
				return
			}
			s.failVideoJob(ctx, job, "client_key_unavailable", keyErr, 0, nil)
			return
		}
		accountScope = key.AccountScope()
	}
	attemptPolicy := s.videoAttemptPolicy()
	excluded := make(map[uint64]bool)
	forbiddenEgressRetried := make(map[uint64]bool)
	var retryPinnedAccountID uint64
	failureAttempts := newFailureAttemptRecorder(http.MethodPost, "/videos/generations")
	var selection *selectionSession
	var lease *accountLease
	var result provider.VideoResult
	var lastErr error

	for attempt := 0; attemptPolicy.allows(attempt); attempt++ {
		attemptStarted := time.Now()
		err = nil
		if lease != nil {
			lease.Release()
			lease = nil
		}
		// First try stays on the account chosen when the local job was created.
		// Create-stage failures may switch accounts; poll failures never reach here as retries.
		pinnedAccountID := retryPinnedAccountID
		retryPinnedAccountID = 0
		if attempt == 0 {
			pinnedAccountID = job.AccountID
		}
		if attempt == 0 && job.Submitted {
			// The checkpoint belongs to the account that submitted it: resume
			// there without a scheduling slot, or not at all.
			credential, resumeErr := s.jobCredential(ctx, job)
			if resumeErr != nil {
				if parent.Err() != nil {
					s.deferVideoJob(parent, job)
					return
				}
				s.endVideoJob(parent, job, media.StatusUncertain, "account_unavailable", errors.New("提交任务的账号已不可用，无法恢复上游任务"), 0, nil)
				return
			}
			lease = &accountLease{Credential: credential, QuotaMode: quotaMode}
		} else if pinnedAccountID > 0 && !excluded[pinnedAccountID] {
			lease, err = s.selector.AcquirePinnedForKey(ctx, route.Provider, pinnedAccountID, route.ID, route.UpstreamModel, quotaMode, true, accountScope)
			if err != nil {
				excluded[pinnedAccountID] = true
				lease = nil
			}
		}
		if lease == nil {
			if selection == nil {
				selection, err = s.selector.beginSelectionSessionForKey(ctx, route.Provider, route.ID, route.UpstreamModel, quotaMode, "", excluded, false, accountScope)
			}
			if err == nil {
				lease, err = selection.Acquire(ctx, excluded, false)
			}
		}
		if err != nil {
			if parent.Err() != nil {
				s.deferVideoJob(parent, job)
				return
			}
			if lastErr == nil {
				lastErr = err
			}
			s.failVideoJob(parent, job, "account_unavailable", lastErr, 0, failureAttempts.snapshot())
			return
		}
		excluded[lease.Credential.ID] = true
		credential, credErr := s.accounts.EnsureCredential(ctx, lease.Credential, false)
		if credErr != nil {
			failureAttempts.captureCredentialFailure(lease.Credential, attemptStarted, false, credErr)
			lastErr = credErr
			if attemptPolicy.hasNext(attempt) {
				continue
			}
			s.failVideoJob(parent, job, "account_unavailable", credErr, 0, failureAttempts.snapshot())
			return
		}
		lease.Credential = credential
		if job.AccountID != credential.ID || job.AccountName != credential.Name {
			job.AccountID, job.AccountName = credential.ID, credential.Name
			_ = s.saveMediaJob(ctx, &job)
		}

		lastProgress := job.Progress
		originCtx := media.WithOrigin(ctx, media.JobOrigin(job, string(audit.OperationVideo)))
		result, err = adapter.GenerateVideo(originCtx, provider.VideoRequest{
			Credential: lease.Credential, Billing: lease.Billing, JobID: job.ID, Model: route.UpstreamModel,
			Operation: operation,
			Prompt:    job.Prompt, Duration: job.Seconds, AspectRatio: job.Size, Resolution: job.Quality,
			ImageURL: inputs.imageURL, LastFrameURL: inputs.lastFrameURL, ReferenceURLs: inputs.referenceURLs,
			ReferenceVideoURLs: inputs.referenceVideoURLs, ReferenceAudios: inputs.referenceAudios, VideoURL: inputs.videoURL,
			Checkpoint: job.Checkpoint,
			SaveCheckpoint: func(value string) error {
				job.Checkpoint, job.Submitted = value, value != ""
				return s.saveMediaJob(ctx, &job)
			},
			Progress: func(value int) {
				value = min(99, max(1, value))
				if value-lastProgress < 5 {
					return
				}
				lastProgress = value
				job.Progress = value
				_ = s.saveMediaJob(ctx, &job)
			},
		})
		if err == nil && result.AssetID == "" && result.URL != "" {
			result, err = s.persistRemoteVideo(originCtx, job.ID, adapter, lease.Credential, result)
		}
		if err == nil {
			break
		}
		if errors.Is(err, errJobLost) || errors.Is(context.Cause(ctx), errJobLost) {
			// Canceled, or recovered by another worker: the job is no longer ours.
			return
		}
		lastErr = err
		captureVideoAttempt(failureAttempts, lease.Credential, attemptStarted, err)
		if parent.Err() != nil {
			s.deferVideoJob(parent, job)
			return
		}

		failureCtx, failureCancel := context.WithTimeout(context.Background(), finalizationTimeout)
		failureHandled := false
		retriableCreate := false
		stage, hasStage := provider.VideoErrorStage(err)
		safeCreateFailure := hasStage && stage == provider.VideoStageCreate
		status, hasStatus := provider.ErrorHTTPStatus(err)
		if errors.Is(err, provider.ErrUnauthorized) {
			if lease.Credential.AuthType == account.AuthTypeSSO {
				s.markSSOCredentialRejected(failureCtx, lease.Credential, fmt.Sprintf("%s SSO credential rejected", lease.Credential.Provider))
			}
			failureHandled = true
			retriableCreate = safeCreateFailure
		} else if hasStatus {
			switch {
			case provider.IsRequestScopedError(err):
				// The request itself was refused; any account would be refused the
				// same way, so it neither cools this account down nor fails over.
				failureHandled = true
			case status == http.StatusUnauthorized && lease.Credential.AuthType == account.AuthTypeSSO:
				s.markSSOCredentialRejected(failureCtx, lease.Credential, fmt.Sprintf("%s SSO credential rejected", lease.Credential.Provider))
				failureHandled = true
				retriableCreate = safeCreateFailure
			case status == http.StatusForbidden && s.providers.RetryForbiddenAsEgress(lease.Credential.Provider):
				// Web anti-bot 403 is egress-scoped. Retry the same account once so
				// egress invalidation can rebuild the route, then move on normally.
				failureHandled = true
				if safeCreateFailure {
					if !forbiddenEgressRetried[lease.Credential.ID] {
						forbiddenEgressRetried[lease.Credential.ID] = true
						retryPinnedAccountID = lease.Credential.ID
						delete(excluded, lease.Credential.ID)
					}
					retriableCreate = true
				}
			case status == http.StatusForbidden && s.providers.Failure(lease.Credential.Provider).AuthenticatedForbidden:
				// A paid Build account is entitled to video, so its 403 is request-level.
				if !account.IsBuildSuper(lease.Credential, lease.Billing) {
					s.selector.MarkFailure(failureCtx, lease.Credential, status, 0)
				}
				failureHandled = true
				retriableCreate = safeCreateFailure && !account.IsBuildSuper(lease.Credential, lease.Billing)
			case (status == http.StatusPaymentRequired || status == http.StatusTooManyRequests) && lease.QuotaMode != "":
				state, reconcileErr := s.accounts.ReconcileRateLimit(failureCtx, lease.Credential.ID, lease.QuotaMode, 0)
				s.applyRateLimitReconciliation(failureCtx, lease.Credential, status, 0, state, reconcileErr)
				failureHandled = true
				retriableCreate = safeCreateFailure
			case status == http.StatusTooManyRequests || status == http.StatusPaymentRequired:
				s.selector.MarkFailure(failureCtx, lease.Credential, status, 0)
				failureHandled = true
				retriableCreate = safeCreateFailure
			case status >= http.StatusInternalServerError:
				// A 5xx may be returned after the upstream accepted the job. Never
				// switch accounts without an idempotency guarantee.
				failureHandled = true
				retriableCreate = false
			default:
				s.selector.MarkFailure(failureCtx, lease.Credential, status, 0)
				failureHandled = true
				retriableCreate = false
			}
		}
		if !failureHandled && !provider.IsMediaPostProcessingError(err) {
			s.selector.MarkFailure(failureCtx, lease.Credential, 0, 0)
			retriableCreate = safeCreateFailure
		}
		failureCancel()
		applyMediaJobEgress(&job, egressTrace, route.Provider)
		s.logVideoGenerationFailure(job, lease.Credential, err)

		// Poll/post-processing failures are bound to the already-created upstream job.
		if provider.IsMediaPostProcessingError(err) || (hasStage && stage == provider.VideoStagePoll) || !retriableCreate || !attemptPolicy.hasNext(attempt) {
			if hasStage && stage == provider.VideoStageSubmitted {
				// The create request may have reached the upstream without returning an ID.
				s.endVideoJob(parent, job, media.StatusUncertain, "submission_uncertain", err, provider.ErrorHTTPStatusOrZero(err), failureAttempts.snapshot())
				return
			}
			failureCode, publicErr := "generation_failed", err
			upstreamStatus := 0
			if hasStatus {
				upstreamStatus = status
			}
			if errors.Is(err, provider.ErrUnauthorized) || (hasStatus && (status == http.StatusUnauthorized || status == http.StatusForbidden)) {
				failureCode, publicErr = "provider_unavailable", errors.New("上游服务暂不可用")
			} else if hasStatus && status == http.StatusTooManyRequests {
				failureCode = "rate_limited"
			}
			s.failVideoJob(parent, job, failureCode, publicErr, upstreamStatus, failureAttempts.snapshot())
			return
		}
		// Create-stage failure: switch account according to runtime attempt policy.
		continue
	}
	if lease == nil {
		s.failVideoJob(parent, job, "account_unavailable", ErrNoAvailableAccount, 0, failureAttempts.snapshot())
		return
	}
	defer lease.Release()

	// Provider 已消费请求体，尽早释放 Base64 物化名额和大字符串。
	inputs = videoInputs{}
	releaseInputSlot()

	if result.Seconds > 0 && result.Seconds != job.Seconds {
		// The provider generated a shorter video (the Web free-tier cap): record
		// and bill what was delivered.
		job.Seconds = result.Seconds
		if pricing, ok := s.resolveVideoPricing(operation, route.Provider, route.UpstreamModel, job.Quality, job.Seconds, job.InputImageCount); ok {
			job.Price = &pricing
		}
	}
	now := time.Now().UTC()
	job.Status, job.Progress, job.UpstreamURL, job.ContentType = media.StatusCompleted, 100, result.URL, result.ContentType
	// 成功终态必须清空历史错误字段，避免管理端/恢复路径把中间失败文案当成最终结果。
	job.ErrorCode, job.ErrorMessage = "", ""
	if result.AssetID != "" {
		job.ResultAssetID = result.AssetID
	}
	applyMediaJobEgress(&job, egressTrace, route.Provider)
	job.LeaseUntil, job.UpdatedAt, job.CompletedAt = nil, now, &now
	if err := s.persistVideoJobWithRetry(parent, job); err != nil {
		s.logger.Error("video_job_terminal_write_failed", "job_id", job.ID, "error", err)
		return
	}
	s.selector.markSuccess(context.Background(), lease.Credential, lease.QuotaProbe)
	s.consumeMediaQuota(route, job.AccountID, lease.QuotaMode)
	if err := s.recordVideoAudit(context.Background(), job, time.Since(startedAt).Milliseconds(), http.StatusOK, failureAttempts.snapshot()); err != nil {
		s.logger.Error("video_usage_record_failed", "job_id", job.ID, "event_id", "video_usage_"+job.ID, "error", err)
	}
	// 输入回收放在账号状态、计费和审计收尾之后，存储抖动不得延迟关键终态逻辑。
	s.releaseVideoInputs(job)
}

// persistRemoteVideo 只重试已经生成的视频结果下载与本地归档，不重新调用生成接口，
// 且所有尝试固定使用创建任务的同一凭据。
func (s *Service) persistRemoteVideo(ctx context.Context, jobID string, adapter provider.VideoAdapter, credential account.Credential, result provider.VideoResult) (provider.VideoResult, error) {
	if s.mediaAssets == nil {
		return result, provider.NewMediaPostProcessingError(provider.MediaPostProcessingStorage, errors.New("视频媒体存储未配置"))
	}
	downloader, ok := adapter.(provider.VideoContentDownloader)
	if !ok {
		return result, provider.NewMediaPostProcessingError(provider.MediaPostProcessingDownload, errors.New("Provider 不支持视频内容下载"))
	}
	var lastErr error
	for attempt := 0; attempt < videoOutputAttempts; attempt++ {
		body, contentType, _, downloadErr := downloader.DownloadVideo(ctx, credential, result.URL)
		if downloadErr != nil {
			lastErr = provider.NewMediaPostProcessingError(provider.MediaPostProcessingDownload, downloadErr)
		} else {
			asset, saveErr := s.mediaAssets.SaveVideo(ctx, jobID, contentType, body)
			_ = body.Close()
			if saveErr == nil {
				result.AssetID = asset.ID
				result.ContentType = asset.MIMEType
				return result, nil
			}
			lastErr = provider.NewMediaPostProcessingError(provider.MediaPostProcessingStorage, saveErr)
		}
		if ctx.Err() != nil || attempt+1 >= videoOutputAttempts {
			break
		}
		if waitErr := waitVideoOutputRetry(ctx, attempt); waitErr != nil {
			return result, waitErr
		}
	}
	return result, lastErr
}

func waitVideoOutputRetry(ctx context.Context, attempt int) error {
	delays := [...]time.Duration{200 * time.Millisecond, 750 * time.Millisecond}
	timer := time.NewTimer(delays[min(attempt, len(delays)-1)])
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *Service) failVideoJob(ctx context.Context, job media.Job, code string, err error, upstreamStatus int, attempts []audit.Attempt) {
	s.endVideoJob(ctx, job, media.StatusFailed, code, err, upstreamStatus, attempts)
}

// endVideoJob records a failed or uncertain outcome. Uncertain jobs keep
// their inputs so an administrator can retry them.
func (s *Service) endVideoJob(ctx context.Context, job media.Job, status media.Status, code string, err error, upstreamStatus int, attempts []audit.Attempt) {
	now := time.Now().UTC()
	job.Status, job.ErrorCode, job.ErrorMessage = status, code, jobErrorMessage(err)
	job.LeaseUntil, job.UpdatedAt, job.CompletedAt = nil, now, &now
	if updateErr := s.persistVideoJobWithRetry(ctx, job); updateErr != nil {
		s.logger.Error("video_job_terminal_write_failed", "job_id", job.ID, "error", updateErr)
		return
	}
	if auditErr := s.recordVideoAudit(context.Background(), job, max(int64(0), now.Sub(job.CreatedAt).Milliseconds()), upstreamStatus, attempts); auditErr != nil {
		s.logger.Error("video_usage_record_failed", "job_id", job.ID, "event_id", "video_usage_"+job.ID, "error", auditErr)
	}
	s.cancelBillingReservation("video_usage_" + job.ID)
	if status != media.StatusUncertain {
		s.releaseVideoInputs(job)
	}
}

func (s *Service) videoAttemptPolicy() routingAttemptPolicy {
	return newRoutingAttemptPolicy(int(s.videoMaxAttempts.Load()))
}

func (s *Service) logVideoGenerationFailure(job media.Job, credential account.Credential, err error) {
	logger := s.logger
	if logger == nil {
		logger = slog.Default()
	}
	attributes := []any{
		"job_id", job.ID,
		"request_id", job.RequestID,
		"account_id", credential.ID,
		"provider", credential.Provider,
		"model", job.UpstreamModel,
		"egress_scope", job.EgressScope,
		"egress_mode", job.EgressMode,
		"error", sanitizeDiagnosticText(err.Error(), 512),
	}
	if status, ok := provider.ErrorHTTPStatus(err); ok {
		attributes = append(attributes, "upstream_status", status)
	}
	if job.EgressNodeID != nil {
		attributes = append(attributes, "egress_node_id", *job.EgressNodeID, "egress_node_name", job.EgressNodeName)
	}
	logger.Warn("video_generation_failed", attributes...)
}

func (s *Service) deferVideoJob(ctx context.Context, job media.Job) {
	now := time.Now().UTC()
	leaseUntil := now.Add(5 * time.Minute)
	job.Status = media.StatusInProgress
	job.LeaseUntil = &leaseUntil
	job.UpdatedAt = now
	job.ErrorCode = ""
	job.ErrorMessage = ""
	if err := s.persistVideoJobWithRetry(ctx, job); err != nil {
		s.logger.Error("video_job_defer_write_failed", "job_id", job.ID, "error", err)
	}
}

// persistVideoJobWithRetry 至少执行一次收尾写入；后续退避可被工作进程关闭信号取消。
func (s *Service) persistVideoJobWithRetry(ctx context.Context, job media.Job) error {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		lastErr = s.mediaJobs.UpdateMediaJob(writeCtx, job)
		cancel()
		if lastErr == nil {
			return nil
		}
		if attempt < 3 {
			timer := time.NewTimer(time.Duration(attempt) * 100 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return errors.Join(lastErr, ctx.Err())
			case <-timer.C:
			}
		}
	}
	return lastErr
}
