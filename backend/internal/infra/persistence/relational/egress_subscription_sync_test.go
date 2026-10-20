package relational

import (
	"context"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/google/uuid"
)

// A sync keeps an administrator's disable and re-enables only the nodes its
// own stale removal disabled.
func TestSubscriptionSyncKeepsAdministratorDisable(t *testing.T) {
	accountStateDatabases(t, func(t *testing.T, database *Database) {
		ctx := context.Background()
		repo := NewEgressRepository(database)
		source, err := repo.CreateEgressSource(ctx, egress.SubscriptionSource{Name: "sync-" + uuid.NewString(), Scope: egress.ScopeWeb, Enabled: true, EncryptedURL: "x", RefreshIntervalSeconds: 3600})
		if err != nil {
			t.Fatal(err)
		}
		sync := func(keys ...string) {
			t.Helper()
			nodes := make([]egress.Node, 0, len(keys))
			for _, key := range keys {
				nodes = append(nodes, egress.Node{Name: "sub-" + key, Scope: egress.ScopeWeb, Enabled: true, SourceID: source.ID, SourceKey: key, EncryptedProxyURL: "proxy-" + key, Health: 1})
			}
			if _, err := repo.UpsertEgressNodesFromSource(ctx, source.ID, nodes); err != nil {
				t.Fatal(err)
			}
		}
		sync("kept", "returning", "dropped")
		listed, err := repo.ListEgressNodes(ctx, egress.ScopeWeb, repository.SortQuery{})
		if err != nil {
			t.Fatal(err)
		}
		ids := make(map[string]uint64)
		for _, node := range listed {
			if node.SourceID == source.ID {
				ids[node.SourceKey] = node.ID
			}
		}
		// The administrator disables one node the subscription keeps and one it drops.
		if _, err := repo.UpdateEgressNodesEnabled(ctx, []uint64{ids["kept"], ids["dropped"]}, false); err != nil {
			t.Fatal(err)
		}
		sync("kept")
		sync("kept", "returning", "dropped")
		for key, want := range map[string]bool{"kept": false, "returning": true, "dropped": false} {
			node, err := repo.GetEgressNode(ctx, ids[key])
			if err != nil {
				t.Fatal(err)
			}
			if node.Enabled != want || node.ProbeError != "" {
				t.Errorf("%s: enabled=%v probeError=%q, want enabled=%v and no stale marker", key, node.Enabled, node.ProbeError, want)
			}
		}
	})
}
