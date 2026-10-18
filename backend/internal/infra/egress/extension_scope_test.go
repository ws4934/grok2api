package egress

import (
	"context"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestExtensionScopeLeasesItsOwnProxyNodes(t *testing.T) {
	domain.SetExtensionScopes([]domain.Scope{"chatgpt"})
	t.Cleanup(func() { domain.SetExtensionScopes(nil) })
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	proxyURL, err := cipher.Encrypt("socks5h://Default.{account}:token@resin:2260")
	if err != nil {
		t.Fatal(err)
	}
	nodes := []domain.Node{
		{ID: 1, Name: "grok", Scope: domain.ScopeWeb, Enabled: true, Health: 1, EncryptedProxyURL: proxyURL},
		{ID: 2, Name: "gpt", Scope: "chatgpt", Enabled: true, Health: 1, EncryptedProxyURL: proxyURL},
	}
	manager := NewManager(egressRepositoryTestStub{nodes: nodes}, cipher)
	lease, err := manager.AcquireCredential(context.Background(), "chatgpt", accountdomain.Credential{ID: 7, Provider: "chatgpt"})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.NodeID != 2 || lease.ProxyURL != "socks5h://Default.chatgpt_7:token@resin:2260" {
		t.Fatalf("extension lease node=%d proxy=%q", lease.NodeID, lease.ProxyURL)
	}

	// Without a node of its own the channel connects directly, never through Grok nodes.
	direct, err := NewManager(egressRepositoryTestStub{nodes: nodes[:1]}, cipher).AcquireCredential(context.Background(), "chatgpt", accountdomain.Credential{ID: 7, Provider: "chatgpt"})
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Release()
	if direct.ProxyURL != "" {
		t.Fatalf("extension scope borrowed another scope's proxy: %q", direct.ProxyURL)
	}
}
