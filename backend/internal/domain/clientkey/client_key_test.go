package clientkey

import (
	"reflect"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestAccountScopeParsingAndNormalization(t *testing.T) {
	if _, valid := ParseProviderScopeValues(nil); valid {
		t.Fatal("an explicitly empty provider scope must be rejected")
	}
	if _, valid := ParseTierScopeValues(nil); valid {
		t.Fatal("an explicitly empty tier scope must be rejected")
	}
	providers, valid := ParseProviderScopeValues([]string{"grok_web", "grok_build", "grok_web"})
	if !valid || !providers.Equal(ProviderScopeOf(account.ProviderBuild, account.ProviderWeb)) {
		t.Fatalf("provider scope = %+v, valid = %v", providers, valid)
	}
	if values := providers.Values(); !reflect.DeepEqual(values, []string{"grok_build", "grok_web"}) {
		t.Fatalf("provider values = %#v", values)
	}
	for _, values := range [][]string{{"all", "grok_web"}, {"grok_web", "all"}, {""}, {"Grok_Web"}, {"grok-web"}} {
		if _, valid := ParseProviderScopeValues(values); valid {
			t.Fatalf("provider scope %#v must be rejected", values)
		}
	}
	all, valid := ParseProviderScopeValues([]string{"all"})
	if !valid || !all.All || len(all.Providers) != 0 {
		t.Fatalf("all provider scope = %+v, valid = %v", all, valid)
	}
	if values := AllProviders().Values(); !reflect.DeepEqual(values, []string{"all"}) {
		t.Fatalf("all provider values = %#v", values)
	}
	if !(ProviderScope{}).Equal(AllProviders()) || ProviderScopeOf(account.ProviderWeb).Equal(AllProviders()) {
		t.Fatal("the zero provider scope must equal all providers and differ from an explicit set")
	}
	if _, valid := NormalizeProviderScope(ProviderScope{Providers: []account.Provider{"grok_web", "Bad"}}); valid {
		t.Fatal("an invalid provider id must be rejected even after an unsorted prefix")
	}
	tiers, valid := ParseTierScopeValues([]string{"free", "super"})
	if !valid || tiers != TierScopeFree|TierScopeSuper {
		t.Fatalf("tier scope = %d, valid = %v", tiers, valid)
	}
	if tiers == TierScopeAll {
		t.Fatal("free + super must continue excluding unknown tiers")
	}
	if _, valid := ParseTierScopeValues([]string{"all", "free"}); valid {
		t.Fatal("all mixed with a tier must be rejected")
	}
	if values := tiers.Values(); !reflect.DeepEqual(values, []string{"free", "super"}) {
		t.Fatalf("tier values = %#v", values)
	}
}

func TestProviderScopeAllowsFutureProviders(t *testing.T) {
	const chatgpt = account.Provider("chatgpt")
	if !AllProviders().Allows(chatgpt) || !(AccountScope{}).AllowsProvider(chatgpt) {
		t.Fatal("all providers must include providers registered later")
	}
	explicit, valid := ParseProviderScopeValues([]string{"grok_web", "chatgpt"})
	if !valid {
		t.Fatal("a syntactically valid future provider must be accepted")
	}
	if values := explicit.Values(); !reflect.DeepEqual(values, []string{"chatgpt", "grok_web"}) {
		t.Fatalf("explicit provider values = %#v", values)
	}
	if !explicit.Allows(chatgpt) || !explicit.Allows(account.ProviderWeb) || explicit.Allows(account.ProviderBuild) {
		t.Fatalf("explicit provider scope = %+v", explicit)
	}
	if AllProviders().Allows("") || AllProviders().Allows("Bad") {
		t.Fatal("invalid provider ids must never be allowed")
	}
}

func TestAccountScopeRestrictionAndProviderGate(t *testing.T) {
	if (AccountScope{}).IsRestricted() || (AccountScope{Providers: AllProviders(), Tiers: TierScopeAll}).IsRestricted() {
		t.Fatal("all providers and all tiers must be unrestricted")
	}
	scope := AccountScope{Providers: ProviderScopeOf(account.ProviderBuild, account.ProviderConsole), Tiers: TierScopeAll}
	if !scope.IsRestricted() || !scope.AllowsProvider(account.ProviderConsole) || scope.AllowsProvider(account.ProviderWeb) {
		t.Fatalf("explicit provider scope = %+v", scope)
	}
	if !(AccountScope{Providers: AllProviders(), Tiers: TierScopeFree}).IsRestricted() {
		t.Fatal("a tier restriction must mark the scope as restricted")
	}
	if (AccountScope{Providers: AllProviders(), Tiers: TierScope(4)}).AllowsProvider(account.ProviderBuild) {
		t.Fatal("an invalid tier scope must fail closed")
	}
}
