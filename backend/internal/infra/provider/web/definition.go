package web

import (
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

// StaticModel: every Web account serves the Imagine products its tier allows.
func (a *Adapter) StaticModel(quotaMode string) bool {
	return account.IsWebImagineQuotaMode(quotaMode)
}
