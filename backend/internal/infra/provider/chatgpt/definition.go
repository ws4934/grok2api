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

// DefaultImageModel is the Responses image tool fallback when none is configured.
const DefaultImageModel = "gpt-image-2.5"

// Definition declares ChatGPT Web: Responses is the canonical conversation
// protocol, usage is estimated locally, images run synchronously or as
// resumable jobs (AsyncImageAdapter), and a 403 is
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
	values := []modeldomain.Route{}
	for _, item := range imageModels {
		publicID, _ := modeldomain.NormalizePublicID(account.ProviderChatGPT, item.id)
		for _, capability := range []modeldomain.Capability{modeldomain.CapabilityImage, modeldomain.CapabilityImageEdit} {
			values = append(values, modeldomain.Route{PublicID: publicID, Provider: account.ProviderChatGPT, UpstreamModel: item.id, Capability: capability, Origin: modeldomain.OriginCatalog, Enabled: true})
		}
	}
	return values
}

// QuotaMode follows the Web limits: every chat consumes "reason", every image "image_gen".
func (a *Adapter) QuotaMode(upstreamModel string) string {
	if len(a.imageCandidates(upstreamModel)) > 0 {
		return quotaModeImage
	}
	return quotaModeReason
}

func (a *Adapter) TierOrder(string) []account.WebTier { return nil }

// NormalizeAccountModelCapabilities makes each image route schedulable on
// accounts whose catalog has one of its conversation models with the image
// tool enabled (image_gen_tool_enabled in /backend-api/models).
func (a *Adapter) NormalizeAccountModelCapabilities(models []string, _ *account.Billing, credential account.Credential) []string {
	capable := a.imageCapableFromCache(credential.ID, models)
	result := models
	for _, item := range imageModels {
		if pickImageModel(a.imageCandidates(item.id), capable) != "" {
			result = append(result, item.id)
		}
	}
	return result
}
