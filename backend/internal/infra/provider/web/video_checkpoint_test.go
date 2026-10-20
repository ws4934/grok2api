package web

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestWebVideoCheckpointNeverGeneratesASubmittedVideoAgain(t *testing.T) {
	var creates atomic.Int32
	rejectCreate := atomic.Bool{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/rest/app-chat/conversations/new" {
			http.NotFound(writer, request)
			return
		}
		creates.Add(1)
		if rejectCreate.Load() {
			writer.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(writer, `{"error":"rate limited"}`)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, `data: {"result":{"response":{"streamingVideoGenerationResponse":{"progress":100,"videoPostId":"post_1","videoUrl":"/videos/final.mp4"}}}}`+"\n\n")
	}))
	defer server.Close()
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encryptedToken, err := cipher.Encrypt("test-sso")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{BaseURL: server.URL, StatsigMode: "manual", StatsigManualValue: "test", VideoTimeoutSeconds: 5},
		infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, nil)
	var saved []string
	request := provider.VideoRequest{
		Credential: account.Credential{ID: 1, Provider: account.ProviderWeb, EncryptedAccessToken: encryptedToken},
		Prompt:     "test", Duration: 5, Checkpoint: "{}",
		SaveCheckpoint: func(value string) error { saved = append(saved, value); return nil },
	}
	const videoURL = "https://assets.grok.com/videos/final.mp4"

	// The marker is saved before the irreversible send, the URL once the stream delivers it.
	if result, err := adapter.GenerateVideo(context.Background(), request); err != nil || result.URL != videoURL {
		t.Fatalf("result = %#v err = %v", result, err)
	}
	if !slices.Equal(saved, []string{`{"submitting":true}`, `{"url":"` + videoURL + `"}`}) || creates.Load() != 1 {
		t.Fatalf("saved = %v creates = %d", saved, creates.Load())
	}

	// A run lost while downloading resumes from the delivered URL.
	request.Checkpoint = saved[1]
	if result, err := adapter.GenerateVideo(context.Background(), request); err != nil || result.URL != videoURL || result.ContentType != "video/mp4" || creates.Load() != 1 {
		t.Fatalf("resumed result = %#v err = %v creates = %d", result, err, creates.Load())
	}

	// A run lost mid-stream can be neither reattached nor safely resent.
	request.Checkpoint = saved[0]
	if _, err := adapter.GenerateVideo(context.Background(), request); creates.Load() != 1 {
		t.Fatalf("unknown submission err = %v creates = %d", err, creates.Load())
	} else if stage, _ := provider.VideoErrorStage(err); stage != provider.VideoStageSubmitted {
		t.Fatalf("unknown submission stage = %q err = %v", stage, err)
	}
	if len(saved) != 2 {
		t.Fatalf("recovery saved = %v", saved)
	}

	// An explicit refusal clears the marker so another attempt may create the video.
	rejectCreate.Store(true)
	request.Checkpoint, saved = "", nil
	if _, err := adapter.GenerateVideo(context.Background(), request); err == nil {
		t.Fatal("rejected create succeeded")
	} else if stage, _ := provider.VideoErrorStage(err); stage != provider.VideoStageCreate {
		t.Fatalf("rejected create stage = %q", stage)
	}
	if !slices.Equal(saved, []string{`{"submitting":true}`, ""}) || creates.Load() != 2 {
		t.Fatalf("saved after refusal = %v creates = %d", saved, creates.Load())
	}

	// A free account gets the capped duration, and the result says so, also on resume.
	rejectCreate.Store(false)
	request.Checkpoint, saved = "", nil
	request.Credential.WebTier, request.Duration = account.WebTierBasic, 10
	if result, err := adapter.GenerateVideo(context.Background(), request); err != nil || result.Seconds != 6 {
		t.Fatalf("free result = %#v err = %v", result, err)
	}
	request.Checkpoint = saved[len(saved)-1]
	if result, err := adapter.GenerateVideo(context.Background(), request); err != nil || result.Seconds != 6 {
		t.Fatalf("resumed free result = %#v err = %v from %s", result, err, request.Checkpoint)
	}
}
