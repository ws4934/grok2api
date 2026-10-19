package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountsyncapp "github.com/chenyme/grok2api/backend/internal/application/accountsync"
	"github.com/gin-gonic/gin"
)

type accountSyncPipeline struct {
	ctx        context.Context
	cancel     context.CancelFunc
	ids        chan uint64
	done       chan accountsyncapp.Result
	progress   func(completed, total int)
	progressMu sync.Mutex
	queued     atomic.Int64
	completed  atomic.Int64
}

func (h *Handler) startSyncPipeline(parent context.Context, progress func(completed, total int)) *accountSyncPipeline {
	ctx, cancel := context.WithCancel(parent)
	pipeline := &accountSyncPipeline{ctx: ctx, cancel: cancel, progress: progress}
	if h.sync == nil {
		return pipeline
	}
	pipeline.ids = make(chan uint64, accountSyncQueueCapacity)
	pipeline.done = make(chan accountsyncapp.Result, 1)
	go func() {
		if observed, ok := h.sync.(accountSyncProgressor); ok && progress != nil {
			pipeline.done <- observed.SyncStreamObserved(ctx, pipeline.ids, func(completed, _ int) {
				pipeline.completed.Store(int64(completed))
				pipeline.reportProgress()
			})
			return
		}
		pipeline.done <- h.sync.SyncStream(ctx, pipeline.ids)
	}()
	return pipeline
}

func (p *accountSyncPipeline) Observe(accountID uint64) error {
	if p.ids == nil {
		return nil
	}
	p.queued.Add(1)
	select {
	case p.ids <- accountID:
		return nil
	case <-p.ctx.Done():
		p.queued.Add(-1)
		return p.ctx.Err()
	}
}

func (p *accountSyncPipeline) Finish(abort bool) accountsyncapp.Result {
	if abort {
		p.cancel()
	}
	if p.ids != nil {
		close(p.ids)
	}
	if !abort {
		// 转换阶段结束后不再增加同步任务，先报告一次固定分母，避免前端看到总数跳变。
		p.reportProgress()
	}
	result := accountsyncapp.Result{}
	if p.done != nil {
		result = <-p.done
	}
	if !abort && p.ids != nil {
		p.completed.Store(int64(result.Succeeded + result.Failed))
		p.reportProgress()
	}
	p.cancel()
	return result
}

// reportProgress 使用已进入同步流水线的任务数报告进度；转换结束后该分母保持固定。
func (p *accountSyncPipeline) reportProgress() {
	if p.ids == nil || p.progress == nil {
		return
	}
	p.progressMu.Lock()
	defer p.progressMu.Unlock()
	p.progress(int(p.completed.Load()), int(p.queued.Load()))
}

type accountTaskProgressResponse struct {
	Completed int    `json:"completed"`
	Total     int    `json:"total"`
	Phase     string `json:"phase,omitempty"`
}

func prepareAccountEventStream(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream; charset=utf-8")
	c.Header("Cache-Control", "no-cache, no-transform")
	c.Header("X-Accel-Buffering", "no")
}

type accountEventStream struct {
	context   *gin.Context
	mu        sync.Mutex
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

func newAccountEventStream(c *gin.Context) *accountEventStream {
	prepareAccountEventStream(c)
	stream := &accountEventStream{context: c, stop: make(chan struct{}), done: make(chan struct{})}
	_ = stream.writeComment("connected")
	go stream.heartbeat()
	return stream
}

func (s *accountEventStream) ProgressObserver() accountapp.BatchProgressObserver {
	return s.PhaseProgressObserver("", nil)
}

func (s *accountEventStream) PhaseProgressObserver(phase string, totalValue *atomic.Int64) accountapp.BatchProgressObserver {
	return func(completed, total int) error {
		if totalValue != nil {
			totalValue.Store(int64(total))
		}
		return s.Write("progress", accountTaskProgressResponse{Completed: completed, Total: total, Phase: phase})
	}
}

func (s *accountEventStream) SyncProgressObserver() func(completed, total int) {
	return func(completed, total int) {
		_ = s.Write("progress", accountTaskProgressResponse{Completed: completed, Total: total, Phase: "syncing"})
	}
}

func (s *accountEventStream) WriteError(code, message string) {
	_ = s.Write("error", gin.H{"code": code, "message": message})
}

func (s *accountEventStream) Write(event string, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := setAccountWriteDeadline(s.context.Writer); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.context.Writer, "event: %s\ndata: %s\n\n", event, payload); err != nil {
		return err
	}
	s.context.Writer.Flush()
	return s.context.Request.Context().Err()
}

func (s *accountEventStream) writeComment(comment string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := setAccountWriteDeadline(s.context.Writer); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.context.Writer, ": %s\n\n", comment); err != nil {
		return err
	}
	s.context.Writer.Flush()
	return s.context.Request.Context().Err()
}

func (s *accountEventStream) heartbeat() {
	defer close(s.done)
	ticker := time.NewTicker(accountEventHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-s.context.Request.Context().Done():
			return
		case <-ticker.C:
			if err := s.writeComment("heartbeat"); err != nil {
				return
			}
		}
	}
}

func (s *accountEventStream) Close() {
	s.closeOnce.Do(func() { close(s.stop) })
	<-s.done
}

func setAccountWriteDeadline(writer http.ResponseWriter) error {
	err := http.NewResponseController(writer).SetWriteDeadline(time.Now().Add(accountEventWriteTimeout))
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

// writeAccountEvent keeps the event encoder independently testable without starting a heartbeat.
func writeAccountEvent(c *gin.Context, event string, value any) error {
	return (&accountEventStream{context: c}).Write(event, value)
}

func (h *Handler) syncInitial(ctx context.Context, accountIDs ...uint64) accountsyncapp.Result {
	if h.sync == nil {
		return accountsyncapp.Result{}
	}
	return h.sync.Sync(ctx, accountIDs...)
}
