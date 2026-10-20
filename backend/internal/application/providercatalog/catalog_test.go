package providercatalog

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/chatgpt"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
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
	entries := New(registry).Entries()
	if len(entries) != 2 || entries[0].ID != "grok_build" || entries[1].ID != "grok_console" {
		t.Fatalf("order: %+v", entries)
	}
	build, console := entries[0], entries[1]
	if build.Name != "Grok Build" || build.ShortName != "Build" || !build.TierScoped || len(build.EgressScopes) != 1 || build.EgressScopes[0] != "grok_build" {
		t.Fatalf("build entry: %+v", build)
	}
	if console.ShortName != "Grok Console" || console.TierScoped || len(console.EgressScopes) != 2 {
		t.Fatalf("console entry: %+v", console)
	}
}

func TestCatalogPublishesCredentialAndQuotaSurface(t *testing.T) {
	chat := definition("acme_chat", provider.ProfileSurface{DefaultMaxConcurrent: 1, QuotaModes: []string{"reason", "image_gen"}})
	chat.Credential = provider.CredentialSurface{AuthType: account.AuthTypeOAuth, Import: true, Refresh: true, RedirectOAuth: true}
	registry := provider.NewRegistry(definedAdapter{chat}, definedAdapter{definition(account.ProviderBuild, provider.ProfileSurface{})})
	entries := New(registry).Entries()
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"quotaModes":null`) {
		t.Fatalf("quotaModes must never be null: %s", data)
	}
	byID := map[string]Entry{}
	for _, entry := range entries {
		byID[entry.ID] = entry
	}
	if got := byID["acme_chat"]; !got.Credential.RedirectOAuth || got.DefaultMaxConcurrent != 1 || len(got.QuotaModes) != 2 || got.QuotaModes[0] != "reason" {
		t.Fatalf("chat entry = %+v", got)
	}
	if got := byID["grok_build"]; got.Credential.RedirectOAuth || got.DefaultMaxConcurrent != account.DefaultMaxConcurrent || got.QuotaModes == nil {
		t.Fatalf("build entry = %+v", got)
	}
}

func TestCatalogMarksAsyncImageProviders(t *testing.T) {
	catalog := New(provider.NewRegistry(chatgpt.NewAdapter(chatgpt.Config{}, nil, nil, nil), cli.NewAdapter(cli.Config{}, nil)))
	async := map[string]bool{}
	for _, entry := range catalog.Entries() {
		async[entry.ID] = entry.Media.AsyncImages
	}
	if !async["chatgpt"] || async["grok_build"] {
		t.Fatalf("async images = %v", async)
	}
}
