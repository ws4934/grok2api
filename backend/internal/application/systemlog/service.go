package systemlog

import (
	"context"
	"sync"
	"time"

	logdomain "github.com/chenyme/grok2api/backend/internal/domain/systemlog"
	"github.com/chenyme/grok2api/backend/internal/pkg/diagnostic"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

const (
	RetentionDays = logdomain.RetentionDays
	MaxEntries    = logdomain.MaxEntries
	queueCapacity = 1024
	batchSize     = 100
)

type Status struct {
	QueueDepth    int        `json:"queueDepth"`
	Dropped       uint64     `json:"dropped"`
	WriteFailures uint64     `json:"writeFailures"`
	LastError     string     `json:"lastError"`
	LastFailureAt *time.Time `json:"lastFailureAt"`
	LastSavedAt   *time.Time `json:"lastSavedAt"`
	RetentionDays int        `json:"retentionDays"`
	MaxEntries    int        `json:"maxEntries"`
}

// Service isolates logging IO from inference. Full queues and failed writes are
// counted rather than blocking the request or recursively logging a sink error.
type Service struct {
	repo       repository.SystemLogRepository
	queue      chan logdomain.Entry
	stop, done chan struct{}
	mu         sync.Mutex
	closed     bool
	status     Status
}

func NewService(repo repository.SystemLogRepository) *Service {
	s := &Service{repo: repo, queue: make(chan logdomain.Entry, queueCapacity), stop: make(chan struct{}), done: make(chan struct{})}
	go s.run()
	return s
}

func (s *Service) Enqueue(entry logdomain.Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.queue <- entry:
	default:
		s.status.Dropped++
	}
}

func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := s.status
	result.QueueDepth = len(s.queue)
	result.RetentionDays = RetentionDays
	result.MaxEntries = MaxEntries
	return result
}

func (s *Service) List(ctx context.Context, query logdomain.Query) ([]logdomain.Entry, bool, error) {
	return s.repo.ListSystemLogs(ctx, query)
}

func (s *Service) Close() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.stop)
	}
	s.mu.Unlock()
	<-s.done
}

func (s *Service) run() {
	defer close(s.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	retentionTicker := time.NewTicker(time.Minute)
	defer retentionTicker.Stop()
	pending := make([]logdomain.Entry, 0, batchSize)
	flush := func() {
		if len(pending) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := s.repo.AppendSystemLogs(ctx, pending)
		cancel()
		now := time.Now().UTC()
		s.mu.Lock()
		if err != nil {
			s.status.Dropped += uint64(len(pending))
			s.status.WriteFailures++
			s.status.LastError = diagnostic.Text(err.Error(), 1024)
			s.status.LastFailureAt = &now
		} else {
			s.status.LastSavedAt = &now
		}
		s.mu.Unlock()
		pending = pending[:0]
	}
	prune := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := s.repo.PruneSystemLogs(ctx, time.Now().UTC().AddDate(0, 0, -RetentionDays), MaxEntries); err != nil {
			now := time.Now().UTC()
			s.mu.Lock()
			s.status.WriteFailures++
			s.status.LastError = diagnostic.Text(err.Error(), 1024)
			s.status.LastFailureAt = &now
			s.mu.Unlock()
		}
	}
	// Appends enforce retention atomically. Also prune during idle periods.
	prune()
	for {
		select {
		case entry := <-s.queue:
			pending = append(pending, entry)
			if len(pending) >= batchSize {
				flush()
			}
		case <-ticker.C:
			if len(pending) > 0 {
				flush()
			}
		case <-retentionTicker.C:
			prune()
		case <-s.stop:
			for {
				select {
				case entry := <-s.queue:
					pending = append(pending, entry)
				default:
					flush()
					return
				}
			}
		}
	}
}
