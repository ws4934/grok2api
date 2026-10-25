// Package grokimage holds the image parameter rules and output helpers shared
// by the Grok Web and Grok Console adapters.
package grokimage

import (
	"strings"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// Grok takes any image parameters a client sends: what it cannot use is
// mapped to the nearest value it has or dropped, never refused.

// Resolution maps a requested tier to the 1k or 2k Grok renders; "" when the
// client asked for none it can read.
func Resolution(value string) string {
	switch pixels := provider.ImageTierPixels(value); {
	case pixels == 0:
		return ""
	case pixels >= 1536:
		return "2k"
	default:
		return "1k"
	}
}

// Quality maps a requested quality to the low or medium grok-imagine-image-2.0
// takes; "" leaves the default.
func Quality(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "auto":
		return ""
	case "low":
		return "low"
	default:
		return "medium"
	}
}
