package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestVideoPinsPersistExecuteAndBillAllTwentyInputs(t *testing.T) {
	f, adapter := newNativeVideoFixture(t, 100)
	muted := false
	input := VideoInput{RequestID: "twenty-pins", ClientKey: f.key, PublicModel: "Console/grok-imagine-video-1.5", Duration: 15, Resolution: "1080p",
		ImageURL: "https://example.com/first.png", LastFrameURL: "https://example.com/last.png", GenerateAudio: &muted, IdempotencyKey: "twenty"}
	for i := range 14 {
		input.ReferenceURLs = append(input.ReferenceURLs, fmt.Sprintf("https://example.com/ref%d.png", i))
	}
	for i := range 4 {
		input.Keyframes = append(input.Keyframes, media.VideoKeyframe{ImageURL: fmt.Sprintf("https://example.com/key%d.png", i), TimestampSeconds: float64(i + 1)})
	}
	job, err := f.service.CreateVideo(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if job.InputImageCount != 20 || job.Quality != "720p" || job.Seconds != 15 {
		t.Fatalf("effective job=%+v", job)
	}
	decoded := decodeVideoInputs(job.InputJSON)
	if len(decoded.imageRefs()) != 20 || decoded.generateAudio == nil || *decoded.generateAudio || len(decoded.keyframes) != 4 {
		t.Fatalf("persisted inputs=%s", job.InputJSON)
	}
	parameters, _ := json.Marshal(job.RequestParameters)
	if !strings.Contains(string(parameters), `"generate_audio":false`) || !strings.Contains(string(parameters), `"720p"`) {
		t.Fatalf("parameter snapshot=%s", parameters)
	}
	duplicate, err := f.service.CreateVideo(context.Background(), input)
	if err != nil || duplicate.ID != job.ID {
		t.Fatalf("same payload was not idempotent: %s %v", duplicate.ID, err)
	}
	audible := true
	input.GenerateAudio = &audible
	if _, err := f.service.CreateVideo(context.Background(), input); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("audio change did not change the payload identity: %v", err)
	}
	adapter.before = func(r provider.VideoRequest) error {
		if r.LastFrameURL != decoded.lastFrameURL || len(r.Keyframes) != 4 || r.Keyframes[3].TimestampSeconds != 4 || r.GenerateAudio == nil || *r.GenerateAudio || r.Resolution != "720p" {
			t.Errorf("worker lost pinned inputs or silence: %+v", r)
		}
		return nil
	}
	f.service.processMediaJob(context.Background(), job.ID)
	stored, err := f.jobs.GetMediaJob(context.Background(), job.ID, f.key.ID)
	if err != nil || stored.Status != media.StatusCompleted {
		t.Fatalf("completed=%+v err=%v", stored, err)
	}
	f.checkBilling(t, 230) // 15s × 14 cents + 20 input images × 1 cent; not 1080p.
}

func TestVideoPinsRejectBeforeEnqueueAndAllowStandaloneInputs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		last      string
		frames    []media.VideoKeyframe
		wantError bool
	}{
		{"last-only", "https://example.com/last.png", nil, false},
		{"key-only", "", []media.VideoKeyframe{{ImageURL: "https://example.com/key.png", TimestampSeconds: 2}}, false},
		{"at-end", "", []media.VideoKeyframe{{ImageURL: "https://example.com/key.png", TimestampSeconds: 6}}, true},
		{"same-slot", "", []media.VideoKeyframe{{ImageURL: "https://example.com/a.png", TimestampSeconds: 2}, {ImageURL: "https://example.com/b.png", TimestampSeconds: 2.1}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, adapter := newNativeVideoFixture(t, 100)
			job, err := f.service.CreateVideo(context.Background(), VideoInput{RequestID: tc.name, ClientKey: f.key, PublicModel: "Console/grok-imagine-video-1.5", Duration: 6, LastFrameURL: tc.last, Keyframes: tc.frames})
			if tc.wantError {
				if !errors.Is(err, provider.ErrVideoParameterInvalid) || job.ID != "" || len(adapter.calls) != 0 {
					t.Fatalf("job=%+v err=%v", job, err)
				}
			} else if err != nil || job.ID == "" {
				t.Fatalf("standalone input rejected: %v", err)
			}
		})
	}
}

func TestVideoRetryRetainsPinnedFramesAndExplicitSilence(t *testing.T) {
	f, _ := newNativeVideoFixture(t, 100)
	muted := false
	job, err := f.service.CreateVideo(context.Background(), VideoInput{RequestID: "pin-retry", ClientKey: f.key, PublicModel: "Console/grok-imagine-video-1.5", Duration: 6,
		LastFrameURL: "https://example.com/last.png", Keyframes: []media.VideoKeyframe{{ImageURL: "https://example.com/key.png", TimestampSeconds: 2}}, GenerateAudio: &muted})
	if err != nil {
		t.Fatal(err)
	}
	job.Status = media.StatusUncertain
	if err := f.jobs.UpdateMediaJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	retry, err := f.service.RetryMediaJob(context.Background(), job.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	inputs := decodeVideoInputs(retry.InputJSON)
	if retry.ID == job.ID || inputs.lastFrameURL == "" || len(inputs.keyframes) != 1 || inputs.keyframes[0].TimestampSeconds != 2 || inputs.generateAudio == nil || *inputs.generateAudio {
		t.Fatalf("retry lost inputs: %+v", retry)
	}
}

func TestVideoPinsResumeFromCheckpointWithSilence(t *testing.T) {
	f, adapter := newNativeVideoFixture(t, 100)
	muted := false
	ctx := context.Background()
	job, err := f.service.CreateVideo(ctx, VideoInput{RequestID: "pin-recovery", ClientKey: f.key, PublicModel: "Console/grok-imagine-video-1.5", Duration: 6,
		LastFrameURL: "https://example.com/last.png", Keyframes: []media.VideoKeyframe{{ImageURL: "https://example.com/key.png", TimestampSeconds: 2}}, GenerateAudio: &muted})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	claimed, ok, err := f.jobs.TryClaimMediaJob(ctx, job.ID, now, now.Add(-time.Second), "expired-pin-worker")
	if err != nil || !ok {
		t.Fatalf("claim=%v err=%v", ok, err)
	}
	claimed.Submitted, claimed.Checkpoint = true, `{"request_id":"accepted-pinned-video"}`
	if err := f.jobs.UpdateMediaJob(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	adapter.before = func(r provider.VideoRequest) error {
		if r.Credential.ID != claimed.AccountID || r.Checkpoint != claimed.Checkpoint || r.LastFrameURL == "" || len(r.Keyframes) != 1 || r.GenerateAudio == nil || *r.GenerateAudio {
			t.Errorf("recovery lost inputs, account or checkpoint: %+v", r)
		}
		return nil
	}
	f.service.processMediaJob(ctx, job.ID)
	stored, err := f.jobs.GetMediaJob(ctx, job.ID, f.key.ID)
	if err != nil || stored.Status != media.StatusCompleted || len(adapter.calls) != 1 {
		t.Fatalf("recovered=%+v calls=%v err=%v", stored, adapter.calls, err)
	}
	f.checkBilling(t, 86)
}

func TestPinnedInputsStageWithoutMutatingCallerAndReleaseOnAbort(t *testing.T) {
	store := &videoAssetStoreStub{}
	stager := inputStager{service: &Service{mediaAssets: store}}
	png := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\npixels"))
	original := []media.VideoKeyframe{{ImageURL: png, TimestampSeconds: 2}}
	in, err := stager.stageVideoInputs(context.Background(), videoInputs{lastFrameURL: png, keyframes: original})
	if err != nil || !provider.IsInputReference(in.lastFrameURL) || !provider.IsInputReference(in.keyframes[0].ImageURL) || original[0].ImageURL != png {
		t.Fatalf("inputs=%+v err=%v", in, err)
	}
	encoded, err := encodeVideoInputs(in)
	if err != nil {
		t.Fatal(err)
	}
	if refs := decodeVideoInput(encoded); len(refs) != 2 {
		t.Fatalf("cleanup omitted a pin: %v", refs)
	}
	stager.abort()
	if len(store.released) != 1 || len(store.released[0]) != 2 {
		t.Fatalf("cleanup=%v", store.released)
	}
}
