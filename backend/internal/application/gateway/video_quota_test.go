package gateway

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/console"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type nativeConsoleVideoStub struct {
	*nativeConsoleImageStub
	mu     sync.Mutex
	calls  []uint64
	before func(provider.VideoRequest) error
	result provider.VideoResult
	quota  func(context.Context, account.Credential) (provider.QuotaSnapshot, error)
}

func (a *nativeConsoleVideoStub) GenerateVideo(_ context.Context, r provider.VideoRequest) (provider.VideoResult, error) {
	a.mu.Lock()
	a.calls = append(a.calls, r.Credential.ID)
	a.mu.Unlock()
	if a.before != nil {
		if err := a.before(r); err != nil {
			return provider.VideoResult{}, err
		}
	}
	if a.result.URL != "" {
		return a.result, nil
	}
	return provider.VideoResult{AssetID: "native-video-test", ContentType: "video/mp4"}, nil
}

func (a *nativeConsoleVideoStub) SyncQuota(ctx context.Context, c account.Credential) (provider.QuotaSnapshot, error) {
	return a.quota(ctx, c)
}

func newNativeVideoFixture(t *testing.T, balances ...int) (nativeImageFixture, *nativeConsoleVideoStub) {
	t.Helper()
	f := newNativeImageFixture(t, balances...)
	a := &nativeConsoleVideoStub{nativeConsoleImageStub: f.adapter}
	a.quota = func(ctx context.Context, c account.Credential) (provider.QuotaSnapshot, error) {
		windows, err := f.accounts.GetQuotaWindows(ctx, []uint64{c.ID})
		return provider.QuotaSnapshot{Windows: windows[c.ID], SyncedAt: time.Now().UTC()}, err
	}
	registry := provider.NewRegistry(a)
	f.service.providers = registry
	f.service.selector.tierOrders = registry
	f.service.accounts = accountapp.NewService(f.accounts, f.audits, memory.NewDeviceSessionStore(), memory.NewStickyStore(), registry, testCipher(t), nil)
	f.service.ConfigureMedia(f.jobs, 2)
	return f, a
}

func createNativeVideo(t *testing.T, f nativeImageFixture, seconds int, resolution string) media.Job {
	t.Helper()
	job, err := f.service.CreateVideo(context.Background(), VideoInput{RequestID: "native-video-test", ClientKey: f.key,
		PublicModel: "Console/grok-imagine-video-1.5", Prompt: "a landscape", Duration: seconds, Resolution: resolution})
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestConsoleVideoLargeRequestPrefersUnusedBudget(t *testing.T) {
	f, _ := newNativeVideoFixture(t, 94, 100)
	job := createNativeVideo(t, f, 15, "1080p")
	if job.AccountID != f.ids[1] {
		t.Fatalf("$3.75 video chose account %d with $0.94, want unused account %d", job.AccountID, f.ids[1])
	}
}

func TestConsoleVideoCompletionDebitsCentsAcrossSharedModes(t *testing.T) {
	f, a := newNativeVideoFixture(t, 100)
	job := createNativeVideo(t, f, 6, "720p") // Native cost: 84 cents, not one video unit.
	f.service.processMediaJob(context.Background(), job.ID)
	stored, err := f.jobs.GetMediaJob(context.Background(), job.ID, f.key.ID)
	if err != nil || stored.Status != media.StatusCompleted || len(a.calls) != 1 {
		t.Fatalf("job=%+v calls=%v err=%v", stored, a.calls, err)
	}
	windows, err := f.accounts.GetQuotaWindows(context.Background(), f.ids)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range windows[f.ids[0]] {
		if w.Remaining != 16 {
			t.Errorf("%s remaining=%d, want 16 cents", w.Mode, w.Remaining)
		}
	}
	f.checkBilling(t, 84)
	// No in-flight slot survives completion.
	current, err := f.service.selector.concurrency.Current(context.Background(), accountConcurrencyKey(f.ids[0]))
	if err != nil || current != 0 {
		t.Fatalf("concurrency=%d err=%v", current, err)
	}
}

func TestConsoleVideoBudgetPreferenceNormalAndSegmented(t *testing.T) {
	for _, segmented := range []bool{false, true} {
		name := "normal"
		if segmented {
			name = "segmented"
		}
		t.Run(name, func(t *testing.T) {
			for _, units := range []int{84, 375} {
				ctx := context.Background()
				s := NewSelector(nil, memory.NewConcurrencyLimiter(), nil, grokTestRegistry, time.Hour, time.Second, time.Minute)
				s.UpdateSegmentedSelector(segmented, 100, 8)
				session := &selectionSession{selector: s, provider: account.ProviderConsole, upstreamModel: "grok-imagine-video-1.5", quotaMode: console.QuotaModeVideo, quotaUnits: units}
				for i := range 100 {
					remaining, priority := 94, 100
					if i == 99 {
						remaining, priority = 100, 0
					}
					session.values = append(session.values, account.RoutingCandidate{
						Credential:           account.Credential{ID: uint64(i + 1), Provider: account.ProviderConsole, Priority: priority},
						QuotaWindow:          &account.QuotaWindow{Mode: console.QuotaModeVideo, Remaining: remaining, Total: 100, Source: account.QuotaSourceUpstream},
						ModelCapabilityKnown: true, SupportsModel: true,
					})
					session.normalCandidates = append(session.normalCandidates, i)
				}
				lease, err := session.Acquire(ctx, nil, false)
				if err != nil {
					t.Fatal(err)
				}
				want := uint64(100) // A large video must outrank a higher-priority spent account.
				if units == 84 {
					want = 1 // Both budgets cover this smaller video; preserve the explicit priority.
				}
				if lease.Credential.ID != want {
					t.Errorf("native units=%d chose=%d, want=%d", units, lease.Credential.ID, want)
				}
				lease.Release()
			}
		})
	}
}

func TestConsoleVideoDoesNotHardRejectOverBudgetOrUnknownAllowance(t *testing.T) {
	for _, remaining := range []int{100, 94, -1} {
		f, a := newNativeVideoFixture(t, remaining)
		job := createNativeVideo(t, f, 15, "1080p")
		f.service.processMediaJob(context.Background(), job.ID)
		stored, err := f.jobs.GetMediaJob(context.Background(), job.ID, f.key.ID)
		if err != nil || stored.Status != media.StatusCompleted || !slices.Equal(a.calls, f.ids) {
			t.Fatalf("remaining=%d job=%+v calls=%v err=%v", remaining, stored, a.calls, err)
		}
		f.checkBilling(t, 375)
	}
}

func prioritizeNativeAccount(t *testing.T, f nativeImageFixture, id uint64) {
	t.Helper()
	c, err := f.accounts.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	c.Priority = 100
	if _, err := f.accounts.Update(context.Background(), c); err != nil {
		t.Fatal(err)
	}
}

func TestConsoleVideoReselectsQueuedAccountAfterImageSpending(t *testing.T) {
	f, a := newNativeVideoFixture(t, 100, 100)
	prioritizeNativeAccount(t, f, f.ids[0])
	job := createNativeVideo(t, f, 15, "1080p")
	if job.AccountID != f.ids[0] {
		t.Fatal("fixture did not initially choose the high-priority unused account")
	}
	result, err := f.generate(t, 1, "url")
	if err != nil {
		t.Fatal(err)
	}
	_ = readFanned(t, result)
	if f.adapter.calls[0].account != f.ids[0] {
		t.Fatal("fixture did not spend four cents on the queued video's account")
	}
	f.service.processMediaJob(context.Background(), job.ID)
	stored, err := f.jobs.GetMediaJob(context.Background(), job.ID, f.key.ID)
	if err != nil || stored.Status != media.StatusCompleted || !slices.Equal(a.calls, []uint64{f.ids[1]}) {
		t.Fatalf("queued job used stale account: job=%+v calls=%v err=%v", stored, a.calls, err)
	}
	f.checkBilling(t, 379)
}

func TestConsoleVideoConcurrentJobsReserveDifferentBudgets(t *testing.T) {
	f, a := newNativeVideoFixture(t, 100, 100)
	prioritizeNativeAccount(t, f, f.ids[0])
	first, second := createNativeVideo(t, f, 15, "1080p"), createNativeVideo(t, f, 15, "1080p")
	if first.AccountID != f.ids[0] || second.AccountID != f.ids[0] {
		t.Fatal("fixture must queue both jobs on the initially preferred account")
	}
	entered, proceed := make(chan uint64, 2), make(chan struct{})
	a.before = func(r provider.VideoRequest) error {
		entered <- r.Credential.ID
		<-proceed
		return nil
	}
	var workers sync.WaitGroup
	workers.Go(func() { f.service.processMediaJob(context.Background(), first.ID) })
	workers.Go(func() { f.service.processMediaJob(context.Background(), second.ID) })
	selected := make([]uint64, 0, 2)
	for range 2 {
		select {
		case id := <-entered:
			selected = append(selected, id)
		case <-time.After(5 * time.Second):
			close(proceed)
			workers.Wait()
			t.Fatal("concurrent videos did not reach two usable budgets")
		}
	}
	close(proceed)
	workers.Wait()
	if selected[0] == selected[1] {
		t.Fatalf("concurrent videos shared the same fresh budget: %v", selected)
	}
	for _, id := range []string{first.ID, second.ID} {
		stored, err := f.jobs.GetMediaJob(context.Background(), id, f.key.ID)
		if err != nil || stored.Status != media.StatusCompleted {
			t.Fatalf("job=%+v err=%v", stored, err)
		}
	}
	f.checkBilling(t, 750)
}

func TestConsoleVideoQuotaSpentBeforeOutputStorageFailure(t *testing.T) {
	f, a := newNativeVideoFixture(t, 100)
	a.result = provider.VideoResult{URL: "https://example.test/video.mp4", ContentType: "video/mp4"}
	job := createNativeVideo(t, f, 6, "720p")
	f.service.processMediaJob(context.Background(), job.ID) // No local media store configured.
	stored, err := f.jobs.GetMediaJob(context.Background(), job.ID, f.key.ID)
	if err != nil || stored.Status != media.StatusFailed || len(a.calls) != 1 {
		t.Fatalf("job=%+v calls=%v err=%v", stored, a.calls, err)
	}
	windows, err := f.accounts.GetQuotaWindows(context.Background(), f.ids)
	if err != nil || windows[f.ids[0]][0].Remaining != 16 {
		t.Fatalf("upstream spend lost after local output failure: windows=%+v err=%v", windows, err)
	}
	f.checkBilling(t, 0)
}

func TestConsoleVideoRejectedCreateReleasesBudgetAndFailsOver(t *testing.T) {
	f, a := newNativeVideoFixture(t, 100, 100)
	prioritizeNativeAccount(t, f, f.ids[0])
	a.before = func(r provider.VideoRequest) error {
		if r.Credential.ID == f.ids[0] {
			return provider.WrapVideoStage(provider.VideoStageCreate, 402, errors.New("spending limit reached"))
		}
		return nil
	}
	job := createNativeVideo(t, f, 15, "1080p")
	f.service.processMediaJob(context.Background(), job.ID)
	stored, err := f.jobs.GetMediaJob(context.Background(), job.ID, f.key.ID)
	if err != nil || stored.Status != media.StatusCompleted || !slices.Equal(a.calls, f.ids) {
		t.Fatalf("job=%+v calls=%v err=%v", stored, a.calls, err)
	}
	f.checkBilling(t, 375)
}

func TestConsoleVideoBudgetDoesNotUseStoredClientPrice(t *testing.T) {
	f, _ := newNativeVideoFixture(t, 94, 100)
	job := createNativeVideo(t, f, 15, "1080p")
	job.Price.CostInUSDTicks = 100_000_000 // Client price is one cent, native estimate remains 375.
	if err := f.jobs.UpdateMediaJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	f.service.processMediaJob(context.Background(), job.ID)
	windows, err := f.accounts.GetQuotaWindows(context.Background(), f.ids)
	if err != nil || windows[f.ids[0]][0].Remaining != 94 || windows[f.ids[1]][0].Remaining != 0 {
		t.Fatalf("client price contaminated native quota: windows=%+v err=%v", windows, err)
	}
	f.checkBilling(t, 1)
}

func TestConsoleVideoResumeStaysOnSubmittingAccount(t *testing.T) {
	f, a := newNativeVideoFixture(t, 94, 100)
	job := createNativeVideo(t, f, 15, "1080p")
	now := time.Now().UTC()
	claimed, ok, err := f.jobs.TryClaimMediaJob(context.Background(), job.ID, now, now.Add(-time.Second), "dead-native-video-worker")
	if err != nil || !ok {
		t.Fatalf("claim=%v err=%v", ok, err)
	}
	claimed.AccountID, claimed.AccountName = f.ids[0], "A"
	claimed.Submitted, claimed.Checkpoint = true, `{"request_id":"accepted-on-A"}`
	if err := f.jobs.UpdateMediaJob(context.Background(), claimed); err != nil {
		t.Fatal(err)
	}
	f.service.processMediaJob(context.Background(), job.ID)
	stored, err := f.jobs.GetMediaJob(context.Background(), job.ID, f.key.ID)
	if err != nil || stored.Status != media.StatusCompleted || !slices.Equal(a.calls, []uint64{f.ids[0]}) {
		t.Fatalf("resumed video switched account: job=%+v calls=%v err=%v", stored, a.calls, err)
	}
	f.checkBilling(t, 375)
}

func TestConsoleVideoUncertainSubmissionKeepsEstimatedBudget(t *testing.T) {
	f, a := newNativeVideoFixture(t, 100, 100)
	prioritizeNativeAccount(t, f, f.ids[0])
	a.before = func(r provider.VideoRequest) error {
		if err := r.SaveCheckpoint(`{"submitting":true}`); err != nil {
			return err
		}
		return provider.WrapVideoStage(provider.VideoStageSubmitted, 0, errors.New("create response lost"))
	}
	job := createNativeVideo(t, f, 15, "1080p")
	f.service.processMediaJob(context.Background(), job.ID)
	stored, err := f.jobs.GetMediaJob(context.Background(), job.ID, f.key.ID)
	if err != nil || stored.Status != media.StatusUncertain || !slices.Equal(a.calls, []uint64{f.ids[0]}) {
		t.Fatalf("uncertain video was replayed: job=%+v calls=%v err=%v", stored, a.calls, err)
	}
	windows, err := f.accounts.GetQuotaWindows(context.Background(), f.ids)
	if err != nil || windows[f.ids[0]][0].Remaining != 0 || windows[f.ids[1]][0].Remaining != 100 {
		t.Fatalf("unresolved submission's budget was reused: windows=%+v err=%v", windows, err)
	}
	f.checkBilling(t, 0)
}

type failedVideoQuotaWriteRepository struct{ repository.AccountRepository }

func (failedVideoQuotaWriteRepository) DecrementQuotaWindowBy(context.Context, uint64, string, int, time.Time) (bool, error) {
	return false, errors.New("quota write unavailable")
}

func TestConsoleVideoQuotaWriteFailureKeepsLocalDebit(t *testing.T) {
	f, _ := newNativeVideoFixture(t, 100)
	s := f.service.selector
	session, err := s.beginSelectionSession(context.Background(), account.ProviderConsole, 0, "grok-imagine-video-1.5", console.QuotaModeVideo, "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := session.Acquire(context.Background(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	release, err := s.reserveVideoQuota(lease, 84)
	if err != nil {
		t.Fatal(err)
	}
	f.service.accounts = accountapp.NewService(failedVideoQuotaWriteRepository{f.accounts}, f.audits, memory.NewDeviceSessionStore(), memory.NewStickyStore(), f.service.providers, testCipher(t), nil)
	f.service.consumeMediaQuota(model.Route{Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-video-1.5"}, f.ids[0], console.QuotaModeVideo, 84)
	release()
	s.quotaMu.RLock()
	remaining := s.quotaConsumptionSnapshot(account.ProviderConsole).remaining(*lease.routingCandidate)
	s.quotaMu.RUnlock()
	if remaining != 16 {
		t.Fatalf("quota persistence failure erased observed native spend: remaining=%d", remaining)
	}
	f.checkBilling(t, 0)
}
