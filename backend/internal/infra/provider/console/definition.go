package console

import (
	"strings"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// Definition declares the stable Grok Console capability boundary. Conversation
// remains stateless while media uses the standard asynchronous xAI resources.
func (a *Adapter) Definition() provider.Definition {
	return provider.Definition{
		Provider:       account.ProviderConsole,
		Profile:        provider.ProfileSurface{Name: "Grok Console", OfficialPricing: true, ShortName: "Console", TierExempt: true, EgressScopes: []egressdomain.Scope{egressdomain.ScopeConsole, egressdomain.ScopeConsoleAsset}},
		ModelNamespace: account.ProviderConsole.ModelNamespace(),
		ModelCatalog:   provider.ModelCatalogStatic,
		ModelCapabilities: []modeldomain.Capability{
			modeldomain.CapabilityResponses,
			modeldomain.CapabilityImage,
			modeldomain.CapabilityImageEdit,
			modeldomain.CapabilityVideo,
			modeldomain.CapabilityTTS,
			modeldomain.CapabilitySTT,
			modeldomain.CapabilityRealtime,
		},
		Quota: provider.QuotaRemoteWindow,
		Credential: provider.CredentialSurface{
			AuthType: account.AuthTypeSSO, Import: true,
		},
		Conversation: provider.ConversationSurface{
			Responses: true, ChatCompletions: true, Messages: true, StatelessPreviousResponse: true,
		},
		Media: provider.MediaSurface{
			ImageGeneration: true, ImageEdit: true, VideoGeneration: true,
			TTS: true, STT: true, Realtime: true,
		},
		// Console shares the browser/clearance surface with Web. A 403 is
		// therefore normally an egress challenge, not proof that the SSO
		// credential is invalid; the gateway must retry after rebuilding the
		// browser session instead of cooling the account.
		Inference: provider.InferencePolicy{Usage: provider.UsageUpstream, RetryForbiddenAsEgress: true, QualityHold: true},
		// A 429 with quota left is transient on Console; the DPoP rollout 403
		// cannot be fixed by another account or egress.
		Failure: provider.FailurePolicy{TerminalAuthSchemeForbidden: true, SoftRateLimit: true},
	}
}

// StaticModel: every Console account serves the static catalog, and every
// catalog model resolves to a quota mode.
func (a *Adapter) StaticModel(quotaMode string) bool {
	return strings.TrimSpace(quotaMode) != ""
}
