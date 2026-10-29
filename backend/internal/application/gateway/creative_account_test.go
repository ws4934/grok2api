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
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/console"
)

func TestCreativeAccountBypassesQuotaAndCooldownButNotScopeOrCapacity(t *testing.T) {
	f := newNativeImageFixture(t, 0, 100)
	ctx := WithCreativeAccount(context.Background(), f.ids[0])
	value, _ := f.accounts.Get(ctx, f.ids[0])
	f.service.selector.MarkFailure(ctx, value, http.StatusTooManyRequests, time.Hour)
	s := f.service.selector
	lease, err := s.AcquireForKey(ctx, account.ProviderConsole, 0, "grok-imagine-image-2.0", console.QuotaModeImage, "", nil, false, clientkey.AccountScope{})
	if err != nil || lease.Credential.ID != value.ID || lease.QuotaProbe || !lease.creativePinned {
		t.Fatalf("diagnostic lease=%+v err=%v", lease, err)
	}
	lease.Release()
	deniedScope := clientkey.AccountScope{Providers: clientkey.ProviderScopeOf(account.ProviderBuild)}
	if lease, err := s.AcquireForKey(ctx, account.ProviderConsole, 0, "", "", "", nil, false, deniedScope); err == nil || lease != nil {
		t.Fatal("administrator account selection bypassed the client key's provider scope")
	}
	var releases []func()
	for range value.MaxConcurrent {
		release, ok, err := s.concurrency.Acquire(ctx, accountConcurrencyKey(value.ID), value.MaxConcurrent)
		if err != nil || !ok {
			t.Fatalf("reserve capacity: acquired=%v err=%v", ok, err)
		}
		releases = append(releases, release)
	}
	if lease, err := s.AcquireForKey(ctx, account.ProviderConsole, 0, "grok-imagine-image-2.0", console.QuotaModeImage, "", nil, false, clientkey.AccountScope{}); err == nil || lease != nil {
		t.Fatal("pinned account bypassed its concurrency limit")
	}
	for _, release := range releases {
		release()
	}
	// A request-scoped exclusion is terminal; it never selects the healthy peer.
	if lease, err := s.AcquireForKey(ctx, account.ProviderConsole, 0, "grok-imagine-image-2.0", console.QuotaModeImage, "", map[uint64]bool{value.ID: true}, false, clientkey.AccountScope{}); err == nil || lease != nil {
		t.Fatalf("excluded pinned account was retried: lease=%+v err=%v", lease, err)
	}
	if lease, err := s.AcquirePinnedForKey(ctx, account.ProviderConsole, f.ids[1], 0, "", "", true, clientkey.AccountScope{}); err == nil || lease != nil {
		t.Fatal("pinned request acquired a different stored-response owner")
	}
	value.Enabled = false
	if _, err := f.accounts.Update(ctx, value); err != nil {
		t.Fatal(err)
	}
	// Credential material is checked on acquisition even when a plan is cached.
	if lease, err := s.AcquireForKey(ctx, account.ProviderConsole, 0, "grok-imagine-image-2.0", console.QuotaModeImage, "", nil, false, clientkey.AccountScope{}); err == nil || lease != nil {
		t.Fatalf("disabled credential was acquired: lease=%+v err=%v", lease, err)
	}
}

func TestCreativePinnedImageKeepsFullBatchDespiteZeroCachedBalance(t *testing.T) {
	f := newNativeImageFixture(t, 0, 100)
	result, err := f.service.GenerateImage(WithCreativeAccount(context.Background(), f.ids[0]), ImageGenerationInput{
		RequestID: "pinned-images", ClientKey: f.key, PublicModel: "Console/grok-imagine-image-2.0", Prompt: "test",
		Count: 3, Quality: "low", Resolution: "1k", ResponseFormat: "url",
	})
	if err != nil {
		t.Fatal(err)
	}
	if response := readFanned(t, result); len(response.Data) != 3 {
		t.Fatalf("outputs=%d", len(response.Data))
	}
	if len(f.adapter.calls) != 1 || f.adapter.calls[0].account != f.ids[0] || f.adapter.calls[0].options.Count != 3 {
		t.Fatalf("native batches=%+v", f.adapter.calls)
	}
	f.checkBilling(t, 12)
}

func TestCreativePinnedImageFailureNeverUsesHealthyPeer(t *testing.T) {
	f := newNativeImageFixture(t, 0, 100)
	f.service.maxAttempts.Store(unlimitedRoutingAttempts)
	f.adapter.failure = errors.New("diagnostic transport failure")
	result, err := f.service.GenerateImage(WithCreativeAccount(context.Background(), f.ids[0]), ImageGenerationInput{
		RequestID: "pinned-failure", ClientKey: f.key, PublicModel: "Console/grok-imagine-image-2.0", Prompt: "test", Count: 1,
	})
	if result != nil || err == nil || len(f.adapter.calls) != 1 || f.adapter.calls[0].account != f.ids[0] {
		t.Fatalf("result=%v calls=%+v err=%v", result, f.adapter.calls, err)
	}
	f.checkBilling(t, 0)
}

func TestCreativePinnedVideoSurvivesQueueAndFailsWithoutSwitching(t *testing.T) {
	f, adapter := newNativeVideoFixture(t, 0, 100)
	f.service.videoMaxAttempts.Store(unlimitedRoutingAttempts)
	adapter.before = func(provider.VideoRequest) error { return statusError{http.StatusPaymentRequired} }
	job, err := f.service.CreateVideo(WithCreativeAccount(context.Background(), f.ids[0]), VideoInput{
		RequestID: "pinned-video", ClientKey: f.key, PublicModel: "Console/grok-imagine-video-1.5", Prompt: "test", Duration: 6, Resolution: "720p",
	})
	if err != nil || job.AccountID != f.ids[0] {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	stored, err := f.jobs.GetMediaJob(context.Background(), job.ID, f.key.ID)
	if err != nil || stored.Params.PinnedAccountID != f.ids[0] {
		t.Fatalf("persisted execution constraint=%+v err=%v", stored.Params, err)
	}
	// A recovered worker receives a fresh context, not the HTTP request context.
	f.service.processMediaJob(context.Background(), job.ID)
	stored, err = f.jobs.GetMediaJob(context.Background(), job.ID, f.key.ID)
	if err != nil || stored.Status != media.StatusFailed || !slices.Equal(adapter.calls, []uint64{f.ids[0]}) {
		t.Fatalf("status=%s calls=%v err=%v", stored.Status, adapter.calls, err)
	}
	f.checkBilling(t, 0)
}

func TestCreativePinnedAsyncImageSurvivesQueueAndFailsWithoutSwitching(t *testing.T) {
	f := newImageJobFixture(t, 2)
	f.service.maxAttempts.Store(unlimitedRoutingAttempts)
	pinned := f.accounts[1].ID
	f.adapter.submit = func(provider.ImageJobRequest, func(provider.ImageCheckpoint) error) (provider.ImageCheckpoint, error) {
		return provider.ImageCheckpoint{}, statusError{http.StatusTooManyRequests}
	}
	job, err := f.service.CreateImageJob(WithCreativeAccount(context.Background(), pinned), ImageJobInput{
		RequestID: "pinned-async-image", ClientKey: f.key, PublicModel: "gpt-image-2", Prompt: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	stored := f.job(t, job.ID)
	if stored.Params.PinnedAccountID != pinned {
		t.Fatalf("persisted pin=%+v", stored.Params)
	}
	f.service.processMediaJob(context.Background(), job.ID)
	stored = f.job(t, job.ID)
	if stored.Status != media.StatusFailed || !slices.Equal(f.adapter.Calls(), []string{fmt.Sprintf("submit:%d", pinned)}) {
		t.Fatalf("status=%s calls=%v", stored.Status, f.adapter.Calls())
	}
}

type creativeResponseAdapter struct{ calls []uint64 }

func (*creativeResponseAdapter) Provider() account.Provider { return account.ProviderBuild }
func (*creativeResponseAdapter) Definition() provider.Definition {
	return testConversationDefinition(account.ProviderBuild)
}
func (a *creativeResponseAdapter) ForwardResponse(_ context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	a.calls = append(a.calls, request.Credential.ID)
	return &provider.Response{StatusCode: 429, Status: "429 Too Many Requests", Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"diagnostic quota limit","type":"rate_limit_error"}}`))}, nil
}

func TestCreativePinnedChatReturnsFailureWithoutSwitching(t *testing.T) {
	adapter := &creativeResponseAdapter{}
	f := newResponseFixture(t, adapter, 2)
	f.service.maxAttempts.Store(unlimitedRoutingAttempts)
	pinned := f.accounts[1].ID
	ctx := WithCreativeAccount(context.Background(), pinned)
	f.service.selector.MarkFailure(ctx, f.accounts[1], 429, time.Hour)
	result, err := f.service.CreateResponse(ctx, Input{RequestID: "pinned-chat", ClientKey: f.key, PublicModel: "grok-4.6", Body: []byte(`{"model":"grok-4.6","input":"test","store":false}`)})
	if result != nil {
		_, _ = io.Copy(io.Discard, result.Body)
		_ = result.Body.Close()
		result.Finalize(Usage{}, "", "")
		if result.StatusCode != 429 {
			t.Fatalf("status=%d", result.StatusCode)
		}
	} else if err == nil {
		t.Fatal("upstream failure disappeared")
	}
	if !slices.Equal(adapter.calls, []uint64{pinned}) {
		t.Fatalf("accounts used=%v", adapter.calls)
	}
}
