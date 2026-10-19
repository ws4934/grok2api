package cli

import (
	"io"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"
	"time"
)

// quietUpstream is an upstream stream in a silent phase: Read blocks until Close.
type quietUpstream struct {
	closed chan struct{}
	once   sync.Once
}

func (u *quietUpstream) Read([]byte) (int, error) { <-u.closed; return 0, io.ErrClosedPipe }
func (u *quietUpstream) Close() error             { u.once.Do(func() { close(u.closed) }); return nil }

// waitGoroutineExit fails unless every goroutine that creator started from
// the calling goroutine exits.
func waitGoroutineExit(t *testing.T, creator string) {
	t.Helper()
	var self [64]byte
	marker := creator + " in goroutine " + strings.Fields(string(self[:runtime.Stack(self[:], false)]))[1] + "\n"
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		var dump strings.Builder
		_ = pprof.Lookup("goroutine").WriteTo(&dump, 2)
		if !strings.Contains(dump.String(), marker) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine started by %s still running after Close", creator)
		}
	}
}

func TestResponsesStreamCloseClosesUpstream(t *testing.T) {
	for name, wrap := range map[string]func(io.ReadCloser) io.ReadCloser{
		"normalizeResponseStream":              (*responsesToolCompatibility)(nil).normalizeResponseStream,
		"(*buildXSearchResponseFilter).stream": (&buildXSearchResponseFilter{}).stream,
	} {
		t.Run(name, func(t *testing.T) {
			upstream := &quietUpstream{closed: make(chan struct{})}
			t.Cleanup(func() { _ = upstream.Close() })
			body := wrap(upstream)
			_ = body.Close()
			select {
			case <-upstream.closed:
			default:
				t.Error("Close did not close the upstream body")
			}
			waitGoroutineExit(t, name)
		})
	}
}
