package console

import (
	"fmt"
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

// NormalizeVideoRequest applies the xAI video defaults and ranges.
func (a *Adapter) NormalizeVideoRequest(options provider.VideoRequestOptions) (provider.VideoRequestOptions, error) {
	return provider.NormalizeGrokVideoRequest(options)
}

// ValidateVideoRequest rejects requests the Console upstream fails only after
// the asynchronous job was queued. Edit and extension exist for
// grok-imagine-video only.
func (a *Adapter) ValidateVideoRequest(options provider.VideoRequestOptions) error {
	model := strings.TrimSpace(options.UpstreamModel)
	if options.Operation != provider.VideoOperationGenerate {
		if model == "grok-imagine-video" {
			return nil
		}
		return provider.ErrVideoOperationUnsupported
	}
	if options.HasImage && (options.ReferenceImages > 0 || options.ReferenceAudios > 0) {
		return fmt.Errorf("%w: image 不能与 reference_images/reference_audios 同时使用", provider.ErrVideoParameterInvalid)
	}
	if model == "grok-imagine-video" || model == "grok-imagine-video-1.5" {
		// 实测：8 张 reference_images 上游回 400
		// "Too many reference images: 8. Maximum allowed is 7."（两个视频模型一致）。
		if options.ReferenceImages > provider.ConsoleVideoMaxReferenceImages {
			return fmt.Errorf("%w: Console reference_images 最多 %d 张，当前为 %d 张", provider.ErrVideoParameterInvalid, provider.ConsoleVideoMaxReferenceImages, options.ReferenceImages)
		}
		// 实测：grok-imagine-video 的 reference-to-video 上游回 400
		// "Duration 15s exceeds the maximum allowed for reference-to-video, which is 10s."；
		// 走 image 字段的 image-to-video 与 grok-imagine-video-1.5 都保持 15s。
		if model == "grok-imagine-video" && options.ReferenceImages > 0 && options.Duration > provider.ConsoleVideoMaxReferenceDurationSeconds {
			return fmt.Errorf("%w: Console %s 的参考图生视频最长 %d 秒，当前为 %d 秒", provider.ErrVideoParameterInvalid, model, provider.ConsoleVideoMaxReferenceDurationSeconds, options.Duration)
		}
	}
	return provider.ValidateGrokVideoResolution(options)
}

// StaticModel: every Console account serves the static catalog, and every
// catalog model resolves to a quota mode.
func (a *Adapter) StaticModel(quotaMode string) bool {
	return strings.TrimSpace(quotaMode) != ""
}
