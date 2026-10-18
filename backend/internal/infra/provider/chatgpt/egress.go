package chatgpt

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
)

var scope = egressdomain.Scope(account.ProviderChatGPT)

// environment is the egress binding of one upstream call.
type environment struct {
	ProxyURL string
	// UserAgent and CFCookies carry a Cloudflare clearance bound to the egress.
	// They are empty unless clearance solving is enabled for ChatGPT.
	UserAgent string
	CFCookies string
}

type environmentKey struct{}
type submitKey struct{}

func withEnvironment(ctx context.Context, v environment) context.Context {
	return context.WithValue(ctx, environmentKey{}, v)
}

func environmentFrom(ctx context.Context) environment {
	v, _ := ctx.Value(environmentKey{}).(environment)
	return v
}

// markSubmitted runs right before the irreversible generation request; from
// then on the call must not be repeated.
func markSubmitted(ctx context.Context) {
	if fn, ok := ctx.Value(submitKey{}).(func()); ok {
		fn()
	}
}

// lease binds ctx to an egress of credential.
func (a *Adapter) lease(ctx context.Context, credential account.Credential) (context.Context, *infraegress.Lease, error) {
	lease, err := a.egress.AcquireCredential(ctx, scope, credential)
	if err != nil {
		return ctx, nil, err
	}
	env := environment{ProxyURL: lease.ProxyURL}
	if lease.CFCookies != "" && a.egress.ClearanceEnabled(scope) {
		env.UserAgent, env.CFCookies = lease.UserAgent, lease.CFCookies
	}
	return withEnvironment(ctx, env), lease, nil
}

// report feeds one outcome back to egress health and reports whether the
// failure belongs to the egress (transport or Cloudflare challenge).
func (a *Adapter) report(ctx context.Context, lease *infraegress.Lease, err error) bool {
	ctx = context.WithoutCancel(ctx)
	var transport *transportError
	var upstream *upstreamError
	switch {
	case err == nil:
		a.egress.FeedbackForScope(ctx, scope, lease.NodeID, http.StatusOK, nil)
	case errors.As(err, &transport):
		a.egress.FeedbackForScope(ctx, scope, lease.NodeID, 0, err)
		return true
	case errors.As(err, &upstream) && upstream.Challenge:
		// With managed clearance the next lease re-solves on the same node;
		// otherwise the node is demoted so the account moves elsewhere.
		if a.egress.ClearanceEnabled(scope) {
			lease.InvalidateClearance()
		} else {
			a.egress.FeedbackForScope(ctx, scope, lease.NodeID, http.StatusForbidden, nil)
		}
		return true
	}
	return false
}

// attempt runs fn on a leased egress. A failure of the egress itself before
// the generation request was sent is retried once on a fresh lease (the failed
// node is cooling by then); a sent request is never repeated. submitted, when
// set, runs once right before the generation request.
func (a *Adapter) attempt(ctx context.Context, credential account.Credential, submitted func(), fn func(context.Context) error) (bool, error) {
	for try := 1; ; try++ {
		callCtx, lease, err := a.lease(ctx, credential)
		if err != nil {
			return false, err
		}
		var sent atomic.Bool
		callCtx = context.WithValue(callCtx, submitKey{}, func() {
			if !sent.Swap(true) && submitted != nil {
				submitted()
			}
		})
		err = fn(callCtx)
		egressFailed := a.report(ctx, lease, err)
		lease.Release()
		if try == 1 && egressFailed && !sent.Load() && ctx.Err() == nil {
			continue
		}
		return sent.Load(), err
	}
}
