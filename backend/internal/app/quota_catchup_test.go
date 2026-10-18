package app

import (
	"reflect"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

type quotaPolicyAdapter struct {
	id    accountdomain.Provider
	quota provider.QuotaKind
}

func (a quotaPolicyAdapter) Provider() accountdomain.Provider { return a.id }

func (a quotaPolicyAdapter) Definition() provider.Definition {
	return provider.Definition{Provider: a.id, Quota: a.quota}
}

func TestStaleQuotaCatchupCoversRemoteWindowProvidersExceptConsole(t *testing.T) {
	registry := provider.NewRegistry(
		quotaPolicyAdapter{accountdomain.ProviderBuild, provider.QuotaBilling},
		quotaPolicyAdapter{accountdomain.ProviderWeb, provider.QuotaRemoteWindow},
		quotaPolicyAdapter{accountdomain.ProviderConsole, provider.QuotaRemoteWindow},
		quotaPolicyAdapter{"acme_chat", provider.QuotaRemoteWindow},
		quotaPolicyAdapter{"acme_local", provider.QuotaLocalWindow},
	)
	got := staleQuotaCatchupProviders(registry)
	if want := []accountdomain.Provider{accountdomain.ProviderWeb, "acme_chat"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("providers = %v, want %v", got, want)
	}
}
