package provider

import (
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
)

// Registry stores enabled Provider Adapters and does not create placeholders for unsupported sources.
type Registry struct {
	adapters    map[account.Provider]Adapter
	definitions map[account.Provider]Definition
	aliases     map[string]ModelAlias
	issues      []error
}

func NewRegistry(adapters ...Adapter) *Registry {
	registry := &Registry{
		adapters:    make(map[account.Provider]Adapter, len(adapters)),
		definitions: make(map[account.Provider]Definition, len(adapters)),
		aliases:     make(map[string]ModelAlias),
	}
	for _, adapter := range adapters {
		if adapter == nil {
			registry.issues = append(registry.issues, errors.New("Provider Adapter 不能为空"))
			continue
		}
		providerValue := adapter.Provider()
		if !providerValue.IsValid() {
			registry.issues = append(registry.issues, fmt.Errorf("Provider Adapter 身份 %q 无效", providerValue))
			continue
		}
		if _, exists := registry.adapters[providerValue]; exists {
			registry.issues = append(registry.issues, fmt.Errorf("Provider %s 重复注册", providerValue))
			continue
		}
		registry.adapters[providerValue] = adapter
		if source, ok := adapter.(DefinitionAdapter); ok {
			registry.definitions[providerValue] = source.Definition().Clone()
		}
		if source, ok := adapter.(ModelAliasAdapter); ok {
			for _, value := range source.ModelAliases() {
				if value.Alias == "" || value.PublicModel == "" {
					continue
				}
				if value.Provider != providerValue {
					registry.issues = append(registry.issues, fmt.Errorf("Provider %s 的模型别名 %q 指向了 %s", providerValue, value.Alias, value.Provider))
					continue
				}
				if !modeldomain.IsCanonicalPublicID(value.Provider, value.PublicModel) {
					registry.issues = append(registry.issues, fmt.Errorf("Provider %s 的模型别名 %q 目标 %q 不是规范内部路由 ID", providerValue, value.Alias, value.PublicModel))
					continue
				}
				if existing, exists := registry.aliases[value.Alias]; exists {
					if existing != value {
						registry.issues = append(registry.issues, fmt.Errorf("模型别名 %q 重复注册", value.Alias))
					}
					continue
				}
				registry.aliases[value.Alias] = value
			}
		}
	}
	return registry
}

// Get returns a registered Provider Adapter.
func (r *Registry) Get(value account.Provider) (Adapter, bool) {
	adapter, ok := r.adapters[value]
	return adapter, ok
}

// ResolveModelAlias returns the canonical internal route for a hidden compatibility model name.
func (r *Registry) ResolveModelAlias(value string) (ModelAlias, bool) {
	result, ok := r.aliases[value]
	return result, ok
}

// TierExempt reports whether accounts of the provider ignore client-key tier scopes.
func (r *Registry) TierExempt(value account.Provider) bool {
	if r == nil {
		return false
	}
	definition, ok := r.definitions[value]
	return ok && definition.Profile.TierExempt
}

// Definition returns the stable capability declaration from a production Adapter.
func (r *Registry) Definition(value account.Provider) (Definition, bool) {
	definition, ok := r.definitions[value]
	return definition.Clone(), ok
}

// Providers returns the built-in display order followed by sorted extension IDs.
func (r *Registry) Providers() []account.Provider {
	values := make([]account.Provider, 0, len(r.adapters))
	for _, value := range account.Providers() {
		if _, ok := r.adapters[value]; ok {
			values = append(values, value)
		}
	}
	var extensions []account.Provider
	for value := range r.adapters {
		if !slices.Contains(account.Providers(), value) {
			extensions = append(extensions, value)
		}
	}
	sort.Slice(extensions, func(i, j int) bool { return extensions[i] < extensions[j] })
	return append(values, extensions...)
}

// Validate checks that production registry definitions match their implemented capability interfaces.
func (r *Registry) Validate() error {
	if r == nil {
		return errors.New("Provider Registry 不能为空")
	}
	if len(r.issues) > 0 {
		return errors.Join(r.issues...)
	}
	for _, value := range r.Providers() {
		adapter, registered := r.adapters[value]
		definition, described := r.definitions[value]
		if !registered || !described {
			return fmt.Errorf("Provider %s 未完整注册 Adapter 与 Definition", value)
		}
		if definition.Provider != value {
			return fmt.Errorf("Provider %s 的 Definition 身份不一致", value)
		}
		if err := definition.Validate(); err != nil {
			return err
		}
		if definition.Conversation.Responses || definition.Conversation.ChatCompletions || definition.Conversation.Messages {
			if _, ok := adapter.(ResponseAdapter); !ok {
				return fmt.Errorf("Provider %s 声明对话能力但未实现适配器", value)
			}
		}
		if _, ok := adapter.(ModelCatalogAdapter); !ok {
			return fmt.Errorf("Provider %s 未实现模型目录适配器", value)
		}
		switch definition.Quota {
		case QuotaBilling:
			if _, ok := adapter.(BillingAdapter); !ok {
				return fmt.Errorf("Provider %s 声明 Billing 额度但未实现适配器", value)
			}
		case QuotaRemoteWindow, QuotaLocalWindow:
			if _, ok := adapter.(QuotaAdapter); !ok {
				return fmt.Errorf("Provider %s 声明窗口额度但未实现适配器", value)
			}
		}
		if definition.Credential.Import {
			if _, ok := adapter.(CredentialCodecAdapter); !ok {
				return fmt.Errorf("Provider %s 声明凭据导入但未实现适配器", value)
			}
		}
		if definition.Credential.Refresh {
			if _, ok := adapter.(CredentialRefreshAdapter); !ok {
				return fmt.Errorf("Provider %s 声明凭据刷新但未实现适配器", value)
			}
		}
		if definition.Credential.DeviceOAuth || definition.Credential.RedirectOAuth {
			if _, ok := adapter.(InteractiveAuthAdapter); !ok {
				return fmt.Errorf("Provider %s 声明交互授权但未实现交互授权接口", value)
			}
		}
		if definition.Media.ImageGeneration {
			if _, ok := adapter.(ImageGenerationAdapter); !ok {
				return fmt.Errorf("Provider %s 声明图像生成能力但未实现适配器", value)
			}
		}
		if definition.Media.ImageEdit {
			if _, ok := adapter.(ImageEditAdapter); !ok {
				return fmt.Errorf("Provider %s 声明图像编辑能力但未实现适配器", value)
			}
		}
		if definition.Media.VideoGeneration {
			if _, ok := adapter.(VideoAdapter); !ok {
				return fmt.Errorf("Provider %s 声明视频能力但未实现适配器", value)
			}
		}
		if definition.Media.TTS {
			if _, ok := adapter.(TTSAdapter); !ok {
				return fmt.Errorf("Provider %s 声明语音合成能力但未实现适配器", value)
			}
		}
		if definition.Media.STT {
			if _, ok := adapter.(STTAdapter); !ok {
				return fmt.Errorf("Provider %s 声明语音识别能力但未实现适配器", value)
			}
		}
		if definition.Media.Realtime {
			if _, ok := adapter.(VoiceWebSocketAdapter); !ok {
				return fmt.Errorf("Provider %s 声明实时语音能力但未实现 WebSocket 适配器", value)
			}
		}
	}
	return nil
}

func (r *Registry) SupportsStoredResponses(value account.Provider) bool {
	definition, ok := r.Definition(value)
	return ok && definition.Conversation.StoredResponses
}

func (r *Registry) SupportsConversation(value account.Provider, operation string) bool {
	definition, ok := r.Definition(value)
	return ok && definition.Conversation.Supports(operation)
}

func (r *Registry) SupportsResponseCompaction(value account.Provider) bool {
	definition, ok := r.Definition(value)
	return ok && definition.Conversation.Compact
}

func (r *Registry) SupportsCredentialRefresh(value account.Provider) bool {
	definition, ok := r.Definition(value)
	return ok && definition.Credential.Refresh
}

func (r *Registry) QuotaKind(value account.Provider) (QuotaKind, bool) {
	definition, ok := r.Definition(value)
	if !ok {
		return "", false
	}
	return definition.Quota, true
}

func (r *Registry) UsageKind(value account.Provider) (UsageKind, bool) {
	definition, ok := r.Definition(value)
	if !ok {
		return "", false
	}
	return definition.Inference.Usage, true
}

func (r *Registry) RetryForbiddenAsEgress(value account.Provider) bool {
	definition, ok := r.Definition(value)
	return ok && definition.Inference.RetryForbiddenAsEgress
}

func (r *Registry) SupportsStatelessPreviousResponse(value account.Provider) bool {
	definition, ok := r.Definition(value)
	return ok && definition.Conversation.StatelessPreviousResponse
}

// Inference returns the provider's gateway runtime policy; unknown providers get the zero policy.
func (r *Registry) Inference(value account.Provider) InferencePolicy {
	if r == nil {
		return InferencePolicy{}
	}
	return r.definitions[value].Inference
}

// Failure returns the provider's failure policy; unknown providers get the generic zero policy.
func (r *Registry) Failure(value account.Provider) FailurePolicy {
	if r == nil {
		return FailurePolicy{}
	}
	return r.definitions[value].Failure
}

func (r *Registry) Responses(value account.Provider) (ResponseAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(ResponseAdapter)
	return result, ok
}

func (r *Registry) Models(value account.Provider) (ModelCatalogAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(ModelCatalogAdapter)
	return result, ok
}

func (r *Registry) Billing(value account.Provider) (BillingAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(BillingAdapter)
	return result, ok
}

// adapterAs returns the provider's adapter when it implements T.
func adapterAs[T any](r *Registry, value account.Provider) (T, bool) {
	var zero T
	if r == nil {
		return zero, false
	}
	result, ok := r.adapters[value].(T)
	return result, ok
}

// StaticCatalog reports whether the provider's models come from its built-in catalog.
func (r *Registry) StaticCatalog(value account.Provider) bool {
	return r != nil && r.definitions[value].ModelCatalog == ModelCatalogStatic
}
