package clientkey

import (
	"slices"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

const (
	DefaultRPMLimit      = 120
	DefaultMaxConcurrent = 8
	MaxRPMLimit          = 100000
	MaxConcurrent        = 1024
	MaxBillingLimitTicks = 9_000_000_000_000_000
)

const InternalKindQualityGuard = "quality_guard"

// ProviderScope restricts a key to upstream providers. All covers every
// registered provider, including providers registered after the key was saved;
// otherwise Providers is a sorted, duplicate-free explicit set. The zero value
// is unrestricted, matching the zero AccountScope used by internal callers.
type ProviderScope struct {
	All       bool
	Providers []account.Provider
}

// AllProviders returns the scope that follows the provider registry.
func AllProviders() ProviderScope { return ProviderScope{All: true} }

// ProviderScopeOf returns the normalized explicit set; no providers means all.
func ProviderScopeOf(providers ...account.Provider) ProviderScope {
	value, _ := NormalizeProviderScope(ProviderScope{Providers: providers})
	return value
}

type TierScope uint8

const (
	TierScopeFree TierScope = 1 << iota
	TierScopeSuper
	TierScopeUnknown
	TierScopeAll = TierScopeFree | TierScopeSuper | TierScopeUnknown
)

type AccountTier uint8

const (
	AccountTierFree AccountTier = iota + 1
	AccountTierSuper
	AccountTierUnknown
)

type AccountScope struct {
	Providers ProviderScope
	Tiers     TierScope
}

// ParseProviderScopeValues accepts ["all"] or syntactically valid provider ids.
// Whether an id is registered is checked by the application layer.
func ParseProviderScopeValues(values []string) (ProviderScope, bool) {
	if len(values) == 0 {
		return ProviderScope{}, false
	}
	if len(values) == 1 && values[0] == "all" {
		return AllProviders(), true
	}
	providers := make([]account.Provider, 0, len(values))
	for _, value := range values {
		provider := account.Provider(value)
		if value == "all" || !provider.IsValid() {
			return ProviderScope{}, false
		}
		providers = append(providers, provider)
	}
	return NormalizeProviderScope(ProviderScope{Providers: providers})
}

func ParseTierScopeValues(values []string) (TierScope, bool) {
	if len(values) == 0 {
		return 0, false
	}
	var scope TierScope
	for _, value := range values {
		switch value {
		case "all":
			if len(values) != 1 {
				return 0, false
			}
			return TierScopeAll, true
		case "free":
			scope |= TierScopeFree
		case "super":
			scope |= TierScopeSuper
		default:
			return 0, false
		}
	}
	return NormalizeTierScope(scope)
}

func (s ProviderScope) Values() []string {
	value, valid := NormalizeProviderScope(s)
	if !valid || value.All {
		return []string{"all"}
	}
	values := make([]string, 0, len(value.Providers))
	for _, provider := range value.Providers {
		values = append(values, string(provider))
	}
	return values
}

func (s TierScope) Values() []string {
	value, valid := NormalizeTierScope(s)
	if !valid || value == TierScopeAll {
		return []string{"all"}
	}
	values := make([]string, 0, 2)
	if value&TierScopeFree != 0 {
		values = append(values, "free")
	}
	if value&TierScopeSuper != 0 {
		values = append(values, "super")
	}
	return values
}

// NormalizeProviderScope sorts and deduplicates an explicit set; an empty set
// means all providers. An already-normalized set is returned without copying
// because routing checks the scope on every request.
func NormalizeProviderScope(value ProviderScope) (ProviderScope, bool) {
	if value.All || len(value.Providers) == 0 {
		return AllProviders(), true
	}
	sorted := true
	for index, provider := range value.Providers {
		if !provider.IsValid() {
			return value, false
		}
		sorted = sorted && (index == 0 || value.Providers[index-1] < provider)
	}
	if sorted {
		return ProviderScope{Providers: value.Providers}, true
	}
	return ProviderScope{Providers: slices.Compact(slices.Sorted(slices.Values(value.Providers)))}, true
}

func (s ProviderScope) Equal(other ProviderScope) bool {
	left, leftValid := NormalizeProviderScope(s)
	right, rightValid := NormalizeProviderScope(other)
	return leftValid == rightValid && left.All == right.All && slices.Equal(left.Providers, right.Providers)
}

func NormalizeTierScope(value TierScope) (TierScope, bool) {
	if value == 0 {
		return TierScopeAll, true
	}
	switch value {
	case TierScopeFree, TierScopeSuper, TierScopeFree | TierScopeSuper, TierScopeAll:
		return value, true
	default:
		return value, false
	}
}

func NormalizeAccountScope(value AccountScope) (AccountScope, bool) {
	providers, providersValid := NormalizeProviderScope(value.Providers)
	tiers, tiersValid := NormalizeTierScope(value.Tiers)
	return AccountScope{Providers: providers, Tiers: tiers}, providersValid && tiersValid
}

func (s ProviderScope) Allows(provider account.Provider) bool {
	value, valid := NormalizeProviderScope(s)
	if !valid || !provider.IsValid() {
		return false
	}
	return value.All || slices.Contains(value.Providers, provider)
}

func (s TierScope) Allows(tier AccountTier) bool {
	value, valid := NormalizeTierScope(s)
	if !valid {
		return false
	}
	switch tier {
	case AccountTierFree:
		return value&TierScopeFree != 0
	case AccountTierSuper:
		return value&TierScopeSuper != 0
	case AccountTierUnknown:
		return value&TierScopeUnknown != 0
	default:
		return false
	}
}

func (s AccountScope) AllowsProvider(provider account.Provider) bool {
	value, valid := NormalizeAccountScope(s)
	return valid && value.Providers.Allows(provider)
}

func (s AccountScope) IsRestricted() bool {
	value, valid := NormalizeAccountScope(s)
	return valid && (!value.Providers.All || value.Tiers != TierScopeAll)
}

func (k Key) AccountScope() AccountScope {
	value, _ := NormalizeAccountScope(AccountScope{Providers: k.ProviderScope, Tiers: k.TierScope})
	return value
}

// Key 表示下游客户端调用凭据及其限制。
type Key struct {
	ID              uint64
	Name            string
	Prefix          string
	SecretHash      string
	EncryptedSecret string
	// InternalKind marks server-managed identities that must never be exposed
	// through the public client-key lifecycle or accepted by external auth.
	InternalKind          string
	Enabled               bool
	ExpiresAt             *time.Time
	RPMLimit              int
	MaxConcurrent         int
	BillingLimitUSDTicks  int64
	BilledUsageUSDTicks   int64
	ReservedUsageUSDTicks int64
	// AllowModelAliases enables discovery and use of dynamically generated reasoning-effort aliases.
	// Registered compatibility aliases remain available to avoid breaking existing clients.
	AllowModelAliases bool
	AllowedModels     []uint64
	// ProviderScope and TierScope narrow routing without adding request-time storage lookups.
	ProviderScope ProviderScope
	TierScope     TierScope
	LastUsedAt    *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// IsAvailable 判断客户端 Key 当前是否可用。
func (k Key) IsAvailable(now time.Time) bool {
	if !k.Enabled {
		return false
	}
	return k.ExpiresAt == nil || now.Before(*k.ExpiresAt)
}

func (k Key) AllowsModel(modelID uint64) bool {
	if len(k.AllowedModels) == 0 {
		return true
	}
	for _, allowed := range k.AllowedModels {
		if allowed == modelID {
			return true
		}
	}
	return false
}
