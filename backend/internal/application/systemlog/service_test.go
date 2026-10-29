package systemlog

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	logdomain "github.com/chenyme/grok2api/backend/internal/domain/systemlog"
)

type testRepo struct {
	mu               sync.Mutex
	entries          []logdomain.Entry
	err              error
	started, release chan struct{}
	once             sync.Once
}

func (r *testRepo) AppendSystemLogs(ctx context.Context, values []logdomain.Entry) error {
	if r.started != nil {
		r.once.Do(func() { close(r.started) })
		select {
		case <-r.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.entries = append(r.entries, values...)
	return nil
}
func (r *testRepo) ListSystemLogs(context.Context, logdomain.Query) ([]logdomain.Entry, bool, error) {
	return nil, false, nil
}
func (r *testRepo) PruneSystemLogs(context.Context, time.Time, int) error { return nil }

func TestSystemLogsFlushOnCloseAndCanReadAfterward(t *testing.T) {
	repo := &testRepo{}
	service := NewService(repo)
	for i := range 20 {
		service.Enqueue(logdomain.Entry{Message: "entry", AccountID: uint64(i)})
	}
	service.Close()
	service.Close()
	service.Enqueue(logdomain.Entry{Message: "ignored"})
	if len(repo.entries) != 20 || service.Status().LastSavedAt == nil {
		t.Fatalf("entries=%d status=%+v", len(repo.entries), service.Status())
	}
}
func TestFullLogQueueDoesNotBlockBusiness(t *testing.T) {
	repo := &testRepo{started: make(chan struct{}), release: make(chan struct{})}
	service := NewService(repo)
	for range batchSize {
		service.Enqueue(logdomain.Entry{})
	}
	select {
	case <-repo.started:
	case <-time.After(time.Second):
		t.Fatal("writer did not start")
	}
	started := time.Now()
	for range queueCapacity + 300 {
		service.Enqueue(logdomain.Entry{})
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("enqueue blocked: %v", elapsed)
	}
	if service.Status().Dropped != 300 {
		t.Fatalf("status=%+v", service.Status())
	}
	close(repo.release)
	service.Close()
}
func TestFailedLogWriteIsVisibleAndDoesNotLeakSecrets(t *testing.T) {
	repo := &testRepo{err: errors.New("storage unavailable https://user:secret@db.test/data?token=secret")}
	service := NewService(repo)
	service.Enqueue(logdomain.Entry{Message: "failure"})
	service.Close()
	status := service.Status()
	if status.Dropped != 1 || status.WriteFailures != 1 || status.LastFailureAt == nil || status.LastError != "storage unavailable https://db.test/data" {
		t.Fatalf("status=%+v", status)
	}
}
