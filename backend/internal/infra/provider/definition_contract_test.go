package provider_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	adobeprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/adobe"
	chatgptprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/chatgpt"
	cliprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	consoleprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/console"
	museprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/muse"
	webprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/web"
)

func TestReadDiagnosticBodyAppliesHardLimit(t *testing.T) {
	body := strings.Repeat("x", provider.MaxDiagnosticBodyBytes+4096)
	data, truncated, err := provider.ReadDiagnosticBody(strings.NewReader(body))
	if err != nil || !truncated || len(data) != provider.MaxDiagnosticBodyBytes {
		t.Fatalf("diagnostic body length = %d, truncated = %v, err = %v", len(data), truncated, err)
	}
}

func TestProductionProviderDefinitionsMatchImplementedCapabilities(t *testing.T) {
	registry := provider.NewRegistry(
		cliprovider.NewAdapter(cliprovider.Config{}, nil),
		webprovider.NewAdapter(webprovider.Config{}, nil, nil, nil, nil),
		consoleprovider.NewAdapter(consoleprovider.Config{}, nil, nil, nil),
		chatgptprovider.NewAdapter(chatgptprovider.Config{}, nil, nil, nil),
		adobeprovider.NewAdapter(adobeprovider.Config{}, nil, nil, nil, nil),
		museprovider.NewAdapter(museprovider.Config{}, nil, nil, nil, nil),
	)
	if err := registry.Validate(); err != nil {
		t.Fatalf("production registry validation failed: %v", err)
	}
	if values := registry.Providers(); !reflect.DeepEqual(values, account.Providers()) {
		t.Fatalf("providers = %#v, want %#v", values, account.Providers())
	}

	tests := []struct {
		provider     account.Provider
		catalog      provider.ModelCatalogKind
		quota        provider.QuotaKind
		capabilities []modeldomain.Capability
		credential   provider.CredentialSurface
		conversation provider.ConversationSurface
		media        provider.MediaSurface
		inference    provider.InferencePolicy
		failure      provider.FailurePolicy
	}{
		{
			provider: account.ProviderBuild, catalog: provider.ModelCatalogRemote, quota: provider.QuotaBilling,
			capabilities: []modeldomain.Capability{modeldomain.CapabilityResponses, modeldomain.CapabilityVideo},
			credential:   provider.CredentialSurface{AuthType: account.AuthTypeOAuth, Import: true, Refresh: true, DeviceOAuth: true},
			conversation: provider.ConversationSurface{Responses: true, ChatCompletions: true, Messages: true, Compact: true, StoredResponses: true, CustomTools: true},
			media:        provider.MediaSurface{VideoGeneration: true},
			inference:    provider.InferencePolicy{Usage: provider.UsageUpstream, SessionIdentity: true, QualityHold: true, QualityAutoDisable: true},
			failure: provider.FailurePolicy{
				AuthenticatedForbidden: true, ForceAccountFailover: true, TerminalRequestScopedForbidden: true,
				FatalHeaderTimeout: true, FatalOriginTLS: true,
			},
		},
		{
			provider: account.ProviderWeb, catalog: provider.ModelCatalogStatic, quota: provider.QuotaRemoteWindow,
			capabilities: []modeldomain.Capability{modeldomain.CapabilityChat, modeldomain.CapabilityImage, modeldomain.CapabilityImageEdit, modeldomain.CapabilityVideo},
			credential:   provider.CredentialSurface{AuthType: account.AuthTypeSSO, Import: true},
			conversation: provider.ConversationSurface{Responses: true, ChatCompletions: true, Messages: true, StoredResponses: true},
			media:        provider.MediaSurface{ImageGeneration: true, ImageEdit: true, VideoGeneration: true, ImageGenerationModelPriced: true},
			inference:    provider.InferencePolicy{Usage: provider.UsageEstimated, RetryForbiddenAsEgress: true},
		},
		{
			provider: account.ProviderConsole, catalog: provider.ModelCatalogStatic, quota: provider.QuotaRemoteWindow,
			capabilities: []modeldomain.Capability{modeldomain.CapabilityResponses, modeldomain.CapabilityImage, modeldomain.CapabilityImageEdit, modeldomain.CapabilityVideo, modeldomain.CapabilityTTS, modeldomain.CapabilitySTT, modeldomain.CapabilityRealtime},
			credential:   provider.CredentialSurface{AuthType: account.AuthTypeSSO, Import: true},
			conversation: provider.ConversationSurface{Responses: true, ChatCompletions: true, Messages: true, StatelessPreviousResponse: true},
			media:        provider.MediaSurface{ImageGeneration: true, ImageEdit: true, VideoGeneration: true, TTS: true, STT: true, Realtime: true},
			inference:    provider.InferencePolicy{Usage: provider.UsageUpstream, RetryForbiddenAsEgress: true, QualityHold: true},
			failure:      provider.FailurePolicy{TerminalAuthSchemeForbidden: true, SoftRateLimit: true, PaymentRequiredQuota: true},
		},
		{
			provider: account.ProviderChatGPT, catalog: provider.ModelCatalogRemote, quota: provider.QuotaRemoteWindow,
			capabilities: []modeldomain.Capability{modeldomain.CapabilityChat, modeldomain.CapabilityResponses, modeldomain.CapabilityImage, modeldomain.CapabilityImageEdit, modeldomain.CapabilityTTS, modeldomain.CapabilitySTT},
			credential:   provider.CredentialSurface{AuthType: account.AuthTypeOAuth, Import: true, Refresh: true, RedirectOAuth: true},
			conversation: provider.ConversationSurface{Responses: true, ChatCompletions: true, Messages: true},
			media:        provider.MediaSurface{ImageGeneration: true, ImageEdit: true, TTS: true, STT: true, SingleImagePerRequest: true},
			inference:    provider.InferencePolicy{Usage: provider.UsageEstimated, RetryForbiddenAsEgress: true},
		},
		{
			provider: account.ProviderAdobe, catalog: provider.ModelCatalogStatic, quota: provider.QuotaRemoteWindow,
			capabilities: []modeldomain.Capability{modeldomain.CapabilityImage, modeldomain.CapabilityImageEdit, modeldomain.CapabilityVideo},
			credential:   provider.CredentialSurface{AuthType: account.AuthTypeOAuth, Import: true, Refresh: true},
			conversation: provider.ConversationSurface{},
			media:        provider.MediaSurface{ImageGeneration: true, ImageEdit: true, VideoGeneration: true, ImageGenerationModelPriced: true, VideoLastFrame: true, VideoReferenceVideos: true},
			inference:    provider.InferencePolicy{Usage: provider.UsageEstimated},
		},
		{
			provider: account.ProviderMuse, catalog: provider.ModelCatalogStatic, quota: provider.QuotaRemoteWindow,
			capabilities: []modeldomain.Capability{modeldomain.CapabilityChat, modeldomain.CapabilityImage, modeldomain.CapabilityImageEdit, modeldomain.CapabilityVideo},
			credential:   provider.CredentialSurface{AuthType: account.AuthTypeOAuth, Import: true, Refresh: true},
			conversation: provider.ConversationSurface{Responses: true, ChatCompletions: true, Messages: true, StatelessPreviousResponse: true},
			media:        provider.MediaSurface{ImageGeneration: true, ImageEdit: true, VideoGeneration: true, ImageGenerationModelPriced: true, SingleImagePerRequest: true},
			inference:    provider.InferencePolicy{Usage: provider.UsageEstimated},
		},
	}
	for _, test := range tests {
		t.Run(string(test.provider), func(t *testing.T) {
			definition, ok := registry.Definition(test.provider)
			if !ok {
				t.Fatal("definition not registered")
			}
			if definition.ModelNamespace != test.provider.ModelNamespace() || definition.ModelCatalog != test.catalog || definition.Quota != test.quota {
				t.Fatalf("definition identity = %#v", definition)
			}
			if !reflect.DeepEqual(definition.ModelCapabilities, test.capabilities) || definition.Credential != test.credential || definition.Conversation != test.conversation || definition.Media != test.media || definition.Inference != test.inference || definition.Failure != test.failure {
				t.Fatalf("definition capabilities = %#v", definition)
			}
			for _, operation := range []string{"responses", "chat", "messages"} {
				if registry.SupportsConversation(test.provider, operation) != test.conversation.Supports(operation) {
					t.Fatalf("%s %s conversation support does not match the declared surface", test.provider, operation)
				}
			}
		})
	}
	if !registry.SupportsResponseCompaction(account.ProviderBuild) || registry.SupportsResponseCompaction(account.ProviderWeb) || registry.SupportsResponseCompaction(account.ProviderConsole) {
		t.Fatal("response compaction capability boundary is inconsistent")
	}
	if !registry.SupportsConversation(account.ProviderBuild, "compaction") || registry.SupportsConversation(account.ProviderWeb, "compaction") || registry.SupportsConversation(account.ProviderConsole, "compaction") {
		t.Fatal("compaction operation capability boundary is inconsistent")
	}
	definition, _ := registry.Definition(account.ProviderWeb)
	definition.ModelCapabilities[0] = modeldomain.CapabilityResponses
	stored, _ := registry.Definition(account.ProviderWeb)
	if stored.ModelCapabilities[0] != modeldomain.CapabilityChat {
		t.Fatal("registry definition was mutated through a returned slice")
	}
}

func TestProviderDefinitionRejectsInconsistentMediaCapability(t *testing.T) {
	definition := provider.Definition{
		Provider:          account.ProviderWeb,
		ModelNamespace:    account.ProviderWeb.ModelNamespace(),
		ModelCatalog:      provider.ModelCatalogStatic,
		ModelCapabilities: []modeldomain.Capability{modeldomain.CapabilityImage},
		Quota:             provider.QuotaRemoteWindow,
		Credential:        provider.CredentialSurface{AuthType: account.AuthTypeSSO},
	}
	if err := definition.Validate(); err == nil {
		t.Fatal("inconsistent media declaration was accepted")
	}
}

// 尾帧与参考视频只交给声明了对应 Media 能力的 Provider；其余渠道在注册表层直接拒绝。
// 不传的视频参数由各 Provider 补：Grok 三家用 xAI 的 16:9 / 720p / 8 秒并限定取值，Adobe
// 用模型自己的默认值，传了不支持的就近对齐。
func TestRegistryNormalizesVideoParametersPerProvider(t *testing.T) {
	registry := provider.NewRegistry(
		cliprovider.NewAdapter(cliprovider.Config{}, nil),
		webprovider.NewAdapter(webprovider.Config{}, nil, nil, nil, nil),
		consoleprovider.NewAdapter(consoleprovider.Config{}, nil, nil, nil),
		adobeprovider.NewAdapter(adobeprovider.Config{}, nil, nil, nil, nil),
	)
	for _, value := range []account.Provider{account.ProviderBuild, account.ProviderWeb, account.ProviderConsole} {
		grok := provider.VideoRequestOptions{Operation: provider.VideoOperationGenerate, UpstreamModel: "grok-imagine-video"}
		if got, err := registry.NormalizeVideoRequest(value, grok); err != nil || got.AspectRatio != "16:9" || got.Resolution != "720p" || got.Duration != 8 {
			t.Fatalf("%s defaults = %+v, %v", value, got, err)
		}
		for _, bad := range []provider.VideoRequestOptions{{Duration: 16}, {AspectRatio: "21:9"}, {Resolution: "4k"}, {Resolution: "1080p", ReferenceAudios: 1}} {
			bad.Operation, bad.UpstreamModel = grok.Operation, grok.UpstreamModel
			if _, err := registry.NormalizeVideoRequest(value, bad); !errors.Is(err, provider.ErrVideoParameterInvalid) {
				t.Errorf("%s %+v err = %v", value, bad, err)
			}
		}
	}
	seedance := provider.VideoRequestOptions{Operation: provider.VideoOperationGenerate, UpstreamModel: "seedance/seedance_2.0"}
	if got, err := registry.NormalizeVideoRequest(account.ProviderAdobe, seedance); err != nil || got.AspectRatio != "16:9" || got.Resolution != "720p" || got.Duration != 4 {
		t.Fatalf("seedance defaults = %+v, %v", got, err)
	}
	seedance.AspectRatio, seedance.Resolution, seedance.Duration = "21:9", "4k", 30
	if got, err := registry.NormalizeVideoRequest(account.ProviderAdobe, seedance); err != nil || got.AspectRatio != "21:9" || got.Resolution != "1080p" || got.Duration != 15 {
		t.Fatalf("seedance explicit = %+v, %v", got, err)
	}
}

// 编辑与延长的取值也归 Provider：xAI 的编辑不收 duration、比例和分辨率；延长不收比例和
// 分辨率，时长 2 到 10 秒，默认 6 秒。
func TestNormalizeGrokVideoRequestEditAndExtension(t *testing.T) {
	edit, extend := provider.VideoOperationEdit, provider.VideoOperationExtend
	for _, test := range []struct {
		in   provider.VideoRequestOptions
		want int // 实际时长；-1 表示拒绝
	}{
		{provider.VideoRequestOptions{Operation: edit}, 0},
		{provider.VideoRequestOptions{Operation: edit, Duration: 6}, -1},
		{provider.VideoRequestOptions{Operation: edit, Resolution: "720p"}, -1},
		{provider.VideoRequestOptions{Operation: edit, AspectRatio: "16:9"}, -1},
		{provider.VideoRequestOptions{Operation: extend}, 6},
		{provider.VideoRequestOptions{Operation: extend, Duration: 2}, 2},
		{provider.VideoRequestOptions{Operation: extend, Duration: 10}, 10},
		{provider.VideoRequestOptions{Operation: extend, Duration: 1}, -1},
		{provider.VideoRequestOptions{Operation: extend, Duration: 11}, -1},
		{provider.VideoRequestOptions{Operation: extend, AspectRatio: "16:9"}, -1},
		{provider.VideoRequestOptions{Operation: extend, Resolution: "720p"}, -1},
	} {
		got, err := provider.NormalizeGrokVideoRequest(test.in)
		if test.want < 0 {
			if !errors.Is(err, provider.ErrVideoParameterInvalid) {
				t.Errorf("%+v err = %v, want rejected", test.in, err)
			}
		} else if err != nil || got.Duration != test.want || got.AspectRatio != "" || got.Resolution != "" {
			t.Errorf("%+v = %+v, %v; want %d s", test.in, got, err, test.want)
		}
	}
}

func TestRegistryRejectsLastFrameForProvidersWithoutSupport(t *testing.T) {
	registry := provider.NewRegistry(
		webprovider.NewAdapter(webprovider.Config{}, nil, nil, nil, nil),
		adobeprovider.NewAdapter(adobeprovider.Config{}, nil, nil, nil, nil),
	)
	options := provider.VideoRequestOptions{Operation: provider.VideoOperationGenerate, UpstreamModel: "grok-imagine-video", HasImage: true, HasLastFrame: true, Duration: 6, Resolution: "720p"}
	if err := registry.ValidateVideoRequest(account.ProviderWeb, options); !errors.Is(err, provider.ErrVideoParameterInvalid) {
		t.Fatalf("web last frame err = %v", err)
	}
	options.UpstreamModel = "veo/3.1-generate"
	if err := registry.ValidateVideoRequest(account.ProviderAdobe, options); err != nil {
		t.Fatalf("adobe veo last frame err = %v", err)
	}

	clips := provider.VideoRequestOptions{Operation: provider.VideoOperationGenerate, UpstreamModel: "grok-imagine-video", ReferenceVideos: 1, Duration: 6, Resolution: "720p"}
	if err := registry.ValidateVideoRequest(account.ProviderWeb, clips); !errors.Is(err, provider.ErrVideoParameterInvalid) {
		t.Fatalf("web reference videos err = %v", err)
	}
	clips.UpstreamModel = "seedance/seedance_2.0"
	if err := registry.ValidateVideoRequest(account.ProviderAdobe, clips); err != nil {
		t.Fatalf("adobe seedance reference videos err = %v", err)
	}
}
