package provider

import (
	"errors"
	"math"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
)

func TestVideoKeyframeTimeValidation(t *testing.T) {
	frame := func(timestamp float64) media.VideoKeyframe {
		return media.VideoKeyframe{ImageURL: "https://example.com/pin.png", TimestampSeconds: timestamp}
	}
	for _, tc := range []struct {
		name   string
		frames []media.VideoKeyframe
		valid  bool
	}{
		{"none", nil, true},
		{"valid", []media.VideoKeyframe{frame(1), frame(2), frame(4), frame(6)}, true},
		{"fractional", []media.VideoKeyframe{frame(1.0 / 3), frame(2.0 / 3)}, true},
		{"start", []media.VideoKeyframe{frame(0)}, false},
		{"end", []media.VideoKeyframe{frame(8)}, false},
		{"after", []media.VideoKeyframe{frame(9)}, false},
		{"nan", []media.VideoKeyframe{frame(math.NaN())}, false},
		{"infinite", []media.VideoKeyframe{frame(math.Inf(1))}, false},
		{"same-slot", []media.VideoKeyframe{frame(2.01), frame(2.1)}, false},
		{"missing-image", []media.VideoKeyframe{{TimestampSeconds: 2}}, false},
		{"too-many", []media.VideoKeyframe{frame(1), frame(2), frame(3), frame(4), frame(5)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateVideoKeyframes(tc.frames, 8)
			if (err == nil) != tc.valid || (err != nil && !errors.Is(err, ErrVideoParameterInvalid)) {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}
