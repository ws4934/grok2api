package egress

import (
	"context"
	"errors"
	"testing"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

func TestExtensionScopesAcceptNodesButNotFallbacks(t *testing.T) {
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
	// Fallback nodes are stored per Grok scope; an extension key must not be dropped silently.
	if _, err := service.validateFallbacks(context.Background(), domain.DefaultOperationsConfig(), map[domain.Scope]FallbackConfigInput{"chatgpt": {Mode: domain.FallbackModeNone}}); err == nil {
		t.Fatal("extension scope fallback accepted")
	}
}
