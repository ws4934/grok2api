package chatgpt

import (
	"context"
	"errors"
	"image"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestAsyncImageStepsCheckpointAroundTheSend(t *testing.T) {
	ctx := context.Background()
	var submits atomic.Int32
	a, assets := newTestAdapter(t, imageUpstream(t, "", 0, &submits))
	request := provider.ImageJobRequest{Credential: testCredential(t, a, credentials{AccessToken: "AT"}), Model: imageModel, Prompt: "a fox", OutputFormat: "jpeg"}
	var saved []provider.ImageCheckpoint
	save := func(cp provider.ImageCheckpoint) error {
		if len(saved) == 0 && submits.Load() != 0 {
			t.Error("the submission marker was saved after the send")
		}
		saved = append(saved, cp)
		return nil
	}
	cp, err := a.SubmitImage(ctx, request, save)
	want := []provider.ImageCheckpoint{{Submitted: true}, {Submitted: true, UpstreamID: "conv1"}}
	if err != nil || cp.UpstreamID != "conv1" || !cp.Submitted || len(saved) != 2 || !slices.EqualFunc(saved, want, func(a, b provider.ImageCheckpoint) bool { return a.Submitted == b.Submitted && a.UpstreamID == b.UpstreamID }) {
		t.Fatalf("submit = %+v %v, saved %+v", cp, err, saved)
	}

	// Recovery resumes from the checkpoint alone and never posts again.
	cp, err = a.PollImage(ctx, request, provider.ImageCheckpoint{Submitted: true, UpstreamID: "conv1"})
	if err != nil || len(cp.Outputs) == 0 {
		t.Fatalf("poll = %+v %v", cp, err)
	}
	data, err := a.FetchImage(ctx, request, cp)
	if err != nil || http.DetectContentType(data) != "image/jpeg" || submits.Load() != 1 || assets.saved != 0 {
		t.Fatalf("fetch: %v type=%s submits=%d saved=%d", err, http.DetectContentType(data), submits.Load(), assets.saved)
	}
	usage := a.ImageUsage("a fox", 1, "high", []image.Point{image.Pt(64, 48)})
	if usage["output_tokens"] != outputImageTokens(64, 48, "", "high") || usage["input_tokens_details"].(map[string]any)["image_tokens"] != int64(256) {
		t.Fatalf("usage = %v", usage)
	}

	// A failed marker write aborts before anything is sent.
	if _, err := a.SubmitImage(ctx, request, func(provider.ImageCheckpoint) error { return errors.New("lost claim") }); err == nil || submits.Load() != 1 {
		t.Fatalf("failed save = %v submits=%d", err, submits.Load())
	}

	// A refusal before generation is unsubmitted, so another account may retry.
	b, _ := newTestAdapter(t, imageUpstream(t, "/backend-api/f/conversation", http.StatusTooManyRequests, &submits))
	request.Credential = testCredential(t, b, credentials{AccessToken: "AT"})
	if cp, err := b.SubmitImage(ctx, request, func(provider.ImageCheckpoint) error { return nil }); err == nil || cp.Submitted || provider.ErrorHTTPStatusOrZero(err) != http.StatusTooManyRequests {
		t.Fatalf("refused submit = %+v %v", cp, err)
	}

	// A send with an unknown outcome stays submitted without an upstream ID.
	c, _ := newTestAdapter(t, imageUpstream(t, "/backend-api/f/conversation", http.StatusBadGateway, &submits))
	request.Credential = testCredential(t, c, credentials{AccessToken: "AT"})
	if cp, err := c.SubmitImage(ctx, request, func(provider.ImageCheckpoint) error { return nil }); err == nil || !cp.Submitted || cp.UpstreamID != "" {
		t.Fatalf("unknown submit = %+v %v", cp, err)
	}
}
