package gateway

import (
	"slices"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestPinnedCandidateIndexMatchesLinearScan(t *testing.T) {
	key := candidateCacheKey{provider: account.ProviderBuild, upstreamModel: "model", includeBotFlagged: true}
	values := []account.RoutingCandidate{
		{Credential: account.Credential{ID: 3}}, {Credential: account.Credential{ID: 1}},
		{Credential: account.Credential{ID: 3}}, {Credential: account.Credential{ID: 2}},
	}
	linear := func(values []account.RoutingCandidate, accountID uint64) (int, bool) {
		for index := range values {
			if values[index].Credential.ID == accountID {
				return index, true
			}
		}
		return 0, false
	}
	selector := &Selector{candidates: map[candidateCacheKey]candidateSnapshot{key: newCandidateSnapshot(values, time.Now().UTC().Add(time.Minute))}}
	check := func(stage string) {
		t.Helper()
		cached := selector.candidates[key].values
		// The cached snapshot uses byAccount; a copy falls back to the scan.
		for _, view := range [][]account.RoutingCandidate{cached, slices.Clone(cached)} {
			for _, accountID := range []uint64{1, 2, 3, 4} {
				gotIndex, gotOK := selector.pinnedCandidateIndex(key, view, accountID)
				wantIndex, wantOK := linear(view, accountID)
				if gotIndex != wantIndex || gotOK != wantOK {
					t.Fatalf("%s: account %d = (%d, %t), want (%d, %t)", stage, accountID, gotIndex, gotOK, wantIndex, wantOK)
				}
			}
		}
	}
	check("snapshot")
	selector.evictCandidate(account.ProviderBuild, 1)
	check("after eviction")
}
