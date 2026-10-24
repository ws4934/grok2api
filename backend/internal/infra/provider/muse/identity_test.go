package muse

import (
	"reflect"
	"strings"
	"testing"
)

// Chrome 155 (the bundled headless shell) reports Chrome, Chromium and
// Not(A:Brand 24 in this order; Google Chrome takes the Chrome slot.
func TestChromeBrandsMatchTheBrowser(t *testing.T) {
	want := []map[string]string{{"brand": "Google Chrome", "version": "155"}, {"brand": "Chromium", "version": "155"}, {"brand": "Not(A:Brand", "version": "24"}}
	if got := chromeBrands("155", "155"); !reflect.DeepEqual(got, want) {
		t.Fatalf("brands = %v", got)
	}
	full := chromeBrands("155", "155.0.8059.40")
	if full[0]["version"] != "155.0.8059.40" || full[2]["version"] != "24.0.0.0" {
		t.Fatalf("full version list = %v", full)
	}
	if got := identityMust(t, "linux").clientHints("155.0.8059.40")["Sec-Ch-Ua"]; got != `"Google Chrome";v="155", "Chromium";v="155", "Not(A:Brand";v="24"` {
		t.Fatalf("Sec-Ch-Ua = %s", got)
	}
}

func TestIdentityOverrideAgrees(t *testing.T) {
	keys := map[string]bool{}
	for _, identity := range browserIdentities {
		if keys[identity.Key] {
			t.Fatalf("duplicate identity %s", identity.Key)
		}
		keys[identity.Key] = true
		override := identity.override("155.0.8059.40")
		ua := override["userAgent"].(string)
		metadata := override["userAgentMetadata"].(map[string]any)
		if !strings.Contains(ua, "("+identity.token+")") || !strings.Contains(ua, "Chrome/155.0.0.0 ") || metadata["fullVersion"] != "155.0.8059.40" {
			t.Fatalf("%s: ua = %q, metadata = %v", identity.Key, ua, metadata)
		}
		if metadata["platform"] != identity.clientHints("155.0.8059.40")["Sec-Ch-Ua-Platform"][1:len(identity.uaPlatform)+1] {
			t.Fatalf("%s: client hint platform disagrees with metadata", identity.Key)
		}
	}
}

func identityMust(t *testing.T, key string) browserIdentity {
	t.Helper()
	identity, ok := identityByKey(key)
	if !ok {
		t.Fatalf("no identity %s", key)
	}
	return identity
}
