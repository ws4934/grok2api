package grokimage

import (
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// The cases mirror the rejections the inference transport enforced before
// validation moved behind providers.
func TestValidateReproducesTransportRules(t *testing.T) {
	for _, test := range []struct {
		name    string
		options provider.ImageRequestOptions
		want    string
	}{
		{name: "generation quality", options: provider.ImageRequestOptions{Quality: "high"}, want: "quality 必须是 low 或 medium"},
		{name: "edit quality", options: provider.ImageRequestOptions{Edit: true, Quality: "high"}, want: "quality 必须是 low 或 medium"},
		{name: "edit resolution", options: provider.ImageRequestOptions{Edit: true, Resolution: "4k"}, want: "resolution 必须是 1k 或 2k"},
		{name: "edit aspect ratio", options: provider.ImageRequestOptions{Edit: true, AspectRatio: "7:5"}, want: "aspect_ratio 不受支持"},
		{name: "edit size", options: provider.ImageRequestOptions{Edit: true, Size: "512x512"}, want: "size 必须是 auto、1024x1024、1024x1536 或 1536x1024"},
		{name: "edit aspect ratio before quality", options: provider.ImageRequestOptions{Edit: true, AspectRatio: "7:5", Quality: "high"}, want: "aspect_ratio 不受支持"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := Validate(test.options); err == nil || err.Error() != test.want {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}

	for _, options := range []provider.ImageRequestOptions{
		{},
		{Quality: " Medium ", Size: "512x512", AspectRatio: "7:5", Resolution: "4k"},
		{Edit: true, Quality: "low", Size: "1536x1024", AspectRatio: "19.5:9", Resolution: "2K"},
		{Edit: true, Resolution: "", HasMask: true, Background: "transparent", OutputFormat: "webp", ResponseFormat: "b64_json"},
		{Count: 4, Streaming: true, PartialImages: 3},
	} {
		if err := Validate(options); err != nil {
			t.Fatalf("%#v rejected: %v", options, err)
		}
	}
}
