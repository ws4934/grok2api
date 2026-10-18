package chatgpt

import (
	"slices"

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

// imageModels lists the image routes and the ChatGPT Web conversation model
// that draws each one (same mapping as chatgpt2api). ChatGPT Web picks the
// image engine itself, so the 2.5 names are compatibility aliases on "auto";
// they cannot force Flare or Sunburst. ImageConversationModel overrides all.
var imageModels = []struct{ id, conversation string }{
	{"gpt-image-2", "gpt-5-3"},
	{"gpt-image-2.5", "auto"},
	{"gpt-image-2.5-flare", "auto"},
	{"gpt-image-2.5-sunburst", "auto"},
}

// DefaultImageModel is the Responses image tool fallback when none is configured.
const DefaultImageModel = "gpt-image-2.5"

// imageConversationModel returns the Web conversation model for an image
// route, or "" when upstreamModel is not an image route.
func (a *Adapter) imageConversationModel(upstreamModel string) string {
	for _, item := range imageModels {
		if item.id == upstreamModel {
			if configured := a.config().ImageConversationModel; configured != "" {
				return configured
			}
			return item.conversation
		}
	}
	return ""
}

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
	if a.imageConversationModel(upstreamModel) != "" {
		return quotaModeImage
	}
	return quotaModeReason
}

func (a *Adapter) TierOrder(string) []account.WebTier { return nil }

// NormalizeAccountModelCapabilities makes each image route schedulable on
// accounts whose catalog contains the conversation model that draws it.
func (a *Adapter) NormalizeAccountModelCapabilities(models []string, _ *account.Billing, _ account.Credential) []string {
	result := models
	for _, item := range imageModels {
		if slices.Contains(models, a.imageConversationModel(item.id)) {
			result = append(result, item.id)
		}
	}
	return result
}
