package relational

import (
	"context"
	"errors"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestExtensionFallbacksPersistBesideGrokColumns(t *testing.T) {
	dialectDatabases(t, func(t *testing.T, db *Database) {
		ctx := context.Background()
		r := NewEgressRepository(db)
		gpt, err := r.CreateEgressNode(ctx, egress.Node{Name: "gpt-backup", Scope: "chatgpt", Enabled: true, EncryptedProxyURL: testEncryptedToken})
		if err != nil {
			t.Fatal(err)
		}
		grok, err := r.CreateEgressNode(ctx, egress.Node{Name: "grok", Scope: egress.ScopeWeb, Enabled: true, EncryptedProxyURL: testEncryptedToken})
		if err != nil {
			t.Fatal(err)
		}
		config := egress.DefaultOperationsConfig()
		config.Fallbacks = map[egress.Scope]egress.FallbackConfig{"chatgpt": {Mode: egress.FallbackModeFixed, NodeID: grok.ID}}
		if _, err := r.SaveEgressOperationsConfig(ctx, config); !errors.Is(err, repository.ErrEgressFallbackInUse) {
			t.Fatalf("a Grok node accepted as ChatGPT fallback: %v", err)
		}
		config.Fallbacks = map[egress.Scope]egress.FallbackConfig{
			"chatgpt":       {Mode: egress.FallbackModeFixed, NodeID: gpt.ID},
			egress.ScopeWeb: {Mode: egress.FallbackModeDirect},
		}
		if _, err := r.SaveEgressOperationsConfig(ctx, config); err != nil {
			t.Fatal(err)
		}
		loaded, err := r.GetEgressOperationsConfig(ctx)
		if err != nil || loaded.FallbackFor("chatgpt") != (egress.FallbackConfig{Mode: egress.FallbackModeFixed, NodeID: gpt.ID}) || loaded.FallbackFor(egress.ScopeWeb).Mode != egress.FallbackModeDirect {
			t.Fatalf("fallbacks not round-tripped: %+v %v", loaded.Fallbacks, err)
		}
		if _, err := r.UpdateEgressNodesEnabled(ctx, []uint64{gpt.ID}, false); !errors.Is(err, repository.ErrEgressFallbackInUse) {
			t.Fatalf("disabled a node in use as fallback: %v", err)
		}
		if err := r.DeleteEgressNode(ctx, gpt.ID); err != nil {
			t.Fatal(err)
		}
		if loaded, _ = r.GetEgressOperationsConfig(ctx); loaded.FallbackFor("chatgpt").Mode != egress.FallbackModeNone {
			t.Fatalf("deleted node still referenced: %+v", loaded.FallbackFor("chatgpt"))
		}
	})
}
