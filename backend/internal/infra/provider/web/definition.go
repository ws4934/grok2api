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
		Provider: account.ProviderWeb,
		Profile: provider.ProfileSurface{Name: "Grok Web", OfficialPricing: true, ShortName: "Web", EgressScopes: []egressdomain.Scope{egressdomain.ScopeWeb, egressdomain.ScopeWebAsset},
			// 480p and 720p videos spend separate Imagine products; together they are the video quota.
			MediaQuota: []provider.MediaQuotaGroup{
				{Kind: provider.MediaQuotaImage, Modes: []string{account.QuotaModeWebImagePro}},
				{Kind: provider.MediaQuotaVideo, Modes: []string{account.QuotaModeWebVideo, account.QuotaModeWebVideo720p}},
			}},
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
			ImageGeneration: true, ImageEdit: true, VideoGeneration: true, ImageGenerationModelPriced: true, ImageEditModelPriced: true,
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

// NormalizeVideoRequest applies the xAI video defaults and ranges.
func (a *Adapter) NormalizeVideoRequest(options provider.VideoRequestOptions) (provider.VideoRequestOptions, error) {
	options, err := provider.NormalizeGrokVideoRequest(options)
	if err != nil {
		return options, err
	}
	options.ReferenceImages = min(options.ReferenceImages, maxWebVideoReferences)
	if options.ReferenceImages > 0 {
		options.Duration = min(options.Duration, 10)
	}
	return options, nil
}

// ValidateVideoRequest matches the captured Imagine video input modes. Uploads
// reference fileMetadataId directly; the retired media-post flow is not used.
func (a *Adapter) ValidateVideoRequest(options provider.VideoRequestOptions) error {
	if options.Operation != provider.VideoOperationGenerate {
		return provider.ErrVideoOperationUnsupported
	}
	if options.ReferenceImages > maxWebVideoReferences {
		return fmt.Errorf("%w: 最多支持 %d 张参考图", provider.ErrVideoParameterInvalid, maxWebVideoReferences)
	}
	unsupported := ""
	switch {
	case options.HasLastFrame:
		unsupported = "last_frame"
	case options.Keyframes > 0:
		unsupported = "keyframes"
	case options.GenerateAudio != nil:
		unsupported = "generate_audio"
	case options.ReferenceVideos > 0:
		unsupported = "reference_videos"
	}
	if unsupported != "" {
		return fmt.Errorf("%w: 当前模型不支持 %s", provider.ErrVideoParameterInvalid, unsupported)
	}
	return nil
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
