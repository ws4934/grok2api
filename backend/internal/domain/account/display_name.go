package account

import "strings"

// DisplayName keeps explicit names and replaces generated identifiers with the
// same own-email / directly linked-email order used by the account page.
func (c Credential) DisplayName() string {
	name := strings.TrimSpace(c.Name)
	if name != "" && !c.generatedName(name) {
		return name
	}
	if email := strings.TrimSpace(c.Email); email != "" {
		return email
	}
	for _, peer := range c.LinkedAccounts {
		if email := strings.TrimSpace(peer.Email); email != "" {
			return email
		}
	}
	if name != "" {
		return name
	}
	if id := strings.TrimSpace(c.UserID); id != "" {
		return id
	}
	return string(c.Provider)
}
func (c Credential) generatedName(name string) bool {
	prefix := ""
	switch c.Provider {
	case ProviderWeb:
		prefix = "Grok Web "
	case ProviderConsole:
		prefix = "Grok Console "
	}
	if prefix != "" && strings.HasPrefix(name, prefix) && shortHexID(strings.TrimPrefix(name, prefix)) {
		return true
	}
	prefix = string(c.Provider) + "-"
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	id := strings.TrimPrefix(name, prefix)
	userID := []rune(strings.TrimSpace(c.UserID))
	return shortHexID(id) || (len(userID) > 0 && id == string(userID[:min(8, len(userID))]))
}
func shortHexID(value string) bool {
	if len(value) != 8 {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}
