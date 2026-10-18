package app

import (
	"reflect"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

type quotaPolicyAdapter struct {
	id         accountdomain.Provider
	quota      provider.QuotaKind
	staleAfter time.Duration
}

func (a quotaPolicyAdapter) Provider() accountdomain.Provider { return a.id }

func (a quotaPolicyAdapter) Definition() provider.Definition {
	return provider.Definition{Provider: a.id, Quota: a.quota, Profile: provider.ProfileSurface{QuotaStaleAfter: a.staleAfter}}
}

func TestStaleQuotaCatchupCoversRemoteWindowProvidersExceptConsole(t *testing.T) {
	registry := provider.NewRegistry(
		quotaPolicyAdapter{accountdomain.ProviderBuild, provider.QuotaBilling, 0},
		quotaPolicyAdapter{accountdomain.ProviderWeb, provider.QuotaRemoteWindow, 0},
		quotaPolicyAdapter{accountdomain.ProviderConsole, provider.QuotaRemoteWindow, 0},
		quotaPolicyAdapter{"acme_chat", provider.QuotaRemoteWindow, 6 * time.Hour},
		quotaPolicyAdapter{"acme_local", provider.QuotaLocalWindow, 0},
	)
	got := staleQuotaCatchups(registry)
	want := []staleQuotaCatchup{{accountdomain.ProviderWeb, webQuotaStaleAfter}, {"acme_chat", 6 * time.Hour}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("catch-ups = %v, want %v", got, want)
	}
}
