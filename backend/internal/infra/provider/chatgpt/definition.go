package chatgpt

import (
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// Quota windows reported by conversation/init limits_progress.
const (
	quotaModeReason = "reason"
	quotaModeImage  = "image_gen"
	quotaModeUpload = "file_upload"
)

// imageModel is the upstream ID of the image routes. The Web conversation
// model that actually draws is the configured ImageConversationModel.
const imageModel = "gpt-image-2"

// Definition declares ChatGPT Web: Responses is the canonical conversation
// protocol, usage is estimated locally, images run synchronously, and a 403 is
// usually a Cloudflare challenge on the egress rather than an account failure.
func (a *Adapter) Definition() provider.Definition {
	return provider.Definition{
		Provider: account.ProviderChatGPT,
		Profile: provider.ProfileSurface{
			Name: "ChatGPT", ShortName: "ChatGPT", TierExempt: true,
			EgressScopes: []egressdomain.Scope{scope}, DefaultMaxConcurrent: 1,
			QuotaModes: []string{quotaModeReason, quotaModeImage, quotaModeUpload},
		},
		ModelNamespace: account.ProviderChatGPT.ModelNamespace(),
		ModelCatalog:   provider.ModelCatalogRemote,
		ModelCapabilities: []modeldomain.Capability{
			modeldomain.CapabilityChat,
			modeldomain.CapabilityResponses,
			modeldomain.CapabilityImage,
			modeldomain.CapabilityImageEdit,
		},
		Quota: provider.QuotaRemoteWindow,
		Credential: provider.CredentialSurface{
			AuthType: account.AuthTypeOAuth, Import: true, Refresh: true, RedirectOAuth: true,
		},
		Conversation: provider.ConversationSurface{Responses: true, ChatCompletions: true, Messages: true},
		Media:        provider.MediaSurface{ImageGeneration: true, ImageEdit: true},
		Inference:    provider.InferencePolicy{Usage: provider.UsageEstimated, RetryForbiddenAsEgress: true},
	}
}

// Routes returns the image routes. Chat routes come from account model sync.
func Routes() []modeldomain.Route {
	publicID, _ := modeldomain.NormalizePublicID(account.ProviderChatGPT, imageModel)
	values := []modeldomain.Route{}
	for _, capability := range []modeldomain.Capability{modeldomain.CapabilityImage, modeldomain.CapabilityImageEdit} {
		values = append(values, modeldomain.Route{PublicID: publicID, Provider: account.ProviderChatGPT, UpstreamModel: imageModel, Capability: capability, Origin: modeldomain.OriginCatalog, Enabled: true})
	}
	return values
}

// QuotaMode follows the Web limits: every chat consumes "reason", every image "image_gen".
func (a *Adapter) QuotaMode(upstreamModel string) string {
	if upstreamModel == imageModel {
		return quotaModeImage
	}
	return quotaModeReason
}

func (a *Adapter) TierOrder(string) []account.WebTier { return nil }

// NormalizeAccountModelCapabilities makes the image routes schedulable on
// accounts whose catalog contains the configured image conversation model.
func (a *Adapter) NormalizeAccountModelCapabilities(models []string, _ *account.Billing, _ account.Credential) []string {
	conversation := a.config().ImageConversationModel
	for _, value := range models {
		if conversation != "" && value == conversation {
			return append(models, imageModel)
		}
	}
	return models
}
