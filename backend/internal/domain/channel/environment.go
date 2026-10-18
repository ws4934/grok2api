package channel

import "context"

type Environment struct {
	ProxyURL string
	NodeID   uint64
	NodeName string
	// UserAgent and CFCookies carry a Cloudflare clearance bound to this egress.
	// They are empty unless clearance solving is enabled for the channel.
	UserAgent string
	CFCookies string
	// Report feeds transport and challenge outcomes back to egress health.
	Report func(status int, err error)
	// InvalidateClearance drops the clearance bound to this egress.
	InvalidateClearance func()
}
type environmentKey struct{}

func WithEnvironment(ctx context.Context, v Environment) context.Context {
	return context.WithValue(ctx, environmentKey{}, v)
}
func EnvironmentFrom(ctx context.Context) Environment {
	v, _ := ctx.Value(environmentKey{}).(Environment)
	return v
}
