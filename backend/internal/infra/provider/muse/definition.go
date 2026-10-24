package muse

import (
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// Definition declares Muse: chat, one image per request and video, all driven
// through the muse.ai web page in a headless browser. The credential is the
// login cookie jar, renewed through /api/session; usage is estimated locally.
func (a *Adapter) Definition() provider.Definition {
	return provider.Definition{
		Provider: account.ProviderMuse,
		Profile: provider.ProfileSurface{
			Name: "Muse", ShortName: "Muse", TierExempt: true,
			EgressScopes:         []egressdomain.Scope{scope},
			DefaultMaxConcurrent: 1,
			QuotaModes:           []string{quotaModeWeekly},
			// Reading the usage panel opens a browser page, so idle accounts sync rarely.
			QuotaStaleAfter: 6 * time.Hour,
		},
		ModelNamespace: account.ProviderMuse.ModelNamespace(),
		ModelCatalog:   provider.ModelCatalogStatic,
		ModelCapabilities: []modeldomain.Capability{
			modeldomain.CapabilityChat,
			modeldomain.CapabilityImage,
			modeldomain.CapabilityImageEdit,
			modeldomain.CapabilityVideo,
		},
		Quota: provider.QuotaRemoteWindow,
		Credential: provider.CredentialSurface{
			AuthType: account.AuthTypeOAuth, Import: true, Refresh: true,
		},
		// Each request is a new side chat; the client replays the history.
		Conversation: provider.ConversationSurface{Responses: true, ChatCompletions: true, Messages: true, StatelessPreviousResponse: true},
		Media: provider.MediaSurface{
			ImageGeneration: true, ImageEdit: true, VideoGeneration: true,
			ImageGenerationModelPriced: true, SingleImagePerRequest: true,
		},
		Inference: provider.InferencePolicy{Usage: provider.UsageEstimated},
	}
}

// QuotaMode: every Muse route spends the weekly usage limit.
func (a *Adapter) QuotaMode(string) string { return quotaModeWeekly }

func (a *Adapter) TierOrder(string) []account.WebTier { return nil }

// StaticModel: every account serves the whole catalog, so routing skips the
// per-account model snapshot.
func (a *Adapter) StaticModel(quotaMode string) bool { return quotaMode == quotaModeWeekly }
