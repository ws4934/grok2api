package egress

import (
	"context"
	"strings"
	"testing"
	"time"

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
	if len(manager.clients) != 0 {
		t.Fatal("an extension lease built and cached a Grok browser client")
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

func TestExtensionScopeFallsBackToItsFixedNode(t *testing.T) {
	domain.SetExtensionScopes([]domain.Scope{"chatgpt"})
	t.Cleanup(func() { domain.SetExtensionScopes(nil) })
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	primary, err := cipher.Encrypt("http://primary:8080")
	if err != nil {
		t.Fatal(err)
	}
	backup, err := cipher.Encrypt("http://backup:8080")
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Minute)
	config := domain.DefaultOperationsConfig()
	config.Fallbacks["chatgpt"] = domain.FallbackConfig{Mode: domain.FallbackModeFixed, NodeID: 2}
	manager := NewManager(fallbackEgressRepository{
		egressRepositoryTestStub: egressRepositoryTestStub{nodes: []domain.Node{
			{ID: 1, Name: "cooling", Scope: "chatgpt", Enabled: true, Health: 1, CooldownUntil: &until, EncryptedProxyURL: primary},
			{ID: 2, Name: "backup", Scope: "chatgpt", Enabled: true, Health: 1, EncryptedProxyURL: backup},
		}},
		config: config,
	}, cipher)
	lease, err := manager.AcquireCredential(context.Background(), "chatgpt", accountdomain.Credential{ID: 7, Provider: "chatgpt"})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.NodeID != 2 || lease.ProxyURL != "http://backup:8080" {
		t.Fatalf("fallback lease node=%d proxy=%q", lease.NodeID, lease.ProxyURL)
	}
	// While the primary is healthy the reserved backup carries no traffic.
	healthy := NewManager(fallbackEgressRepository{
		egressRepositoryTestStub: egressRepositoryTestStub{nodes: []domain.Node{
			{ID: 1, Name: "primary", Scope: "chatgpt", Enabled: true, Health: 1, EncryptedProxyURL: primary},
			{ID: 2, Name: "backup", Scope: "chatgpt", Enabled: true, Health: 1, EncryptedProxyURL: backup},
		}},
		config: config,
	}, cipher)
	for id := uint64(1); id <= 8; id++ {
		lease, err := healthy.AcquireCredential(context.Background(), "chatgpt", accountdomain.Credential{ID: id, Provider: "chatgpt"})
		if err != nil {
			t.Fatal(err)
		}
		lease.Release()
		if lease.NodeID != 1 {
			t.Fatalf("account %d used the reserved backup node", id)
		}
	}
}

type targetRecordingSolver struct{ targets []string }

func (s *targetRecordingSolver) Solve(_ context.Context, cfg ClearanceConfig, _ string) (clearanceSolution, error) {
	s.targets = append(s.targets, cfg.TargetURL)
	return clearanceSolution{Cookies: "cf_clearance=solved", UserAgent: "Mozilla/5.0 (X11; Linux x86_64) Chrome/142.0.0.0 Safari/537.36"}, nil
}

func TestExtensionClearanceSolvesOnlyAfterAChallenge(t *testing.T) {
	domain.SetExtensionScopes([]domain.Scope{"chatgpt"})
	t.Cleanup(func() { domain.SetExtensionScopes(nil) })
	solver := &targetRecordingSolver{}
	manager := NewManager(egressRepositoryTestStub{}, nil) // no nodes: direct egress
	manager.solver = solver
	// The Grok mode stays manual; the extension scope still solves on demand.
	manager.UpdateClearanceConfig(ClearanceConfig{Mode: "manual", FlareSolverrURL: "http://solver", TargetURL: "https://grok.com", Timeout: time.Second, RefreshInterval: time.Hour})
	acquire := func() *Lease {
		t.Helper()
		lease, err := manager.AcquireCredential(context.Background(), "chatgpt", accountdomain.Credential{ID: 7, Provider: "chatgpt"})
		if err != nil {
			t.Fatal(err)
		}
		lease.Release()
		return lease
	}
	if lease := acquire(); lease.CFCookies != "" || len(solver.targets) != 0 {
		t.Fatal("clearance solved while the scope is not enabled")
	}
	manager.SetExtensionClearance("chatgpt", "https://chatgpt.com/")
	first := acquire()
	if first.CFCookies != "" || len(solver.targets) != 0 {
		t.Fatal("on-demand clearance solved before any challenge")
	}
	first.InvalidateClearance() // the adapter saw a Cloudflare challenge
	solved := acquire()
	if len(solver.targets) != 1 || solver.targets[0] != "https://chatgpt.com" || solved.CFCookies != "cf_clearance=solved" || !strings.Contains(solved.UserAgent, "Linux") {
		t.Fatalf("targets=%v lease cookies=%q ua=%q", solver.targets, solved.CFCookies, solved.UserAgent)
	}
	if acquire(); len(solver.targets) != 1 {
		t.Fatal("a fresh clearance was solved again")
	}
	// A Grok Web direct lease never sees the ChatGPT clearance.
	web, err := manager.Acquire(context.Background(), domain.ScopeWeb, "account")
	if err != nil {
		t.Fatal(err)
	}
	web.Release()
	if web.CFCookies == "cf_clearance=solved" {
		t.Fatal("ChatGPT clearance leaked into the Grok direct key")
	}
}
