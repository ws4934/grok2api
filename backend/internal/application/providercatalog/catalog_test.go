package providercatalog

import (
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

type definedAdapter struct{ definition provider.Definition }

func (a definedAdapter) Provider() account.Provider      { return a.definition.Provider }
func (a definedAdapter) Definition() provider.Definition { return a.definition }

func definition(p account.Provider, profile provider.ProfileSurface) provider.Definition {
	return provider.Definition{
		Provider: p, Profile: profile, ModelNamespace: p.ModelNamespace(), ModelCatalog: provider.ModelCatalogStatic,
		ModelCapabilities: []modeldomain.Capability{modeldomain.CapabilityResponses}, Quota: provider.QuotaRemoteWindow,
		Credential: provider.CredentialSurface{AuthType: account.AuthTypeSSO}, Conversation: provider.ConversationSurface{Responses: true},
		Inference: provider.InferencePolicy{Usage: provider.UsageUpstream},
	}
}

func TestCatalogFollowsProviderOrderAndDefinitions(t *testing.T) {
	registry := provider.NewRegistry(
		definedAdapter{definition(account.ProviderConsole, provider.ProfileSurface{Name: "Grok Console", TierExempt: true, EgressScopes: []egressdomain.Scope{egressdomain.ScopeConsole, egressdomain.ScopeConsoleAsset}})},
		definedAdapter{definition(account.ProviderBuild, provider.ProfileSurface{Name: "Grok Build", ShortName: "Build"})},
	)
	entries := New(registry, Entry{ID: "chatgpt", Name: "ChatGPT", EgressScopes: []string{"chatgpt"}}).Entries()
	if len(entries) != 3 || entries[0].ID != "grok_build" || entries[1].ID != "grok_console" || entries[2].ID != "chatgpt" {
		t.Fatalf("order: %+v", entries)
	}
	build, console := entries[0], entries[1]
	if build.Name != "Grok Build" || build.ShortName != "Build" || !build.TierScoped || len(build.EgressScopes) != 1 || build.EgressScopes[0] != "grok_build" {
		t.Fatalf("build entry: %+v", build)
	}
	if console.ShortName != "Grok Console" || console.TierScoped || len(console.EgressScopes) != 2 {
		t.Fatalf("console entry: %+v", console)
	}
	if entries[2].Order != 2 || !New(registry).Has(account.ProviderBuild) || New(registry).Has("chatgpt") {
		t.Fatal("catalog membership")
	}
}
