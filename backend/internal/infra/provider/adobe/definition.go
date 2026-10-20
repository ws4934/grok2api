package adobe

import (
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// Definition 声明 Adobe Firefly：纯媒体 Provider，图像生成/编辑与视频生成都是
// 异步任务（submit→poll→取结果），凭据用 Cookie 自动续期（IMS OAuth），usage
// 为本地估算（媒体按模型计价）。模型目录是手写表（models.go），所有账号通用，
// 启动时同步路由。
func (a *Adapter) Definition() provider.Definition {
	return provider.Definition{
		Provider: account.ProviderAdobe,
		Profile: provider.ProfileSurface{
			Name: "Adobe Firefly", ShortName: "Adobe", TierExempt: true,
			EgressScopes:         []egressdomain.Scope{scope},
			DefaultMaxConcurrent: 1,
			QuotaModes:           []string{quotaModeCredits},
			// Images and videos spend the same credits.
			MediaQuota: []provider.MediaQuotaGroup{{Kind: provider.MediaQuotaCredits, Modes: []string{quotaModeCredits}}},
		},
		ModelNamespace: account.ProviderAdobe.ModelNamespace(),
		ModelCatalog:   provider.ModelCatalogStatic,
		ModelCapabilities: []modeldomain.Capability{
			modeldomain.CapabilityImage,
			modeldomain.CapabilityImageEdit,
			modeldomain.CapabilityVideo,
		},
		Quota: provider.QuotaRemoteWindow,
		Credential: provider.CredentialSurface{
			AuthType: account.AuthTypeOAuth, Import: true, Refresh: true,
		},
		Media: provider.MediaSurface{
			ImageGeneration: true, ImageEdit: true, VideoGeneration: true,
			ImageGenerationModelPriced: true, VideoLastFrame: true, VideoReferenceVideos: true,
		},
		Inference: provider.InferencePolicy{Usage: provider.UsageEstimated},
	}
}

// QuotaMode：Adobe 的所有图像/视频模型都消费账号积分窗口。
func (a *Adapter) QuotaMode(upstreamModel string) string { return quotaModeCredits }

func (a *Adapter) TierOrder(string) []account.WebTier { return nil }

// StaticModel 声明每个账号都服务 credits 模式下的全部模型，路由因此跳过逐账号
// 的模型能力快照。
func (a *Adapter) StaticModel(quotaMode string) bool { return quotaMode == quotaModeCredits }
