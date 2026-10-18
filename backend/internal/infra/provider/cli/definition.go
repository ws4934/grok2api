package cli

import (
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// Definition 集中声明 Grok Build 的稳定能力边界；上游协议细节仍由 Adapter 维护。
func (a *Adapter) Definition() provider.Definition {
	return provider.Definition{
		Provider:          account.ProviderBuild,
		Profile:           provider.ProfileSurface{Name: "Grok Build", OfficialPricing: true, ShortName: "Build", EgressScopes: []egressdomain.Scope{egressdomain.ScopeBuild}},
		ModelNamespace:    account.ProviderBuild.ModelNamespace(),
		ModelCatalog:      provider.ModelCatalogRemote,
		ModelCapabilities: []modeldomain.Capability{modeldomain.CapabilityResponses, modeldomain.CapabilityVideo},
		Quota:             provider.QuotaBilling,
		Credential: provider.CredentialSurface{
			AuthType: account.AuthTypeOAuth, Import: true, Refresh: true, DeviceOAuth: true,
		},
		Conversation: provider.ConversationSurface{
			Responses: true, ChatCompletions: true, Messages: true, Compact: true, StoredResponses: true,
		},
		Media: provider.MediaSurface{VideoGeneration: true},
		Inference: provider.InferencePolicy{
			Usage: provider.UsageUpstream, SessionIdentity: true, QualityHold: true, QualityAutoDisable: true,
		},
		// The Build API authenticates every request: a 403 is an account or
		// request decision, never an expired token, and the official client
		// gives up on header timeouts and origin-TLS failures.
		Failure: provider.FailurePolicy{
			AuthenticatedForbidden: true, ForceAccountFailover: true, TerminalRequestScopedForbidden: true,
			FatalHeaderTimeout: true, FatalOriginTLS: true,
		},
	}
}
