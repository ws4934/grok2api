package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestConsoleVideoCheckpointPollsInsteadOfCreatingAgain(t *testing.T) {
	var creates atomic.Int32
	rejectCreate := atomic.Bool{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if serveTestDPoPToken(t, writer, request) {
			return
		}
		verifyTestDPoPProof(t, request)
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1/videos/generations":
			creates.Add(1)
			if rejectCreate.Load() {
				writer.WriteHeader(http.StatusTooManyRequests)
				_, _ = writer.Write([]byte(`{"error":"rate limited"}`))
				return
			}
			_, _ = writer.Write([]byte(`{"request_id":"upstream-video-1"}`))
		case request.Method == http.MethodGet && request.URL.Path == "/v1/videos/upstream-video-1":
			_, _ = writer.Write([]byte(`{"status":"done","progress":100,"video":{"url":"https://vidgen.x.ai/result.mp4"}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	adapter, credential := newConsoleTestAdapter(t, server.URL)
	var saved []string
	request := provider.VideoRequest{
		Credential: credential, Model: "grok-imagine-video", Prompt: "animate", Duration: 6, Resolution: "720p",
		SaveCheckpoint: func(value string) error { saved = append(saved, value); return nil },
	}
	ctx := context.Background()

	// The marker is saved before the irreversible create, the upstream ID after it.
	if _, err := adapter.GenerateVideo(ctx, request); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(saved, []string{`{"submitting":true}`, `{"request_id":"upstream-video-1"}`}) || creates.Load() != 1 {
		t.Fatalf("saved = %v creates = %d", saved, creates.Load())
	}

	// Recovery polls the saved upstream job.
	request.Checkpoint = saved[1]
	if result, err := adapter.GenerateVideo(ctx, request); err != nil || result.URL != "https://vidgen.x.ai/result.mp4" || creates.Load() != 1 {
		t.Fatalf("resumed result = %#v err = %v creates = %d", result, err, creates.Load())
	}

	// A marker without an ID can be neither resumed nor safely resent.
	request.Checkpoint = saved[0]
	if _, err := adapter.GenerateVideo(ctx, request); err == nil || creates.Load() != 1 {
		t.Fatalf("unknown submission err = %v creates = %d", err, creates.Load())
	} else if stage, _ := provider.VideoErrorStage(err); stage != provider.VideoStageSubmitted {
		t.Fatalf("unknown submission stage = %q", stage)
	}

	// An explicit refusal clears the marker so another account may retry.
	rejectCreate.Store(true)
	request.Checkpoint, saved = "", nil
	if _, err := adapter.GenerateVideo(ctx, request); err == nil {
		t.Fatal("rejected create succeeded")
	} else if stage, _ := provider.VideoErrorStage(err); stage != provider.VideoStageCreate {
		t.Fatalf("rejected create stage = %q", stage)
	}
	if !slices.Equal(saved, []string{`{"submitting":true}`, ""}) {
		t.Fatalf("saved after refusal = %v", saved)
	}
}
