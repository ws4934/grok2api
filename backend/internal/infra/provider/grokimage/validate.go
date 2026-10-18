// Package grokimage holds the image parameter rules shared by the Grok Web and
// Grok Console adapters.
package grokimage

import (
	"errors"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// Validate applies the Grok image parameter rules; the messages are client-safe.
// mask, background and output_format are accepted and ignored as before.
func Validate(options provider.ImageRequestOptions) error {
	if options.Edit {
		switch normalize(options.AspectRatio) {
		case "", "auto", "1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3", "2:1", "1:2", "19.5:9", "9:19.5", "20:9", "9:20":
		default:
			return errors.New("aspect_ratio 不受支持")
		}
		switch normalize(options.Size) {
		case "", "auto", "1024x1024", "1024x1536", "1536x1024":
		default:
			return errors.New("size 必须是 auto、1024x1024、1024x1536 或 1536x1024")
		}
		switch normalize(options.Resolution) {
		case "", "1k", "2k":
		default:
			return errors.New("resolution 必须是 1k 或 2k")
		}
	}
	switch normalize(options.Quality) {
	case "", "low", "medium":
		return nil
	default:
		return errors.New("quality 必须是 low 或 medium")
	}
}

func normalize(value string) string { return strings.ToLower(strings.TrimSpace(value)) }
