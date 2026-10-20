package gateway

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/batch"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// RecoverMediaJobs records missing usage of finished jobs and requeues jobs
// that were never started or whose worker lost its lease.
func (s *Service) RecoverMediaJobs(ctx context.Context) error {
	if s.mediaJobs == nil {
		return nil
	}
	usageErr := s.reconcileMediaUsage(ctx)
	values, err := s.mediaJobs.ListRecoverableMediaJobs(ctx, 1000)
	if err != nil {
		return errors.Join(usageErr, err)
	}
	for _, job := range values {
		// Image and video jobs queue apart: one full queue must not hold back the other.
		_ = s.enqueueMediaJob(job.ID)
	}
	return usageErr
}

// RunMediaWorkers 使用固定 Worker 处理持久化任务，避免突发请求按任务创建无界 goroutine。
func (s *Service) RunMediaWorkers(ctx context.Context) {
	if s.mediaQueue == nil || s.mediaWorker <= 0 {
		return
	}
	var workers sync.WaitGroup
	for _, queue := range []chan string{s.mediaQueue, s.imageQueue} {
		if queue != nil {
			s.runMediaQueue(ctx, queue, &workers)
		}
	}
	workers.Wait()
}

// runMediaQueue starts the mediaWorker workers of one queue.
func (s *Service) runMediaQueue(ctx context.Context, queue chan string, workers *sync.WaitGroup) {
	workers.Add(s.mediaWorker)
	for range s.mediaWorker {
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case id := <-queue:
					err := batch.Do(ctx, func(workCtx context.Context) error {
						s.processMediaJob(workCtx, id)
						return nil
					})
					s.mediaMu.Lock()
					delete(s.mediaQueued, id)
					s.mediaMu.Unlock()
					if err != nil && ctx.Err() == nil {
						if panicErr, ok := err.(*batch.PanicError); ok {
							s.logger.Error("media_worker_panicked", "job_id", id, "error", panicErr, "stack", string(panicErr.Stack))
						} else {
							s.logger.Error("media_worker_failed", "job_id", id, "error", err)
						}
					}
				}
			}
		}()
	}
}

func (s *Service) enqueueMediaJob(id string) bool {
	if id == "" || s.mediaQueue == nil {
		return false
	}
	queue := s.mediaQueue
	if s.imageQueue != nil && strings.HasPrefix(id, ImageJobIDPrefix) {
		queue = s.imageQueue
	}
	s.mediaMu.Lock()
	if _, exists := s.mediaQueued[id]; exists {
		s.mediaMu.Unlock()
		return true
	}
	s.mediaQueued[id] = struct{}{}
	s.mediaMu.Unlock()
	select {
	case queue <- id:
		return true
	default:
		s.mediaMu.Lock()
		delete(s.mediaQueued, id)
		s.mediaMu.Unlock()
		full := s.mediaQueueFull.Add(1)
		if s.logger != nil && (full == 1 || full%100 == 0) {
			s.logger.Warn("media_queue_full", "count", full, "queued", len(queue), "capacity", cap(queue))
		}
		return false
	}
}

// processMediaJob claims a job and runs the executor of its kind.
func (s *Service) processMediaJob(ctx context.Context, id string) {
	job, claimed, err := s.claimMediaJob(ctx, id)
	if err != nil {
		s.logger.Warn("media_job_claim_failed", "job_id", id, "error", err)
		return
	}
	if !claimed {
		return
	}
	route, err := s.models.Get(ctx, job.ModelRouteID)
	if err != nil && !errors.Is(err, repository.ErrNotFound) {
		// A failed lookup is no missing route: recovery retries once the lease ends.
		s.logger.Warn("media_job_route_lookup_failed", "job_id", id, "error", err)
		return
	}
	end := func(status media.Status, code string, cause error) {
		if job.Kind == media.KindImage {
			s.endImageJob(ctx, &job, status, code, cause, 0)
		} else {
			s.endVideoJob(ctx, job, status, code, cause, 0, nil)
		}
	}
	switch {
	case err != nil:
		end(media.StatusFailed, "model_not_found", errors.New("模型路由不存在"))
	case job.Generation-job.ResumeGeneration > mediaJobMaxClaims:
		status := media.StatusFailed
		if job.Submitted {
			status = media.StatusUncertain
		}
		end(status, "recovery_exhausted", errors.New("任务多次恢复仍未完成"))
	case job.Kind == media.KindImage:
		s.runImageJob(ctx, job, route)
	default:
		s.runVideoJob(ctx, job, route)
	}
}

// RunMediaRecovery 周期认领新建后未启动或执行实例失联后的媒体任务。
func (s *Service) RunMediaRecovery(ctx context.Context) {
	ticker := time.NewTicker(videoJobRecoveryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.RecoverMediaJobs(ctx); err != nil {
				s.logger.Warn("media_job_recovery_failed", "error", err)
			}
		}
	}
}

// claimMediaJob takes the job with the lease its worker's heartbeat renews,
// so a dead worker's job is recovered within minutes.
func (s *Service) claimMediaJob(ctx context.Context, id string) (media.Job, bool, error) {
	now := time.Now().UTC()
	claimToken, err := security.NewOpaqueToken(18)
	if err != nil {
		return media.Job{}, false, err
	}
	return s.mediaJobs.TryClaimMediaJob(ctx, id, now, now.Add(mediaJobLease), claimToken)
}
