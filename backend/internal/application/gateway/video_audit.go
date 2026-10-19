package gateway

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func (s *Service) reconcileMediaUsage(ctx context.Context) error {
	jobs, err := s.mediaJobs.ListUnrecordedTerminalMediaJobs(ctx, 200)
	if err != nil {
		return err
	}
	var result error
	for _, job := range jobs {
		if job.Kind == media.KindImage {
			if err := s.recordImageJobAudits(ctx, job, 0); err != nil {
				result = firstError(result, fmt.Errorf("任务 %s: %w", job.ID, err))
			}
			continue
		}
		durationMS := int64(0)
		if job.CompletedAt != nil {
			durationMS = max(int64(0), job.CompletedAt.Sub(job.CreatedAt).Milliseconds())
		}
		if err := s.recordVideoAudit(ctx, job, durationMS, 0, nil); err != nil {
			result = firstError(result, fmt.Errorf("任务 %s: %w", job.ID, err))
		}
	}
	return result
}

func (s *Service) recordVideoAudit(ctx context.Context, job media.Job, durationMS int64, upstreamStatus int, attempts []audit.Attempt) error {
	var accountID *uint64
	if job.AccountID > 0 {
		value := job.AccountID
		accountID = &value
	}
	createdAt := time.Now().UTC()
	if job.CompletedAt != nil && !job.CompletedAt.IsZero() {
		createdAt = job.CompletedAt.UTC()
	}
	statusCode := resolveVideoAuditStatusCode(job, upstreamStatus, attempts)
	record := audit.Record{
		EventID: "video_usage_" + job.ID, RequestID: job.RequestID, ClientKeyID: job.ClientKeyID, ClientKeyName: job.ClientKeyName,
		ClientIP:     job.ClientIP,
		ModelRouteID: job.ModelRouteID, ModelPublicID: job.Model, ModelUpstreamModel: job.UpstreamModel,
		Provider: job.Provider, Operation: audit.OperationVideo, UsageSource: audit.UsageSourceNone,
		AccountID: accountID, AccountName: job.AccountName, StatusCode: statusCode, ErrorCode: job.ErrorCode,
		EgressNodeID: job.EgressNodeID, EgressNodeName: job.EgressNodeName, EgressScope: job.EgressScope, EgressMode: audit.EgressMode(job.EgressMode),
		MediaInputImages: int64(job.InputImageCount),
		DurationMS:       durationMS, AttemptCount: len(attempts), Attempts: append([]audit.Attempt(nil), attempts...), CreatedAt: createdAt,
		RequestMethod: http.MethodPost, RequestPath: "/v1/videos/generations",
	}
	if job.Status == media.StatusCompleted && job.Seconds > 0 {
		record.MediaOutputSeconds = int64(max(0, job.Seconds))
	}
	operation := provider.VideoOperation(job.Operation)
	if operation == "" {
		operation = provider.VideoOperationGenerate
	}
	// Settle at the quote reserved at submission; jobs created before price
	// snapshots fall back to the current price.
	if job.Status == media.StatusCompleted && operation == provider.VideoOperationGenerate {
		if job.Price != nil {
			if job.Price.Model != "" {
				job.Price.Apply(&record)
			}
		} else if pricing, ok := s.pricing.QuoteVideo(account.Provider(job.Provider), job.UpstreamModel, job.Quality, job.Seconds, job.InputImageCount); ok {
			pricing.Apply(&record)
		}
	}
	if durable, ok := s.audits.(interface {
		CreateDurable(context.Context, audit.Record) error
	}); ok {
		if err := durable.CreateDurable(ctx, record); err != nil {
			return err
		}
	} else if err := s.audits.Create(ctx, record); err != nil {
		return err
	}
	markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	return s.mediaJobs.MarkMediaJobUsageRecorded(markCtx, job.ID, time.Now().UTC())
}

func resolveVideoAuditStatusCode(job media.Job, upstreamStatus int, attempts []audit.Attempt) int {
	if job.Status == media.StatusCompleted {
		return http.StatusOK
	}
	if upstreamStatus >= 100 && upstreamStatus <= 599 {
		return upstreamStatus
	}
	for i := len(attempts) - 1; i >= 0; i-- {
		if attempts[i].UpstreamStatusCode != nil {
			status := *attempts[i].UpstreamStatusCode
			if status >= 100 && status <= 599 {
				return status
			}
		}
	}
	if status := parseUpstreamStatusFromMessage(job.ErrorMessage); status > 0 {
		return status
	}
	switch job.ErrorCode {
	case "account_unavailable", "provider_unavailable":
		return http.StatusServiceUnavailable
	case "model_not_found":
		return http.StatusNotFound
	case "rate_limited":
		return http.StatusTooManyRequests
	default:
		return http.StatusBadGateway
	}
}

func parseUpstreamStatusFromMessage(message string) int {
	message = strings.TrimSpace(message)
	if message == "" {
		return 0
	}
	// Console/Web summaries commonly look like: "Console 媒体上游返回 429: ..."
	for _, token := range []string{"返回 ", "status ", "Status ", "HTTP "} {
		if idx := strings.Index(message, token); idx >= 0 {
			rest := strings.TrimSpace(message[idx+len(token):])
			n := 0
			for i := 0; i < len(rest); i++ {
				ch := rest[i]
				if ch < '0' || ch > '9' {
					break
				}
				n = n*10 + int(ch-'0')
				if n > 599 {
					return 0
				}
			}
			if n >= 100 && n <= 599 {
				return n
			}
		}
	}
	return 0
}

func captureVideoAttempt(recorder *failureAttemptRecorder, credential account.Credential, startedAt time.Time, err error) {
	if recorder == nil || err == nil {
		return
	}
	status, hasStatus := provider.ErrorHTTPStatus(err)
	stage := "upstream_response"
	if videoStage, ok := provider.VideoErrorStage(err); ok {
		stage = "video_" + string(videoStage)
	}
	attempt := audit.Attempt{
		Source:         audit.AttemptSourceUpstreamHTTP,
		Stage:          stage,
		AccountID:      auditAccountID(credential.ID),
		AccountName:    credential.Name,
		Method:         recorder.method,
		RequestPath:    recorder.path,
		StartedAt:      startedAt.UTC(),
		DurationMS:     time.Since(startedAt).Milliseconds(),
		TransportError: sanitizeDiagnosticText(err.Error(), diagnosticTextLimit),
		ErrorChain:     errorFrames(err),
	}
	if hasStatus {
		value := status
		attempt.UpstreamStatusCode = &value
		attempt.UpstreamStatus = http.StatusText(status)
		body := []byte(sanitizeDiagnosticText(err.Error(), diagnosticTextLimit))
		attempt.ResponseBody = body
	}
	recorder.append(attempt)
}
