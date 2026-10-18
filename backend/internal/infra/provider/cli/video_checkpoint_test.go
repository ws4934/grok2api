package cli

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestBuildVideoCheckpointPollsInsteadOfCreatingAgain(t *testing.T) {
	adapter, encrypted := newTestBuildVideoAdapter(t)
	var creates, polls atomic.Int32
	rejectCreate := atomic.Bool{}
	ctx, crash := context.WithCancel(context.Background())
	defer crash()
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1/videos/generations":
			creates.Add(1)
			if rejectCreate.Load() {
				return jsonResponse(http.StatusTooManyRequests, `{"error":"rate limited"}`, request), nil
			}
			return jsonResponse(http.StatusOK, `{"request_id":"vid_1"}`, request), nil
		case request.Method == http.MethodGet && request.URL.Path == "/v1/videos/vid_1":
			if polls.Add(1) == 1 {
				crash() // the worker dies while the upstream job is generating
				return jsonResponse(http.StatusOK, `{"status":"processing","progress":40}`, request), nil
			}
			return jsonResponse(http.StatusOK, `{"status":"completed","video":{"url":"https://assets.grok.com/videos/vid_1.mp4"}}`, request), nil
		}
		t.Errorf("unexpected request %s %s", request.Method, request.URL)
		return jsonResponse(http.StatusNotFound, `{}`, request), nil
	})
	var saved []string
	request := provider.VideoRequest{
		Credential: account.Credential{ID: 9, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted},
		Prompt:     "animate", Duration: 6, Checkpoint: "{}",
		SaveCheckpoint: func(value string) error { saved = append(saved, value); return nil },
	}

	// The marker is saved before the irreversible create, the upstream ID after it.
	if _, err := adapter.GenerateVideo(ctx, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("crashed run err = %v", err)
	}
	if !slices.Equal(saved, []string{`{"submitting":true}`, `{"request_id":"vid_1"}`}) || creates.Load() != 1 {
		t.Fatalf("saved = %v creates = %d", saved, creates.Load())
	}

	// Recovery polls the saved upstream job.
	request.Checkpoint = saved[1]
	if result, err := adapter.GenerateVideo(context.Background(), request); err != nil || result.URL != "https://assets.grok.com/videos/vid_1.mp4" || creates.Load() != 1 {
		t.Fatalf("resumed result = %#v err = %v creates = %d", result, err, creates.Load())
	}

	// A marker without an ID can be neither resumed nor safely resent.
	request.Checkpoint = saved[0]
	if _, err := adapter.GenerateVideo(context.Background(), request); creates.Load() != 1 {
		t.Fatalf("unknown submission err = %v creates = %d", err, creates.Load())
	} else if stage, _ := provider.VideoErrorStage(err); stage != provider.VideoStageSubmitted {
		t.Fatalf("unknown submission stage = %q err = %v", stage, err)
	}
	if len(saved) != 2 {
		t.Fatalf("recovery saved = %v", saved)
	}

	// An explicit refusal clears the marker so another account may retry.
	rejectCreate.Store(true)
	request.Checkpoint, saved = "", nil
	if _, err := adapter.GenerateVideo(context.Background(), request); err == nil {
		t.Fatal("rejected create succeeded")
	} else if stage, _ := provider.VideoErrorStage(err); stage != provider.VideoStageCreate {
		t.Fatalf("rejected create stage = %q", stage)
	}
	if !slices.Equal(saved, []string{`{"submitting":true}`, ""}) {
		t.Fatalf("saved after refusal = %v", saved)
	}
}

// countingUploadIssuer records new upload tickets; recovery must reuse the
// asset of the original ticket instead of issuing another.
type countingUploadIssuer struct {
	uploadIssuerStub
	issued atomic.Int32
}

func (u *countingUploadIssuer) IssueVideoUpload(ctx context.Context, jobID string) (string, string, error) {
	u.issued.Add(1)
	return u.uploadIssuerStub.IssueVideoUpload(ctx, jobID)
}

func TestBuildXAIVideoCheckpointResumesOnFallbackWithUploadAsset(t *testing.T) {
	adapter, encrypted := newFallbackTestAdapter(t)
	issuer := &countingUploadIssuer{uploadIssuerStub: uploadIssuerStub{url: "https://public.example/v1/media/uploads/eeff", assetID: "vid_xai"}}
	adapter.SetVideoUploadIssuer(issuer)
	var primaryCreates, xaiCreates, xaiPolls atomic.Int32
	ctx, crash := context.WithCancel(context.Background())
	defer crash()
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		primary := strings.Contains(request.URL.Host, "primary.test")
		switch {
		case request.Method == http.MethodPost && primary:
			primaryCreates.Add(1)
			return jsonResponse(http.StatusForbidden, `{"error":"forbidden"}`, request), nil
		case request.Method == http.MethodPost:
			xaiCreates.Add(1)
			return jsonResponse(http.StatusOK, `{"request_id":"job_xai"}`, request), nil
		case request.Method == http.MethodGet && !primary && strings.HasSuffix(request.URL.Path, "/videos/job_xai"):
			if request.Header.Get("User-Agent") != "xai-grok-build/0.2.99" {
				t.Errorf("XAI poll user agent = %q", request.Header.Get("User-Agent"))
			}
			if xaiPolls.Add(1) == 1 {
				crash()
				return jsonResponse(http.StatusOK, `{"status":"processing"}`, request), nil
			}
			return jsonResponse(http.StatusOK, `{"status":"done"}`, request), nil
		}
		t.Errorf("unexpected request %s %s", request.Method, request.URL)
		return jsonResponse(http.StatusNotFound, `{}`, request), nil
	})
	var saved []string
	request := provider.VideoRequest{
		Credential: account.Credential{ID: 15, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted, BuildSuperEntitled: true},
		JobID:      "video_xai", Prompt: "waves", Duration: 6, Resolution: "720p",
		SaveCheckpoint: func(value string) error { saved = append(saved, value); return nil },
	}

	// The primary 403 created nothing, so its marker is cleared before XAI.
	if _, err := adapter.GenerateVideo(ctx, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("crashed run err = %v", err)
	}
	want := []string{`{"submitting":true}`, "", `{"submitting":true}`, `{"request_id":"job_xai","xai":true,"asset_id":"vid_xai"}`}
	if !slices.Equal(saved, want) {
		t.Fatalf("saved = %v", saved)
	}

	// Recovery polls XAI and waits for the original upload asset.
	request.Checkpoint = saved[3]
	result, err := adapter.GenerateVideo(context.Background(), request)
	if err != nil || result.AssetID != "vid_xai" {
		t.Fatalf("resumed result = %#v err = %v", result, err)
	}
	if primaryCreates.Load() != 1 || xaiCreates.Load() != 1 || issuer.issued.Load() != 1 || issuer.waitCalls.Load() != 1 || len(saved) != 4 {
		t.Fatalf("primary creates = %d xai creates = %d issued = %d waits = %d saved = %v",
			primaryCreates.Load(), xaiCreates.Load(), issuer.issued.Load(), issuer.waitCalls.Load(), saved)
	}
}
