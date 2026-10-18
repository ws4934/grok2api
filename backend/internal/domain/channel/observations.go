package channel

import "context"

type quotaObserverKey struct{}
type stageObserverKey struct{}

func WithStageObserver(ctx context.Context, observer func(string) error) context.Context {
	return context.WithValue(ctx, stageObserverKey{}, observer)
}
func ObserveStage(ctx context.Context, stage string) error {
	if observer, ok := ctx.Value(stageObserverKey{}).(func(string) error); ok {
		return observer(stage)
	}
	return nil
}

// WithQuotaObserver attaches the durable pre-send observer. The protocol must
// stop if recording consumption fails; it must never send first and record later.
func WithQuotaObserver(ctx context.Context, observer func(string) error) context.Context {
	return context.WithValue(ctx, quotaObserverKey{}, observer)
}
func ObserveQuotaSend(ctx context.Context, feature string) error {
	if observer, ok := ctx.Value(quotaObserverKey{}).(func(string) error); ok {
		return observer(feature)
	}
	return nil
}
