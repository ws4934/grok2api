package provider_test

import (
	"context"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	cliprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
)

type interactiveContractAdapter struct{ credential provider.CredentialSurface }

func (interactiveContractAdapter) Provider() account.Provider { return "acme_login" }

func (a interactiveContractAdapter) Definition() provider.Definition {
	return provider.Definition{
		Provider: "acme_login", ModelNamespace: account.Provider("acme_login").ModelNamespace(), ModelCatalog: provider.ModelCatalogStatic,
		ModelCapabilities: []modeldomain.Capability{modeldomain.CapabilityChat}, Quota: provider.QuotaBilling,
		Credential: a.credential, Inference: provider.InferencePolicy{Usage: provider.UsageUpstream},
	}
}

func (interactiveContractAdapter) ListModels(context.Context, account.Credential) ([]string, error) {
	return nil, nil
}

func (interactiveContractAdapter) GetBilling(context.Context, account.Credential) (account.Billing, error) {
	return account.Billing{}, nil
}

type interactiveLoginAdapter struct{ interactiveContractAdapter }

func (interactiveLoginAdapter) StartAuthorization(context.Context) (provider.AuthorizationChallenge, error) {
	return provider.AuthorizationChallenge{}, nil
}

func (interactiveLoginAdapter) CompleteAuthorization(context.Context, provider.AuthorizationChallenge, string) (provider.CredentialSeed, error) {
	return provider.CredentialSeed{}, nil
}

func TestRegistryRequiresInteractiveAuthForDeviceAndRedirectLogins(t *testing.T) {
	for _, credential := range []provider.CredentialSurface{
		{AuthType: account.AuthTypeOAuth, DeviceOAuth: true},
		{AuthType: account.AuthTypeOAuth, RedirectOAuth: true},
	} {
		if err := provider.NewRegistry(interactiveContractAdapter{credential}).Validate(); err == nil {
			t.Fatalf("%+v accepted without InteractiveAuthAdapter", credential)
		}
		registry := provider.NewRegistry(interactiveLoginAdapter{interactiveContractAdapter{credential}})
		if err := registry.Validate(); err != nil {
			t.Fatalf("%+v: %v", credential, err)
		}
		if _, ok := registry.InteractiveAuth("acme_login"); !ok {
			t.Fatal("interactive auth adapter not exposed")
		}
	}
	if _, ok := provider.NewRegistry(cliprovider.NewAdapter(cliprovider.Config{}, nil)).InteractiveAuth(account.ProviderBuild); !ok {
		t.Fatal("Grok Build device login is not an InteractiveAuthAdapter")
	}
}
