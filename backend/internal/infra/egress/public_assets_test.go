package egress

import (
	"context"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestPublicConsoleAssetsUseOnlyExplicitResourceEgress(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := cipher.Encrypt("http://127.0.0.1:18080")
	if err != nil {
		t.Fatal(err)
	}
	api := domain.Node{ID: 1, Name: "API", Scope: domain.ScopeConsole, Enabled: true, Health: 1, EncryptedProxyURL: proxy}
	web := domain.Node{ID: 2, Name: "Web", Scope: domain.ScopeWeb, Enabled: true, Health: 1, EncryptedProxyURL: proxy}
	asset := domain.Node{ID: 3, Name: "assets", Scope: domain.ScopeConsoleAsset, Enabled: true, Health: 1, EncryptedProxyURL: proxy}
	cooling := asset
	until := time.Now().Add(time.Hour)
	cooling.CooldownUntil = &until
	for _, tc := range []struct {
		name       string
		nodes      []domain.Node
		fallback   domain.FallbackConfig
		wantNode   uint64
		configured bool
		wantErr    bool
	}{
		{name: "API and Web bindings do not proxy resources", nodes: []domain.Node{api, web}},
		{name: "dedicated resource node", nodes: []domain.Node{api, web, asset}, wantNode: 3, configured: true},
		{name: "explicit resource fallback", nodes: []domain.Node{api, web}, fallback: domain.FallbackConfig{Mode: domain.FallbackModeFixed, NodeID: 1}, wantNode: 1, configured: true},
		{name: "explicit direct fallback", nodes: []domain.Node{api}, fallback: domain.FallbackConfig{Mode: domain.FallbackModeDirect}},
		{name: "unavailable resource node does not inherit API", nodes: []domain.Node{api, cooling}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := domain.DefaultOperationsConfig()
			config.Fallbacks[domain.ScopeConsoleAsset] = tc.fallback
			config.Fallbacks[domain.ScopeConsole] = domain.FallbackConfig{Mode: domain.FallbackModeFixed, NodeID: 2}
			manager := NewManager(fallbackEgressRepository{egressRepositoryTestStub: egressRepositoryTestStub{nodes: tc.nodes}, config: config}, cipher)
			// Both an existing account binding and account context must be ignored.
			ctx, trace := WithTrace(WithEgressNode(WithAccount(context.Background(), "grok_console", 99), 1))
			lease, configured, err := manager.AcquireConsoleAssetIfConfigured(ctx, "99")
			if tc.wantErr {
				if err == nil || lease != nil {
					t.Fatalf("unavailable resource egress = %+v, %v", lease, err)
				}
				return
			}
			if err != nil || configured != tc.configured {
				t.Fatalf("configured=%v err=%v", configured, err)
			}
			if !configured {
				if lease != nil {
					t.Fatalf("implicit proxy lease = %+v", lease)
				}
			} else {
				defer lease.Release()
				if lease.NodeID != tc.wantNode || (tc.wantNode > 0 && lease.ProxyURL != "http://127.0.0.1:18080") {
					t.Fatalf("resource lease = %+v", lease)
				}
			}
			selection, ok := trace.Selection(domain.ScopeConsoleAsset)
			if !ok || selection.Scope != domain.ScopeConsoleAsset || selection.NodeID != tc.wantNode {
				t.Fatalf("resource trace = %+v", selection)
			}
		})
	}
}
