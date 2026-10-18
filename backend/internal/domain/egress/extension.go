package egress

import "slices"

// extensionScopes lists the egress scopes owned by registered extension
// channels. It is written once during application startup, before any request
// is served, and only read afterwards.
var extensionScopes []Scope

// SetExtensionScopes registers extension channel IDs as egress scopes. These
// scopes get proxy nodes and subscription sources, but no fallback node
// configuration and no Cloudflare clearance handling.
func SetExtensionScopes(scopes []Scope) { extensionScopes = slices.Clone(scopes) }

func IsExtensionScope(scope Scope) bool { return slices.Contains(extensionScopes, scope) }

func ExtensionScopes() []Scope { return slices.Clone(extensionScopes) }
