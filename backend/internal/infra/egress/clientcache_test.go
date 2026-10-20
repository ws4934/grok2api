package egress

import (
	"context"
	"sync/atomic"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// Accounts that share a proxy node keep their own browser clients instead of
// rebuilding one, and its proxy connection, on every request. Cookies travel
// in request headers, so they do not split the cache.
func TestBrowserClientsOnOneNodeDoNotEvictEachOther(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	proxyURL, err := cipher.Encrypt("http://proxy.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	cookieA, _ := cipher.Encrypt("cf_clearance=a")
	cookieB, _ := cipher.Encrypt("cf_clearance=b")
	const chrome146 = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"
	const chrome144 = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36"
	for _, test := range []struct {
		name    string
		agents  [2]string
		cookies [2]string
		builds  int64
	}{
		{name: "two user agents", agents: [2]string{chrome146, chrome144}, builds: 2},
		{name: "two cookies", agents: [2]string{chrome146, chrome146}, cookies: [2]string{cookieA, cookieB}, builds: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			node := domain.Node{ID: 1, Name: "shared", Scope: domain.ScopeWeb, Enabled: true, Health: 1, EncryptedProxyURL: proxyURL}
			manager := NewManager(egressRepositoryTestStub{nodes: []domain.Node{node}}, cipher)
			var builds atomic.Int64
			manager.newBrowserClient = func(string, string) (*browserClient, error) {
				builds.Add(1)
				return &browserClient{}, nil
			}
			for i := range 20 {
				credential := accountdomain.Credential{ID: uint64(1 + i%2), Provider: accountdomain.ProviderWeb, EncryptedCloudflareCookie: test.cookies[i%2]}
				lease, err := manager.AcquireCredential(WithUserAgent(context.Background(), test.agents[i%2]), domain.ScopeWeb, credential)
				if err != nil {
					t.Fatal(err)
				}
				lease.Release()
			}
			if got := builds.Load(); got != test.builds {
				t.Fatalf("browser clients built for 20 requests = %d, want %d", got, test.builds)
			}
		})
	}
}
