package channelhttp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/channel"
	"github.com/gin-gonic/gin"
)

// imageKeepAlive spaces the SSE comments sent while an image job runs.
var imageKeepAlive = 15 * time.Second

func startSSE(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	c.Status(200)
	c.Writer.Flush()
}

// sseEvent ignores write errors: a disconnect cancels the request context,
// which the job watcher handles.
func sseEvent(c *gin.Context, name string, v any) {
	b, _ := json.Marshal(v)
	_, _ = fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", name, b)
	c.Writer.Flush()
}

// watchImageJob polls a job until it ends, calling progress after every poll
// and writing keep-alive comments meanwhile. A client disconnect cancels the
// work that has not reached the upstream, as the synchronous path does.
func (h *Handler) watchImageJob(c *gin.Context, key uint64, job domain.ImageJob, progress func(domain.ImageJob) error) (domain.ImageJob, error) {
	ctx := c.Request.Context()
	defer func() {
		if ctx.Err() != nil && !terminal(job.Status) {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = h.service.CancelImageJob(cleanup, key, job.ID)
		}
	}()
	poll, alive := time.NewTicker(time.Second), time.NewTicker(imageKeepAlive)
	defer poll.Stop()
	defer alive.Stop()
	for {
		if e := progress(job); e != nil {
			return job, e
		}
		if terminal(job.Status) {
			return job, nil
		}
		select {
		case <-ctx.Done():
			return job, ctx.Err()
		case <-alive.C:
			_, _ = c.Writer.WriteString(": keep-alive\n\n")
			c.Writer.Flush()
		case <-poll.C:
			next, e := h.service.GetImageJob(ctx, key, job.ID)
			if e != nil {
				return job, e
			}
			job = next
		}
	}
}

// jobFailure describes a job that ended without a deliverable image.
func jobFailure(job domain.ImageJob) (code, message string) {
	code = "image_generation_failed"
	for _, item := range job.Items {
		if item.Status == "uncertain" {
			code = "result_uncertain"
		}
		if item.Error != "" && message == "" {
			message = item.Error
		}
	}
	if message == "" {
		message = "Image generation failed"
	}
	return code, message
}

// streamImages answers an Images request with stream: true. Each image is sent
// once stored; there are no partial images because the upstream has none.
func (h *Handler) streamImages(c *gin.Context, key uint64, job domain.ImageJob, format string) {
	ctx := c.Request.Context()
	event := "image_generation.completed"
	if job.Request.Operation == "image_edit" {
		event = "image_edit.completed"
	}
	startSSE(c)
	sent := map[string]bool{}
	job, e := h.watchImageJob(c, key, job, func(job domain.ImageJob) error {
		r := job.Request
		for _, item := range job.Items {
			if item.Status != "succeeded" || sent[item.ID] {
				continue
			}
			results, e := h.service.ImageItemResults(ctx, key, job.ID, item.ID, format)
			if e != nil {
				return e
			}
			sent[item.ID] = true
			for _, result := range results {
				body := gin.H{"type": event, "created_at": time.Now().Unix(), "size": imageSize(item, r), "quality": r.Quality, "background": r.Background, "output_format": r.OutputFormat, "usage": h.imageUsage(ctx, job, []domain.ImageItem{item})}
				if format == "url" {
					body["url"] = result.URL
				} else {
					body["b64_json"] = result.Base64
				}
				sseEvent(c, event, body)
			}
		}
		return nil
	})
	if ctx.Err() != nil || (e == nil && len(sent) > 0) {
		return
	}
	code, message := jobFailure(job)
	if e != nil {
		code, message = "result_unavailable", "Image result unavailable; inspect X-Image-Job-ID"
	}
	sseEvent(c, "error", gin.H{"type": "error", "error": gin.H{"type": "server_error", "code": code, "message": message, "param": nil}})
}
