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
		Failure: provider.FailurePolicy{TerminalAuthSchemeForbidden: true, SoftRateLimit: true, PaymentRequiredQuota: true},
	}
}

// NormalizeVideoRequest applies the xAI video defaults and ranges and fits
// the request to the Console upstream instead of letting it fail the job:
// a first frame or up to 7 references (measured: 8 answer 400 "Maximum
// allowed is 7", on both video models), and grok-imagine-video's
// reference-to-video stops at 10 s (measured: 15 s answers 400; image-to-video
// and grok-imagine-video-1.5 keep 15 s).
func (a *Adapter) NormalizeVideoRequest(options provider.VideoRequestOptions) (provider.VideoRequestOptions, error) {
	if options.Operation == provider.VideoOperationGenerate {
		options = provider.FitGrokVideoInputs(options, provider.ConsoleVideoMaxReferenceImages)
	}
	options, err := provider.NormalizeGrokVideoRequest(options)
	if strings.TrimSpace(options.UpstreamModel) == "grok-imagine-video" && options.ReferenceImages > 0 {
		options.Duration = min(options.Duration, provider.ConsoleVideoMaxReferenceDurationSeconds)
	}
	return options, err
}

// ValidateVideoRequest: edit and extension exist for grok-imagine-video only.
func (a *Adapter) ValidateVideoRequest(options provider.VideoRequestOptions) error {
	if options.Operation != provider.VideoOperationGenerate && strings.TrimSpace(options.UpstreamModel) != "grok-imagine-video" {
		return provider.ErrVideoOperationUnsupported
	}
	return nil
}

// StaticModel: every Console account serves the static catalog, and every
// catalog model resolves to a quota mode.
func (a *Adapter) StaticModel(quotaMode string) bool {
	return strings.TrimSpace(quotaMode) != ""
}
