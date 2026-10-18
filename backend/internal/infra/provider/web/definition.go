package web

import (
	"fmt"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// Definition 集中声明 Grok Web 的稳定能力边界；Web 私有协议由本包内部维护。
func (a *Adapter) Definition() provider.Definition {
	return provider.Definition{
		Provider:       account.ProviderWeb,
		Profile:        provider.ProfileSurface{Name: "Grok Web", OfficialPricing: true, ShortName: "Web", EgressScopes: []egressdomain.Scope{egressdomain.ScopeWeb, egressdomain.ScopeWebAsset}},
		ModelNamespace: account.ProviderWeb.ModelNamespace(),
		ModelCatalog:   provider.ModelCatalogStatic,
		ModelCapabilities: []modeldomain.Capability{
			modeldomain.CapabilityChat,
			modeldomain.CapabilityImage,
			modeldomain.CapabilityImageEdit,
			modeldomain.CapabilityVideo,
		},
		Quota: provider.QuotaRemoteWindow,
		Credential: provider.CredentialSurface{
			AuthType: account.AuthTypeSSO, Import: true,
		},
		Conversation: provider.ConversationSurface{
			Responses: true, ChatCompletions: true, Messages: true, StoredResponses: true,
		},
		Media: provider.MediaSurface{
			ImageGeneration: true, ImageEdit: true, VideoGeneration: true, ImageGenerationModelPriced: true,
		},
		Inference: provider.InferencePolicy{Usage: provider.UsageEstimated, RetryForbiddenAsEgress: true},
	}
}

// AccountTier maps the Web subscription tier to client-key tiers.
func (a *Adapter) AccountTier(candidate account.RoutingCandidate) clientkey.AccountTier {
	switch candidate.Credential.WebTier {
	case account.WebTierBasic:
		return clientkey.AccountTierFree
	case account.WebTierSuper, account.WebTierHeavy:
		return clientkey.AccountTierSuper
	default:
		return clientkey.AccountTierUnknown
	}
}

// CandidateQuotaMode spends image edits of Basic accounts from the Imagine Pro window.
func (a *Adapter) CandidateQuotaMode(candidate account.RoutingCandidate, quotaMode string) string {
	if quotaMode != account.QuotaModeWebImageEdit {
		return quotaMode
	}
	switch candidate.Credential.WebTier {
	case account.WebTierSuper, account.WebTierHeavy:
		return account.QuotaModeWebImageEdit
	default:
		return account.QuotaModeWebImagePro
	}
}

// ValidateVideoRequest: the Web protocol is captured for text-to-video only;
// the removed media-post path must not come back for image or reference input.
func (a *Adapter) ValidateVideoRequest(options provider.VideoRequestOptions) error {
	if options.Operation != provider.VideoOperationGenerate {
		return provider.ErrVideoOperationUnsupported
	}
	if options.HasImage || options.ReferenceImages > 0 {
		return fmt.Errorf("%w: Grok Web 当前仅支持文本生视频；图片视频请使用 Build 或 Console Provider", provider.ErrVideoOperationUnsupported)
	}
	return provider.ValidateGrokVideoResolution(options)
}

// VideoQuotaMode spends default and 720p videos from the dedicated 720p product.
func (a *Adapter) VideoQuotaMode(quotaMode, resolution string) string {
	if quotaMode == account.QuotaModeWebVideo {
		resolution = strings.ToLower(strings.TrimSpace(resolution))
		if resolution == "" || resolution == "720p" {
			return account.QuotaModeWebVideo720p
		}
	}
	return quotaMode
}

// StaticModel: every Web account serves the Imagine products its tier allows.
func (a *Adapter) StaticModel(quotaMode string) bool {
	return account.IsWebImagineQuotaMode(quotaMode)
}
