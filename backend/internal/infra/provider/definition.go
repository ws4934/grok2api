package provider

import (
	"fmt"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
)

// ModelCatalogKind 表示模型目录来自真实上游发现还是项目内置目录。
type ModelCatalogKind string

const (
	ModelCatalogRemote ModelCatalogKind = "remote"
	ModelCatalogStatic ModelCatalogKind = "static"
)

// QuotaKind 表示号池可用额度的权威来源。
type QuotaKind string

const (
	QuotaBilling      QuotaKind = "billing"
	QuotaRemoteWindow QuotaKind = "remote_window"
	QuotaLocalWindow  QuotaKind = "local_window"
)

// ConversationSurface 描述统一对话 API 在当前 Provider 上的真实能力边界。
type ConversationSurface struct {
	Responses       bool
	ChatCompletions bool
	Messages        bool
	Compact         bool
	StoredResponses bool
	// StatelessPreviousResponse lets a provider without stored Responses accept
	// previous_response_id: the gateway drops the ID and forwards the history
	// the client replays.
	StatelessPreviousResponse bool
	// CustomTools: the upstream runs Responses custom (freeform) tools and
	// parallel tool calls, so Codex clients may use their agent tools.
	CustomTools bool
}

// Supports 判断统一对话协议入口是否由当前 Provider 明确声明。
func (s ConversationSurface) Supports(operation string) bool {
	switch operation {
	case "responses":
		return s.Responses
	case "compaction":
		return s.Compact
	case "chat":
		return s.ChatCompletions
	case "messages":
		return s.Messages
	default:
		return false
	}
}

// MediaSurface 描述图像与视频 API 在当前 Provider 上的真实能力边界。
type MediaSurface struct {
	ImageGeneration bool
	ImageEdit       bool
	VideoGeneration bool
	TTS             bool
	STT             bool
	Realtime        bool
	// ImageGenerationModelPriced: image generation picks the product by model
	// and forwards no resolution or quality, so neither selects a price tier.
	ImageGenerationModelPriced bool
}

// CredentialSurface 描述号池凭据的接入和维护方式。
type CredentialSurface struct {
	AuthType    account.AuthType
	Import      bool
	Refresh     bool
	DeviceOAuth bool
	// RedirectOAuth providers log in through a browser redirect whose callback
	// URL the administrator pastes back (OAuth authorization code + PKCE).
	RedirectOAuth bool
}

// UsageKind 表示文本推理 usage 的权威性；媒体请求仍由独立计费路径记录。
type UsageKind string

const (
	UsageUpstream  UsageKind = "upstream"
	UsageEstimated UsageKind = "estimated"
)

// InferencePolicy 描述与上游协议相关、但必须由统一网关执行的运行策略。
type InferencePolicy struct {
	Usage                  UsageKind
	RetryForbiddenAsEgress bool
	// SessionIdentity derives a stable upstream session ID (prompt_cache_key),
	// account affinity and reasoning replay key from client session signals.
	SessionIdentity bool
	// QualityHold lets the quality retry hold reasoning streams for inspection;
	// QualityAutoDisable also lets it disable accounts below the output-speed floor.
	QualityHold        bool
	QualityAutoDisable bool
}

// FailurePolicy declares provider-specific reactions to upstream failures. The
// zero value is the generic path: a 403 may be a credential or egress problem,
// the upstream's X-Should-Retry veto is honoured, and only content-safety 403s
// are returned to the client unchanged.
type FailurePolicy struct {
	// AuthenticatedForbidden: a 403 comes from an authenticated session. It never
	// triggers a credential refresh, an unclassified 403 still cools the account,
	// the admin forbidden-reauth codes apply, and an explicit chat-endpoint
	// denial blocks only the model on that account.
	AuthenticatedForbidden bool
	// ForceAccountFailover: 402, 403, 429 and a failed encrypted-reasoning
	// recovery are bound to the account and rotate it even when the upstream
	// sends X-Should-Retry: false.
	ForceAccountFailover bool
	// TerminalRequestScopedForbidden returns every request-scoped 403 to the
	// client unchanged; TerminalAuthSchemeForbidden only those demanding a new
	// auth scheme (DPoP), which no other account or egress can satisfy.
	TerminalRequestScopedForbidden bool
	TerminalAuthSchemeForbidden    bool
	// FatalHeaderTimeout and FatalOriginTLS (Cloudflare 525/526) end failover,
	// as the official client does.
	FatalHeaderTimeout bool
	FatalOriginTLS     bool
	// SoftRateLimit: a 429 that quota reconciliation does not explain isolates
	// the account for Retry-After without growing its failure count.
	SoftRateLimit bool
}

// ProfileSurface presents the provider and declares traits the shared
// gateway, egress and admin surfaces must honour.
type ProfileSurface struct {
	Name      string
	ShortName string
	// TierExempt providers ignore client-key tier restrictions: their accounts
	// have no reliable free/paid classification.
	TierExempt bool
	// EgressScopes lists the egress scopes the provider uses. The first one is
	// the primary scope and equals the provider ID.
	EgressScopes []egressdomain.Scope
	// DefaultMaxConcurrent is the per-account concurrency for new accounts; 0 keeps the shared default.
	DefaultMaxConcurrent int
	// QuotaModes lists the quota window modes shown for the provider's accounts.
	QuotaModes []string
	// OfficialPricing bills the provider with the official xAI price list
	// (domain/audit/pricing.go) when no custom price exists.
	OfficialPricing bool
}

// Definition 是 Provider 注册表的权威静态描述；渠道专属协议仍由各 Adapter 实现。
type Definition struct {
	Provider          account.Provider
	Profile           ProfileSurface
	ModelNamespace    string
	ModelCatalog      ModelCatalogKind
	ModelCapabilities []modeldomain.Capability
	Quota             QuotaKind
	Credential        CredentialSurface
	Conversation      ConversationSurface
	Media             MediaSurface
	Inference         InferencePolicy
	Failure           FailurePolicy
}

// DefinitionAdapter 由生产 Provider 实现，用于注册明确且可校验的能力边界。
type DefinitionAdapter interface {
	Adapter
	Definition() Definition
}

// Clone 返回可安全交给调用方读取的副本，避免切片字段被外部修改后污染注册表。
func (d Definition) Clone() Definition {
	d.ModelCapabilities = append([]modeldomain.Capability(nil), d.ModelCapabilities...)
	d.Profile.EgressScopes = append([]egressdomain.Scope(nil), d.Profile.EgressScopes...)
	d.Profile.QuotaModes = append([]string(nil), d.Profile.QuotaModes...)
	return d
}

// DisplayName falls back to the provider ID when no profile name is declared.
func (d Definition) DisplayName() string {
	if d.Profile.Name != "" {
		return d.Profile.Name
	}
	return string(d.Provider)
}

// ShortName falls back to the display name.
func (d Definition) ShortName() string {
	if d.Profile.ShortName != "" {
		return d.Profile.ShortName
	}
	return d.DisplayName()
}

// EgressScopes returns the declared scopes, defaulting to the primary scope.
func (d Definition) EgressScopes() []egressdomain.Scope {
	if len(d.Profile.EgressScopes) == 0 {
		return []egressdomain.Scope{egressdomain.Scope(d.Provider)}
	}
	return append([]egressdomain.Scope(nil), d.Profile.EgressScopes...)
}

// SupportsModelCapability 判断该渠道能否注册指定类型的模型路由。
func (d Definition) SupportsModelCapability(capability modeldomain.Capability) bool {
	for _, supported := range d.ModelCapabilities {
		if supported == capability {
			return true
		}
	}
	return false
}

// Validate 检查静态描述内部是否自洽，避免接口实现和后台路由配置悄悄漂移。
func (d Definition) Validate() error {
	if !d.Provider.IsValid() {
		return fmt.Errorf("Provider %q 无效", d.Provider)
	}
	if strings.TrimSpace(d.ModelNamespace) == "" || d.ModelNamespace != d.Provider.ModelNamespace() {
		return fmt.Errorf("Provider %s 的模型命名空间无效", d.Provider)
	}
	if d.ModelCatalog != ModelCatalogRemote && d.ModelCatalog != ModelCatalogStatic {
		return fmt.Errorf("Provider %s 的模型目录类型无效", d.Provider)
	}
	if len(d.ModelCapabilities) == 0 {
		return fmt.Errorf("Provider %s 未声明模型能力", d.Provider)
	}
	capabilities := make(map[modeldomain.Capability]struct{}, len(d.ModelCapabilities))
	for _, capability := range d.ModelCapabilities {
		switch capability {
		case modeldomain.CapabilityResponses, modeldomain.CapabilityChat, modeldomain.CapabilityImage, modeldomain.CapabilityImageEdit, modeldomain.CapabilityVideo, modeldomain.CapabilityTTS, modeldomain.CapabilitySTT, modeldomain.CapabilityRealtime:
		default:
			return fmt.Errorf("Provider %s 声明了无效模型能力 %q", d.Provider, capability)
		}
		if _, exists := capabilities[capability]; exists {
			return fmt.Errorf("Provider %s 重复声明模型能力 %q", d.Provider, capability)
		}
		capabilities[capability] = struct{}{}
	}
	if scopes := d.Profile.EgressScopes; len(scopes) > 0 && scopes[0] != egressdomain.Scope(d.Provider) {
		return fmt.Errorf("Provider %s 的主出口作用域必须与 Provider ID 相同", d.Provider)
	}
	if d.Quota != QuotaBilling && d.Quota != QuotaRemoteWindow && d.Quota != QuotaLocalWindow {
		return fmt.Errorf("Provider %s 的额度策略无效", d.Provider)
	}
	if d.Credential.AuthType != account.AuthTypeOAuth && d.Credential.AuthType != account.AuthTypeSSO {
		return fmt.Errorf("Provider %s 的认证类型无效", d.Provider)
	}
	if d.Inference.Usage != UsageUpstream && d.Inference.Usage != UsageEstimated {
		return fmt.Errorf("Provider %s 的 usage 策略无效", d.Provider)
	}
	if (d.Conversation.Compact || d.Conversation.StoredResponses) && !d.Conversation.Responses {
		return fmt.Errorf("Provider %s 的 Responses 扩展能力缺少 Responses 基础能力", d.Provider)
	}
	if d.Credential.AuthType != account.AuthTypeOAuth && (d.Credential.Refresh || d.Credential.DeviceOAuth || d.Credential.RedirectOAuth) {
		return fmt.Errorf("Provider %s 的非 OAuth 凭据不能声明刷新或 Device OAuth", d.Provider)
	}
	mediaCapabilities := []struct {
		enabled    bool
		capability modeldomain.Capability
		name       string
	}{
		{enabled: d.Media.ImageGeneration, capability: modeldomain.CapabilityImage, name: "图像生成"},
		{enabled: d.Media.ImageEdit, capability: modeldomain.CapabilityImageEdit, name: "图像编辑"},
		{enabled: d.Media.VideoGeneration, capability: modeldomain.CapabilityVideo, name: "视频生成"},
		{enabled: d.Media.TTS, capability: modeldomain.CapabilityTTS, name: "语音合成"},
		{enabled: d.Media.STT, capability: modeldomain.CapabilitySTT, name: "语音识别"},
		{enabled: d.Media.Realtime, capability: modeldomain.CapabilityRealtime, name: "实时语音"},
	}
	for _, item := range mediaCapabilities {
		_, declared := capabilities[item.capability]
		if item.enabled != declared {
			return fmt.Errorf("Provider %s 的%s接口与模型能力声明不一致", d.Provider, item.name)
		}
	}
	return nil
}
