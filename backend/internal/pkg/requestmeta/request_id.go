package requestmeta

import "context"

type requestIDContextKey struct{}

func WithRequestID(ctx context.Context, value string) context.Context {
	return context.WithValue(ctx, requestIDContextKey{}, value)
}
func RequestID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(requestIDContextKey{}).(string)
	return value
}
