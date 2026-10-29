package web

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func sequentialVideoTestClock() func() time.Time {
	var calls atomic.Int64
	return func() time.Time { return time.Unix(0, calls.Add(1)*int64(11*time.Second)) }
}
func TestWebVideoOnlyAcceptsCompletedGeneration(t *testing.T) {
	for _, test := range []struct {
		name, stream, want string
		rejected           bool
	}{
		{"ignores history attachment", `{"result":{"response":{"modelResponse":{"fileAttachments":["/videos/old.mp4"]}}}}` + "\n" + `{"result":{"response":{"streamingVideoGenerationResponse":{"progress":100,"videoUrl":"/videos/new.mp4"}}}}`, "/videos/new.mp4", false},
		{"ignores intermediate URL", `{"result":{"response":{"streamingVideoGenerationResponse":{"progress":5,"videoUrl":"/videos/preview.mp4"}}}}` + "\n" + `{"result":{"response":{"streamingVideoGenerationResponse":{"progress":100,"videoUrl":"/videos/new.mp4"}}}}`, "/videos/new.mp4", false},
		{"attachment alone is not generation", `{"result":{"response":{"modelResponse":{"fileAttachments":["/videos/old.mp4"]}}}}`, "", true},
		{"unfinished stream with URL", `{"result":{"response":{"streamingVideoGenerationResponse":{"progress":5,"videoUrl":"/videos/preview.mp4"}}}}`, "", true},
		{"mismatched attachment id", `{"result":{"response":{"streamingVideoGenerationResponse":{"progress":100,"videoId":"new"},"modelResponse":{"fileAttachments":["/generated/old/generated_video.mp4"]}}}}`, "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, _, err := parseVideoStream(&http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(test.stream))}, nil)
			var rejected *webVideoResultError
			if errors.As(err, &rejected) != test.rejected || test.want != "" && !strings.HasSuffix(result.URL, test.want) {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}
func TestRejectedWebResultRequiresDurableCheckpointClear(t *testing.T) {
	for _, fail := range []bool{false, true} {
		saved := "not-cleared"
		request := provider.VideoRequest{SaveCheckpoint: func(value string) error {
			if fail {
				return errors.New("storage unavailable")
			}
			saved = value
			return nil
		}}
		err := rejectWebVideoResult(request, &webVideoResultError{reason: "too_fast", elapsed: 5 * time.Second})
		stage, _ := provider.VideoErrorStage(err)
		if fail {
			if stage != provider.VideoStageSubmitted || saved != "not-cleared" {
				t.Fatalf("unsafe retry: %v", err)
			}
		} else if stage != provider.VideoStageResultRejected || saved != "" {
			t.Fatalf("rejection=%v saved=%q", err, saved)
		}
	}
}

func TestFreshWebVideoRejectsFastResultsButKeepsDownloadRecovery(t *testing.T) {
	var creates atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		creates.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"result":{"response":{"streamingVideoGenerationResponse":{"progress":100,"videoPostId":"post","videoUrl":"/videos/output.mp4"}}}}`+"\n\n")
	}))
	defer server.Close()
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	token, err := cipher.Encrypt("test-sso")
	if err != nil {
		t.Fatal(err)
	}
	for _, elapsed := range []time.Duration{0, 5800 * time.Millisecond, 10 * time.Second, 10*time.Second + time.Nanosecond, 40 * time.Second} {
		t.Run(elapsed.String(), func(t *testing.T) {
			a := NewAdapter(Config{BaseURL: server.URL, StatsigMode: "manual", StatsigManualValue: "test", VideoTimeoutSeconds: 5}, infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, nil)
			calls := 0
			a.videoNow = func() time.Time {
				calls++
				if calls == 1 {
					return time.Unix(0, 0)
				}
				return time.Unix(0, int64(elapsed))
			}
			saved := []string{}
			request := provider.VideoRequest{Credential: account.Credential{ID: 1, Provider: account.ProviderWeb, EncryptedAccessToken: token}, Duration: 6, Prompt: "test", SaveCheckpoint: func(value string) error { saved = append(saved, value); return nil }}
			result, err := a.GenerateVideo(context.Background(), request)
			if elapsed <= minimumFreshWebVideoTime {
				stage, _ := provider.VideoErrorStage(err)
				if stage != provider.VideoStageResultRejected || result.URL != "" || len(saved) != 2 || saved[1] != "" {
					t.Fatalf("result=%+v err=%v saved=%v", result, err, saved)
				}
				return
			}
			if err != nil || result.URL == "" || len(saved) != 2 {
				t.Fatalf("result=%+v err=%v saved=%v", result, err, saved)
			}
			before := creates.Load()
			request.Checkpoint = saved[1]
			a.videoNow = func() time.Time { t.Fatal("recovery must not run fresh-generation clock"); return time.Time{} }
			if resumed, err := a.GenerateVideo(context.Background(), request); err != nil || resumed.URL != result.URL || creates.Load() != before {
				t.Fatalf("recovery=%+v err=%v", resumed, err)
			}
		})
	}
}
func TestModeratedVideoNeverFallsBackToAnAttachment(t *testing.T) {
	stream := `{"result":{"response":{"streamingVideoGenerationResponse":{"progress":100,"moderated":true,"videoUrl":"/videos/blocked.mp4"},"modelResponse":{"fileAttachments":["/videos/old.mp4"]}}}}`
	result, _, err := parseVideoStream(&http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(stream))}, nil)
	if result.URL != "" || !provider.IsRequestScopedError(err) || !provider.IsContentSafetyRejection(err.Error()) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestIncompleteLongRunningWebVideoIsNeverReplayed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"result":{"response":{"streamingVideoGenerationResponse":{"progress":5,"videoUrl":"/videos/preview.mp4"}}}}`)
	}))
	defer server.Close()
	a, credential := testMediaAdapter(t, server.URL)
	calls := 0
	a.videoNow = func() time.Time {
		calls++
		return time.Unix(int64(calls)*40, 0)
	}
	var saved []string
	_, err := a.GenerateVideo(context.Background(), provider.VideoRequest{Credential: credential, Duration: 6, Prompt: "test", SaveCheckpoint: func(value string) error {
		saved = append(saved, value)
		return nil
	}})
	stage, _ := provider.VideoErrorStage(err)
	if stage != provider.VideoStagePoll || len(saved) != 1 || saved[0] != `{"submitting":true}` {
		t.Fatalf("incomplete video became replayable: err=%v saved=%v", err, saved)
	}
}
