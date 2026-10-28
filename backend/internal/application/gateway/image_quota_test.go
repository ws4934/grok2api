package gateway

import (
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/console"
)

func TestImageQuotaPoolSurvivesOtherModeReloadAndHonorsNewSnapshots(t *testing.T) {
	f := newNativeImageFixture(t, 12)
	now := time.Now().UTC()
	window := account.QuotaWindow{Mode: console.QuotaModeImage, Remaining: 12, Source: account.QuotaSourceUpstream, SyncedAt: &now}
	candidate := account.RoutingCandidate{Credential: account.Credential{ID: f.ids[0], Provider: account.ProviderConsole}, QuotaWindow: &window}
	lease := &accountLease{Credential: candidate.Credential, QuotaMode: console.QuotaModeImage, routingCandidate: &candidate}
	o := provider.ImageRequestOptions{UpstreamModel: "grok-imagine-image-2.0", Count: 2, Resolution: "1k", Quality: "low"}
	_, release, err := f.service.reserveImageQuota(lease, o)
	if err != nil {
		t.Fatal(err)
	}
	s := f.service.selector
	s.clearQuotaConsumption(account.ProviderConsole)
	// Its in-flight 8 cents survived the mode cache reload: only one more fits.
	got, secondRelease, err := f.service.reserveImageQuota(lease, o)
	if err != nil || got.Count != 1 {
		t.Fatalf("in-flight pool hold lost after reload: count=%d err=%v", got.Count, err)
	}
	secondRelease()
	s.ConsumeQuota(account.ProviderConsole, f.ids[0], console.QuotaModeImage, 8)
	release()
	release() // Cleanup is idempotent.
	s.clearQuotaConsumption(account.ProviderConsole)
	videoWindow := window
	videoWindow.Mode = console.QuotaModeVideo
	videoCandidate := candidate
	videoCandidate.QuotaWindow = &videoWindow
	s.ConsumeQuota(account.ProviderConsole, f.ids[0], console.QuotaMode, 4)
	s.quotaMu.RLock()
	exhausted := quotaWindowExhausted(videoCandidate, s.quotaConsumptionSnapshot(account.ProviderConsole))
	s.quotaMu.RUnlock()
	if !exhausted {
		t.Fatal("stale video snapshot did not see the same Console pool spent by images/chat")
	}
	// A later authoritative allowance replaces the baseline, not the holds.
	later := now.Add(time.Minute)
	window.SyncedAt = &later
	s.clearQuotaConsumption(account.ProviderConsole)
	got, release, err = f.service.reserveImageQuota(lease, o)
	if err != nil || got.Count != 2 {
		t.Fatalf("new upstream snapshot did not restore allowance: count=%d err=%v", got.Count, err)
	}
	release()
	f.checkBilling(t, 0)
}
