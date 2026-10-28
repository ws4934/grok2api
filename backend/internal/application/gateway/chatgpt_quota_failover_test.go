package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type quotaSwitchImageAdapter struct {
	*asyncImageAdapter
	rejected uint64
	all      bool
	accepted bool
	calls    []uint64
}

func (a *quotaSwitchImageAdapter) GenerateImage(_ context.Context, r provider.ImageGenerationRequest) (*provider.Response, error) {
	a.calls = append(a.calls, r.Credential.ID)
	if a.all || r.Credential.ID == a.rejected {
		if a.accepted {
			return nil, acceptedQuotaError{statusError{429}}
		}
		return &provider.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"120"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"rate_limit_exceeded","message":"Usage limit reached"}}`))}, nil
	}
	return &provider.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"created":1,"data":[{"url":"https://example.test/image.png"}]}`)), QuotaUnits: 1}, nil
}

type acceptedQuotaError struct{ statusError }

func (acceptedQuotaError) RetryAfterDuration() time.Duration { return 120 * time.Second }

func TestChatGPTAcceptedImageQuotaFailureDoesNotReplayOrPenalizeAccount(t *testing.T) {
	f := newImageJobFixture(t, 2)
	setChatGPTQuota(t, f, "image_gen", 5)
	adapter := &quotaSwitchImageAdapter{asyncImageAdapter: f.adapter, rejected: f.accounts[0].ID, accepted: true}
	f.service.providers = provider.NewRegistry(adapter)
	input := ImageGenerationInput{RequestID: "accepted-quota", ClientKey: f.key, PublicModel: "gpt-image-2", Prompt: "fox", Count: 1}
	result, err := f.service.GenerateImage(context.Background(), input)
	if result != nil || provider.ErrorHTTPStatusOrZero(errors.Unwrap(err)) != 429 || len(adapter.calls) != 1 {
		t.Fatalf("accepted generation was replayed: result=%+v err=%v calls=%v", result, err, adapter.calls)
	}
	view, err := f.service.accounts.Get(context.Background(), f.accounts[0].ID)
	if err != nil || view.Credential.FailureCount != 0 || view.Credential.CooldownUntil == nil || !view.Credential.CooldownUntil.After(time.Now()) {
		t.Fatalf("typed rate limit did not apply soft cooldown: failures=%d err=%v", view.Credential.FailureCount, err)
	}
	input.RequestID = "after-accepted-quota"
	result, err = f.service.GenerateImage(context.Background(), input)
	if err != nil || result.StatusCode != 200 || len(adapter.calls) != 2 || adapter.calls[1] != f.accounts[1].ID {
		t.Fatalf("next request did not switch accounts: result=%+v err=%v calls=%v", result, err, adapter.calls)
	}
	result.Finalize(Usage{OutputImages: 1}, "", "")
	result.Body.Close()
}

func setChatGPTQuota(t *testing.T, f *imageJobFixture, mode string, remaining int) {
	t.Helper()
	repo := f.service.selector.accounts.(*relational.AccountRepository)
	// Use the same synchronous local invalidation as application wiring.
	repo.SetInvalidationObserver(func(_ context.Context, event repository.InvalidationEvent) {
		f.service.selector.ApplyInvalidation(event)
	})
	now, reset := time.Now().UTC(), time.Now().UTC().Add(time.Hour)
	for _, credential := range f.accounts {
		if err := repo.SaveQuotaWindows(context.Background(), credential.ID, account.WebTier(""), now, []account.QuotaWindow{{AccountID: credential.ID, Mode: mode, Remaining: remaining, ResetAt: &reset, SyncedAt: &now, Source: account.QuotaSourceUpstream, UpdatedAt: now}}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestChatGPTImageQuotaSwitchesInSameRequestAndSkipsSpentAccountNextTime(t *testing.T) {
	f := newImageJobFixture(t, 2)
	setChatGPTQuota(t, f, "image_gen", 5)
	adapter := &quotaSwitchImageAdapter{asyncImageAdapter: f.adapter, rejected: f.accounts[0].ID}
	f.service.providers = provider.NewRegistry(adapter)
	for i := range 2 {
		result, err := f.service.GenerateImage(context.Background(), ImageGenerationInput{RequestID: fmt.Sprintf("quota-switch-%d", i), ClientKey: f.key, PublicModel: "gpt-image-2", Prompt: "fox", Count: 1})
		if err != nil || result.StatusCode != 200 {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		io.Copy(io.Discard, result.Body)
		result.Finalize(Usage{OutputImages: 1}, "", "")
		result.Body.Close()
	}
	if want := []uint64{f.accounts[0].ID, f.accounts[1].ID, f.accounts[1].ID}; !slices.Equal(adapter.calls, want) {
		t.Fatalf("calls=%v want=%v", adapter.calls, want)
	}
	view, err := f.service.accounts.Get(context.Background(), f.accounts[0].ID)
	if err != nil || view.Credential.FailureCount != 0 || view.Credential.AuthStatus != account.AuthStatusActive {
		t.Fatalf("quota exhaustion changed account health: %+v %v", view, err)
	}
	if billed, reserved := f.billing(t); billed != 2*400_000_000 || reserved != 0 {
		t.Fatalf("billing=%d reserved=%d", billed, reserved)
	}
}

func TestChatGPTImageQuotaAllAccountsSpentReturns429NotNoAvailableAccount(t *testing.T) {
	f := newImageJobFixture(t, 2)
	setChatGPTQuota(t, f, "image_gen", 5)
	adapter := &quotaSwitchImageAdapter{asyncImageAdapter: f.adapter, all: true}
	f.service.providers = provider.NewRegistry(adapter)
	result, err := f.service.GenerateImage(context.Background(), ImageGenerationInput{RequestID: "quota-all-spent", ClientKey: f.key, PublicModel: "gpt-image-2", Prompt: "fox", Count: 1})
	var failure *UpstreamFailure
	if result != nil || !errors.As(err, &failure) || failure.HTTPStatus != 429 || len(adapter.calls) != 2 {
		t.Fatalf("result=%+v failure=%+v err=%v calls=%v", result, failure, err, adapter.calls)
	}
	if billed, reserved := f.billing(t); billed != 0 || reserved != 0 {
		t.Fatalf("billing=%d reserved=%d", billed, reserved)
	}
}

func TestChatGPTImageTransientRateLimitDoesNotIncreaseFailureCount(t *testing.T) {
	f := newImageJobFixture(t, 2)
	adapter := &quotaSwitchImageAdapter{asyncImageAdapter: f.adapter, rejected: f.accounts[0].ID}
	f.service.providers = provider.NewRegistry(adapter)
	result, err := f.service.GenerateImage(context.Background(), ImageGenerationInput{RequestID: "quota-unknown", ClientKey: f.key, PublicModel: "gpt-image-2", Prompt: "fox", Count: 1})
	if err != nil {
		t.Fatal(err)
	}
	result.Finalize(Usage{}, "", "")
	result.Body.Close()
	view, err := f.service.accounts.Get(context.Background(), f.accounts[0].ID)
	if err != nil || view.Credential.FailureCount != 0 {
		t.Fatalf("transient limit penalized account: %+v %v", view, err)
	}
}

func TestChatGPTImageJobQuotaSwitchesBeforeSubmission(t *testing.T) {
	f := newImageJobFixture(t, 2)
	setChatGPTQuota(t, f, "image_gen", 5)
	f.adapter.submit = func(r provider.ImageJobRequest, save func(provider.ImageCheckpoint) error) (provider.ImageCheckpoint, error) {
		if r.Credential.ID == f.accounts[0].ID {
			return provider.ImageCheckpoint{}, statusError{429}
		}
		cp := provider.ImageCheckpoint{Submitted: true, UpstreamID: "accepted"}
		return cp, save(cp)
	}
	job := f.create(t, ImageJobInput{})
	f.service.processMediaJob(context.Background(), job.ID)
	stored := f.job(t, job.ID)
	if stored.Status != media.StatusCompleted || stored.AccountID != f.accounts[1].ID {
		t.Fatalf("job=%+v calls=%v", stored, f.adapter.Calls())
	}
	if calls := f.adapter.Calls(); len(calls) != 4 || calls[0] != fmt.Sprintf("submit:%d", f.accounts[0].ID) || calls[1] != fmt.Sprintf("submit:%d", f.accounts[1].ID) {
		t.Fatal(calls)
	}
}

func (a *quotaSwitchImageAdapter) ForwardResponse(_ context.Context, r provider.ResponseResourceRequest) (*provider.Response, error) {
	a.calls = append(a.calls, r.Credential.ID)
	if r.Credential.ID == a.rejected {
		return &provider.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"120"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"rate_limit_exceeded","message":"Usage limit reached"}}`))}, nil
	}
	return &provider.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"chatcmpl-quota","object":"chat.completion","model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))}, nil
}

func TestChatGPTChatQuotaSwitchesWithinRequest(t *testing.T) {
	f := newImageJobFixture(t, 2)
	setChatGPTQuota(t, f, "reason", 5)
	adapter := &quotaSwitchImageAdapter{asyncImageAdapter: f.adapter, rejected: f.accounts[0].ID}
	f.service.providers = provider.NewRegistry(adapter)
	result, err := f.service.CreateChatCompletion(context.Background(), Input{RequestID: "quota-chat", ClientKey: f.key, PublicModel: "gpt-5", PromptCacheKey: "quota-session", Body: []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`)})
	if err != nil || result.StatusCode != 200 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	io.Copy(io.Discard, result.Body)
	result.Finalize(Usage{}, "", "")
	result.Body.Close()
	if want := []uint64{f.accounts[0].ID, f.accounts[1].ID}; !slices.Equal(adapter.calls, want) {
		t.Fatalf("calls=%v want=%v", adapter.calls, want)
	}
}

func TestChatGPTImageJobAllAccountsSpentHasRateLimitError(t *testing.T) {
	f := newImageJobFixture(t, 2)
	setChatGPTQuota(t, f, "image_gen", 5)
	f.adapter.submit = func(provider.ImageJobRequest, func(provider.ImageCheckpoint) error) (provider.ImageCheckpoint, error) {
		return provider.ImageCheckpoint{}, statusError{429}
	}
	job := f.create(t, ImageJobInput{})
	f.service.processMediaJob(context.Background(), job.ID)
	stored := f.job(t, job.ID)
	if stored.Status != media.StatusFailed || stored.ErrorCode != "rate_limited" || len(f.adapter.Calls()) != 2 {
		t.Fatalf("status=%s error=%s calls=%v", stored.Status, stored.ErrorCode, f.adapter.Calls())
	}
}
