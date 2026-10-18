package channel

import "context"

type Environment struct {
	ProxyURL string
	NodeID   uint64
}
type environmentKey struct{}

func WithEnvironment(ctx context.Context, v Environment) context.Context {
	return context.WithValue(ctx, environmentKey{}, v)
}
func EnvironmentFrom(ctx context.Context) Environment {
	v, _ := ctx.Value(environmentKey{}).(Environment)
	return v
}
