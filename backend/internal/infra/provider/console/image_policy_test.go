package console

import (
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestImagePolicyUsesEffectiveParametersForPriceAndQuota(t *testing.T) {
	a := &Adapter{}
	o, err := a.NormalizeImageRequest(provider.ImageRequestOptions{UpstreamModel: "grok-imagine-image-2.0", Edit: true, Count: 2, InputImages: 7, Size: "1536x1024", Resolution: "4K", Quality: "high"})
	if err != nil || o.InputImages != 3 || o.Resolution != "2k" || o.Quality != "medium" || o.AspectRatio != "3:2" || o.Size != "" {
		t.Fatalf("effective=%+v err=%v", o, err)
	}
	price, ok := audit.EstimateOfficialImageEditCost(o.UpstreamModel, o.Resolution, o.Quality, o.Count, o.InputImages)
	if !ok || price.CostInUSDTicks != 1_900_000_000 {
		t.Fatalf("effective edit price=%+v priced=%v", price, ok)
	}
	if units, ok := a.ImageQuotaUnits(QuotaModeImage, o); !ok || units != 19 {
		t.Fatalf("quota=%d known=%v, want cents not image count", units, ok)
	}
	for _, edit := range []bool{false, true} {
		defaulted, err := a.NormalizeImageRequest(provider.ImageRequestOptions{UpstreamModel: o.UpstreamModel, Edit: edit, InputImages: 1, Count: 1, Quality: "auto"})
		want := "low"
		if edit {
			want = "medium"
		}
		if err != nil || defaulted.Quality != want || defaulted.Resolution != "1k" {
			t.Fatalf("edit=%v auto=%+v err=%v", edit, defaulted, err)
		}
	}
	legacy, err := a.NormalizeImageRequest(provider.ImageRequestOptions{UpstreamModel: "grok-imagine-image", Count: 2, Quality: "high"})
	if err != nil || legacy.Quality != "" {
		t.Fatalf("legacy model retained an ignored price tier: %+v %v", legacy, err)
	}
	if units, ok := a.ImageQuotaUnits(QuotaModeImage, legacy); !ok || units != 4 {
		t.Fatalf("legacy quota=%d known=%v", units, ok)
	}
}
