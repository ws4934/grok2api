package updatecheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The daily background check joins an admin's in-flight check; the admin
// leaving must not fail it, or the snapshot would say check_failed for a day.
func TestCheckOutlivesTheCallerThatStartedIt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
		_, _ = w.Write([]byte(`{"tag_name":"v9.9.9","html_url":"https://github.com/ws4934/grok2api/releases/tag/v9.9.9"}`))
	}))
	defer server.Close()
	service := NewService("v1.0.0", "", server.Client())
	service.latestURL = server.URL

	adminCtx, adminLeaves := context.WithCancel(context.Background())
	go func() { _ = service.Check(adminCtx) }()
	time.Sleep(50 * time.Millisecond)
	background := make(chan Snapshot, 1)
	go func() { background <- service.Check(context.Background()) }()
	time.Sleep(50 * time.Millisecond)
	adminLeaves()
	snapshot := <-background
	if snapshot.Status != StatusUpdateAvailable {
		t.Fatalf("background check with a live context got %s (%s)", snapshot.Status, snapshot.Error)
	}
}
