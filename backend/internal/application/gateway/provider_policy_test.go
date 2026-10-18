package gateway

import (
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
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

// grokTestRegistry holds all production Grok adapters for routing hooks.
var grokTestRegistry = grokRegistry(account.ProviderBuild, account.ProviderWeb, account.ProviderConsole)

// grokRoutingHooks gives selector test doubles the production routing hooks
// next to their own tier orders.
type grokRoutingHooks struct{}

func (grokRoutingHooks) AccountTier(value account.Provider, candidate account.RoutingCandidate) clientkey.AccountTier {
	return grokTestRegistry.AccountTier(value, candidate)
}

func (grokRoutingHooks) CandidateQuotaMode(value account.Provider, candidate account.RoutingCandidate, quotaMode string) string {
	return grokTestRegistry.CandidateQuotaMode(value, candidate, quotaMode)
}

func (grokRoutingHooks) StaticCatalog(value account.Provider) bool {
	return grokTestRegistry.StaticCatalog(value)
}

func (grokRoutingHooks) StaticModel(value account.Provider, quotaMode string) bool {
	return grokTestRegistry.StaticModel(value, quotaMode)
}

func (grokRoutingHooks) SchedulingRisk(value account.Provider, credential account.Credential) bool {
	return grokTestRegistry.SchedulingRisk(value, credential)
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

func staticModelOf(value account.Provider, quotaMode string) bool {
	return grokRegistry(value).StaticModel(value, quotaMode)
}

func failurePolicyOf(value account.Provider) provider.FailurePolicy {
	return grokDefinition(value).Failure
}

// withGrokPolicy copies the gateway policies of the production provider into a fake's definition.
func withGrokPolicy(definition provider.Definition) provider.Definition {
	grok := grokDefinition(definition.Provider)
	if definition.ModelCatalog == "" {
		definition.ModelCatalog = grok.ModelCatalog
	}
	definition.Conversation.StatelessPreviousResponse = grok.Conversation.StatelessPreviousResponse
	definition.Inference.SessionIdentity = grok.Inference.SessionIdentity
	definition.Inference.QualityHold = grok.Inference.QualityHold
	definition.Inference.QualityAutoDisable = grok.Inference.QualityAutoDisable
	definition.Media.ImageGenerationModelPriced = grok.Media.ImageGenerationModelPriced
	definition.Failure = grok.Failure
	return definition
}
