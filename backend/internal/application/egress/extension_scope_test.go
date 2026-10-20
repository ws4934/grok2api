package egress

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestExtensionScopesAcceptNodesAndFallbacks(t *testing.T) {
	service := &Service{}
	input := Input{Name: "gpt", Scope: "chatgpt", Enabled: true}
	if _, err := service.applyInput(domain.Node{}, input, true); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unregistered scope accepted: %v", err)
	}
	domain.SetExtensionScopes([]domain.Scope{"chatgpt"})
	t.Cleanup(func() { domain.SetExtensionScopes(nil) })
	if _, err := service.applyInput(domain.Node{}, input, true); err != nil {
		t.Fatalf("registered extension scope rejected: %v", err)
	}
	if err := validateImportInput(ImportInput{Name: "gpt", Scope: "chatgpt", Content: "http://proxy:8080"}); err != nil {
		t.Fatalf("extension scope import rejected: %v", err)
	}
	fallbacks, err := service.validateFallbacks(context.Background(), domain.DefaultOperationsConfig(), map[domain.Scope]FallbackConfigInput{"chatgpt": {Mode: domain.FallbackModeDirect}})
	if err != nil || fallbacks["chatgpt"].Mode != domain.FallbackModeDirect || fallbacks[domain.ScopeWeb].Mode == "" {
		t.Fatalf("extension fallback: %+v %v", fallbacks, err)
	}
	if _, err := service.validateFallbacks(context.Background(), domain.DefaultOperationsConfig(), map[domain.Scope]FallbackConfigInput{"unregistered": {Mode: domain.FallbackModeDirect}}); err == nil {
		t.Fatal("unregistered scope fallback accepted")
	}
}

// The fixed fallback node of an extension scope stays enabled like a Grok
// scope's; a disabled fallback would only fail at runtime.
func TestExtensionScopeFixedFallbackNodeCannotBeDisabled(t *testing.T) {
	domain.SetExtensionScopes([]domain.Scope{"chatgpt"})
	t.Cleanup(func() { domain.SetExtensionScopes(nil) })
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "egress.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(relational.NewEgressRepository(database), cipher, "")
	proxy := "http://fallback:8080"
	node, err := service.Create(ctx, Input{Name: "gpt-fallback", Scope: "chatgpt", Enabled: true, ProxyURL: &proxy})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.UpdateOperationsConfig(ctx, OperationsConfigInput{ProbeIntervalSeconds: 300, AssignmentIntervalSeconds: 300,
		Fallbacks: map[domain.Scope]FallbackConfigInput{"chatgpt": {Mode: domain.FallbackModeFixed, NodeID: node.ID}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Update(ctx, node.ID, Input{Name: node.Name, Scope: "chatgpt", Enabled: false}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("edit disabling the fixed fallback: err = %v", err)
	}
	if _, err := service.UpdateManyEnabled(ctx, []uint64{node.ID}, false); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("batch disabling the fixed fallback: err = %v", err)
	}
}

func TestExtensionAccountsBindOnlyToTheirOwnScope(t *testing.T) {
	domain.SetExtensionScopes([]domain.Scope{"chatgpt"})
	t.Cleanup(func() { domain.SetExtensionScopes(nil) })
	for _, tc := range []struct {
		scope    domain.Scope
		provider accountdomain.Provider
		want     bool
	}{
		{"chatgpt", "chatgpt", true},
		{domain.ScopeWeb, "chatgpt", false},
		{"chatgpt", accountdomain.ProviderWeb, false},
	} {
		if got := scopeSupportsProvider(tc.scope, tc.provider); got != tc.want {
			t.Errorf("scope %s provider %s: got %v", tc.scope, tc.provider, got)
		}
	}
}
