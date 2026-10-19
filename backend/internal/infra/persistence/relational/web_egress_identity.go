package relational

import (
	"encoding/hex"
	"strings"
)

const (
	webSSOSourcePrefix    = "sso:"
	stableSSODigestLength = 64
	egressDigestLength    = 32
)

func egressIdentityFromWebSourceKey(sourceKey string) (string, bool) {
	digest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(sourceKey), webSSOSourcePrefix))
	if !strings.HasPrefix(strings.TrimSpace(sourceKey), webSSOSourcePrefix) || len(digest) != stableSSODigestLength {
		return "", false
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return "", false
	}
	return "sso_" + strings.ToLower(digest[:egressDigestLength]), true
}
