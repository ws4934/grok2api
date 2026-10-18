package egress

import (
	"context"
	"fmt"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// Logins without an account row (ID 0) used to hash onto the node of
// account 0 together; with their own identity they spread like accounts do
// and stay on one node per login.
func TestPendingCredentialNodeFollowsItsIdentity(t *testing.T) {
	domain.SetExtensionScopes([]domain.Scope{"chatgpt"})
	t.Cleanup(func() { domain.SetExtensionScopes(nil) })
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	var nodes []domain.Node
	for id := uint64(1); id <= 8; id++ {
		proxyURL, err := cipher.Encrypt(fmt.Sprintf("http://node-%d:8080", id))
		if err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, domain.Node{ID: id, Name: fmt.Sprintf("node-%d", id), Scope: "chatgpt", Enabled: true, Health: 1, EncryptedProxyURL: proxyURL})
	}
	manager := NewManager(egressRepositoryTestStub{nodes: nodes}, cipher)
	acquire := func(identity string) uint64 {
		t.Helper()
		lease, err := manager.AcquireCredential(context.Background(), "chatgpt", accountdomain.Credential{Provider: "chatgpt", EgressIdentity: identity})
		if err != nil {
			t.Fatal(err)
		}
		lease.Release()
		return lease.NodeID
	}
	used := map[uint64]bool{}
	for index := range 8 {
		identity := fmt.Sprintf("chatgpt_pending_%d", index)
		node := acquire(identity)
		if again := acquire(identity); again != node {
			t.Fatalf("%s moved from node %d to %d", identity, node, again)
		}
		used[node] = true
	}
	if len(used) < 2 {
		t.Fatalf("eight pending logins shared nodes %v", used)
	}
}
