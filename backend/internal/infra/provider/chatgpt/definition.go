package chatgpt

import (
	"slices"
	"time"

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
			QuotaModes:        []string{quotaModeReason, quotaModeImage, quotaModeUpload, quotaModeCodex5Hour, quotaModeCodexWeekly, quotaModeCodex},
			DisplayQuotaModes: []string{quotaModeCodex5Hour, quotaModeCodexWeekly},
			MediaQuota:        []provider.MediaQuotaGroup{{Kind: provider.MediaQuotaImage, Modes: []string{quotaModeImage}}},
			// Windows carry their reset time and every request refreshes its
			// window, so idle accounts need only an occasional sync.
			QuotaStaleAfter: 6 * time.Hour,
		},
		ModelNamespace: account.ProviderChatGPT.ModelNamespace(),
		ModelCatalog:   provider.ModelCatalogRemote,
		ModelCapabilities: []modeldomain.Capability{
			modeldomain.CapabilityChat,
			modeldomain.CapabilityResponses,
			modeldomain.CapabilityImage,
			modeldomain.CapabilityImageEdit,
			modeldomain.CapabilityTTS,
			modeldomain.CapabilitySTT,
		},
		Quota: provider.QuotaRemoteWindow,
		Credential: provider.CredentialSurface{
			AuthType: account.AuthTypeOAuth, Import: true, Refresh: true, RedirectOAuth: true,
		},
		Conversation: provider.ConversationSurface{Responses: true, ChatCompletions: true, Messages: true},
		Media:        provider.MediaSurface{ImageGeneration: true, ImageEdit: true, TTS: true, STT: true, SingleImagePerRequest: true},
		// SessionIdentity keeps a client session on one account, whose ChatGPT
		// conversation a follow-up request then continues.
		Inference: provider.InferencePolicy{Usage: provider.UsageEstimated, RetryForbiddenAsEgress: true, SessionIdentity: true},
	}
}

// Routes returns the image and voice routes. Chat routes come from account model sync.
func Routes() []modeldomain.Route {
	values := []modeldomain.Route{}
	add := func(id string, capability modeldomain.Capability) {
		publicID, _ := modeldomain.NormalizePublicID(account.ProviderChatGPT, id)
		values = append(values, modeldomain.Route{PublicID: publicID, Provider: account.ProviderChatGPT, UpstreamModel: id, Capability: capability, Origin: modeldomain.OriginCatalog, Enabled: true})
	}
	for _, item := range imageModels {
		add(item.id, modeldomain.CapabilityImage)
		add(item.id, modeldomain.CapabilityImageEdit)
	}
	for _, id := range codexImageModels {
		add(id, modeldomain.CapabilityImage)
		add(id, modeldomain.CapabilityImageEdit)
	}
	add(TTSModel, modeldomain.CapabilityTTS)
	add(STTModel, modeldomain.CapabilitySTT)
	return values
}

// QuotaMode follows the Web limits: every chat consumes "reason", every image
// "image_gen"; dictation and speech, answered by speechModel, use neither.
// Codex images draw on the Codex usage windows instead.
func (a *Adapter) QuotaMode(upstreamModel string) string {
	if upstreamModel == STTModel || upstreamModel == TTSModel {
		return ""
	}
	if _, ok := codexImageEngine(upstreamModel); ok {
		return quotaModeCodex
	}
	if len(a.imageCandidates(upstreamModel)) > 0 {
		return quotaModeImage
	}
	return quotaModeReason
}

func (a *Adapter) TierOrder(string) []account.WebTier { return nil }

// NormalizeAccountModelCapabilities makes the voice routes schedulable on
// every account, each image route on accounts whose catalog has one of
// its conversation models with the image tool enabled
// (image_gen_tool_enabled in /backend-api/models), and the Codex image routes
// on paid plans.
func (a *Adapter) NormalizeAccountModelCapabilities(models []string, _ *account.Billing, credential account.Credential) []string {
	capable := a.imageCapableFromCache(credential.ID, models)
	result := append(slices.Clip(models), TTSModel, STTModel)
	for _, item := range imageModels {
		if pickImageModel(a.imageCandidates(item.id), capable) != "" {
			result = append(result, item.id)
		}
	}
	plan := credential.Plan
	if a.cipher != nil && credential.EncryptedAccessToken != "" {
		if c, err := a.decrypt(credential); err == nil {
			plan = planOf(credential, c)
		}
	}
	if codexPlan(plan) {
		result = append(result, codexImageModels...)
	}
	return result
}

// PricingModel prices a Codex image route as its Images API model.
func (a *Adapter) PricingModel(upstreamModel string) string {
	if engine, ok := codexImageEngine(upstreamModel); ok {
		return engine
	}
	return upstreamModel
}
