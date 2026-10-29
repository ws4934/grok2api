package console

import (
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestVideoQuotaUsesNativeCentsForEffectiveParameters(t *testing.T) {
	a := &Adapter{}
	r := provider.NewRegistry(a)
	for _, tc := range []struct {
		model, resolution string
		seconds, refs     int
		first             bool
		want              int
	}{
		{"grok-imagine-video-1.5", "720p", 6, 0, false, 84},
		{"grok-imagine-video-1.5", "1080p", 15, 0, false, 375},
		// References fit to 14, keep the pinned first frame and render at 720p.
		{"grok-imagine-video-1.5", "1080p", 15, 20, true, 225},
		{"grok-imagine-video", "480p", 6, 0, true, 31},
	} {
		o, err := a.NormalizeVideoRequest(provider.VideoRequestOptions{Operation: provider.VideoOperationGenerate,
			UpstreamModel: tc.model, Resolution: tc.resolution, Duration: tc.seconds, ReferenceImages: tc.refs, HasImage: tc.first})
		if err != nil {
			t.Fatal(err)
		}
		units, known := r.VideoQuotaUnits(account.ProviderConsole, QuotaModeVideo, o)
		if !known || units != tc.want {
			t.Errorf("%+v quota=%d known=%v, want=%d", tc, units, known, tc.want)
		}
	}
	for _, o := range []provider.VideoRequestOptions{
		{Operation: provider.VideoOperationEdit, UpstreamModel: "grok-imagine-video"},
		{Operation: provider.VideoOperationGenerate, UpstreamModel: "unknown", Resolution: "720p", Duration: 6},
	} {
		if _, known := r.VideoQuotaUnits(account.ProviderConsole, QuotaModeVideo, o); known {
			t.Errorf("invented a quota estimate for %+v", o)
		}
	}
	if _, known := r.VideoQuotaUnits(account.ProviderConsole, QuotaModeImage, provider.VideoRequestOptions{}); known {
		t.Fatal("video estimator applied to a different quota product")
	}
}
