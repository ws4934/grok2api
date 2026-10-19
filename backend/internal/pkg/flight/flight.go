// Package flight shares one load among concurrent callers of the same key.
package flight

import (
	"context"
	"time"

	"golang.org/x/sync/singleflight"
)

// Do runs load once per key for all concurrent callers. The load keeps ctx's
// values but not its cancellation and is bounded by timeout, so the caller
// that started it cannot fail the others by leaving. Each caller still stops
// waiting when its own ctx ends.
func Do[T any](ctx context.Context, group *singleflight.Group, key string, timeout time.Duration, load func(context.Context) (T, error)) (T, error) {
	var zero T
	// A caller that already left must not start a detached load.
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	results := group.DoChan(key, func() (any, error) {
		loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		defer cancel()
		return load(loadCtx)
	})
	select {
	case <-ctx.Done():
		return zero, ctx.Err()
	case result := <-results:
		value, _ := result.Val.(T)
		return value, result.Err
	}
}
