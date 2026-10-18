package egress

import (
	"context"
	"errors"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
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
