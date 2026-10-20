package grokimage

import (
	"context"
	"strings"
	"time"

	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// OutputAttempts bounds the download and storage attempts for one generated image.
const OutputAttempts = 3

// SaveWithRetry stores a generated image, retrying only the local write: the
// upstream generation is never repeated.
func SaveWithRetry(ctx context.Context, assets provider.ImageAssetStore, raw []byte) (mediadomain.Asset, error) {
	var lastErr error
	for attempt := 0; attempt < OutputAttempts; attempt++ {
		asset, err := assets.SaveImage(ctx, raw)
		if err == nil {
			return asset, nil
		}
		lastErr = err
		if ctx.Err() != nil || attempt+1 >= OutputAttempts {
			break
		}
		if err := WaitRetry(ctx, attempt); err != nil {
			return mediadomain.Asset{}, err
		}
	}
	return mediadomain.Asset{}, lastErr
}

// WaitRetry waits out the backoff after a failed attempt, or returns ctx's error.
func WaitRetry(ctx context.Context, attempt int) error {
	delays := [...]time.Duration{200 * time.Millisecond, 750 * time.Millisecond}
	timer := time.NewTimer(delays[min(attempt, len(delays)-1)])
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// TrustedAssetHost reports whether host serves Grok-generated images.
func TrustedAssetHost(host string) bool {
	return strings.EqualFold(host, "assets.grok.com") || strings.EqualFold(host, "imagine-public.x.ai") || strings.EqualFold(host, "imgen.x.ai")
}
