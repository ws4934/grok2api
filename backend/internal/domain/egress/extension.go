package egress

import "slices"

// extensionScopes lists the egress scopes owned by registered extension
// channels. It is written once during application startup, before any request
// is served, and only read afterwards.
var extensionScopes []Scope

// SetExtensionScopes registers extension channel IDs as egress scopes. These
// scopes get proxy nodes, subscription sources and fallback configuration;
// Cloudflare clearance is solved on demand for scopes that set a target.
func SetExtensionScopes(scopes []Scope) { extensionScopes = slices.Clone(scopes) }

func IsExtensionScope(scope Scope) bool { return slices.Contains(extensionScopes, scope) }

func ExtensionScopes() []Scope { return slices.Clone(extensionScopes) }

// AllScopes lists the Grok scopes followed by the extension scopes.
func AllScopes() []Scope {
	return append([]Scope{ScopeBuild, ScopeWeb, ScopeConsole, ScopeWebAsset, ScopeConsoleAsset}, extensionScopes...)
}
