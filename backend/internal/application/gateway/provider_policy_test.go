package gateway

import (
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/console"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/web"
)

// Provider-specific gateway behavior is declared by the production adapters.
// Fakes that stand in for a Grok provider copy those declarations so the
// gateway treats them exactly like the real provider.
var grokTestAdapters = map[account.Provider]provider.DefinitionAdapter{
	account.ProviderBuild:   cli.NewAdapter(cli.Config{}, nil),
	account.ProviderWeb:     web.NewAdapter(web.Config{}, nil, nil, nil, nil),
	account.ProviderConsole: console.NewAdapter(console.Config{}, nil, nil, nil),
}

// grokDefinition returns the production Definition, or an empty one for other providers.
func grokDefinition(value account.Provider) provider.Definition {
	if adapter, ok := grokTestAdapters[value]; ok {
		return adapter.Definition()
	}
	return provider.Definition{Provider: value}
}

// grokRegistry registers the production adapters of the given Grok providers.
func grokRegistry(values ...account.Provider) *provider.Registry {
	adapters := make([]provider.Adapter, 0, len(values))
	for _, value := range values {
		adapters = append(adapters, grokTestAdapters[value])
	}
	return provider.NewRegistry(adapters...)
}

func failurePolicyOf(value account.Provider) provider.FailurePolicy {
	return grokDefinition(value).Failure
}

// withGrokPolicy copies the gateway policies of the production provider into a fake's definition.
func withGrokPolicy(definition provider.Definition) provider.Definition {
	grok := grokDefinition(definition.Provider)
	definition.Conversation.StatelessPreviousResponse = grok.Conversation.StatelessPreviousResponse
	definition.Inference.SessionIdentity = grok.Inference.SessionIdentity
	definition.Inference.QualityHold = grok.Inference.QualityHold
	definition.Inference.QualityAutoDisable = grok.Inference.QualityAutoDisable
	definition.Media.ImageGenerationModelPriced = grok.Media.ImageGenerationModelPriced
	definition.Failure = grok.Failure
	return definition
}
