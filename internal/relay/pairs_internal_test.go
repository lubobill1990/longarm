package relay

import (
	"testing"

	"github.com/lubobill1990/longarm/internal/pki"
)

// TestWorkerPairsByNodeID: a worker whose certificate carries a node ID
// matches a pair by that ID only - another node that has (or had, or will
// have) the name never inherits the pair; a certificate without an ID (an
// older worker) matches by name; a pair from an older hub (no IDs) is
// matched by names alone.
func TestWorkerPairsByNodeID(t *testing.T) {
	const idA, idB, idX = "aaaa0000-0000-4000-8000-000000000001", "bbbb0000-0000-4000-8000-000000000002", "cccc0000-0000-4000-8000-000000000003"
	w := func(name, id string) pki.Identity { return pki.Identity{Role: pki.RoleWorker, Name: name, ID: id} }
	s := &Server{}
	s.SetWorkerPairs([]WorkerPair{{A: "wa", B: "wb", AID: idA, BID: idB}})
	for _, c := range []struct {
		a, b pki.Identity
		want bool
	}{
		{w("wa", idA), w("wb", idB), true},
		{w("wb", idB), w("wa", idA), true},
		{w("wa", ""), w("wb", idB), true},                                   // wa's legacy certificate (no ID)
		{w("wa", ""), w("wb", ""), true},                                    // both legacy
		{w("wa", idX), w("wb", idB), false},                                 // another node named wa
		{w("renamed", idA), w("wb", idB), true},                             // wa renamed: its ID still matches
		{w("wb", idB), w("wc", ""), false},                                  // not paired
		{w("wa", idA), w("wa", idA), false},                                 // itself
		{w("wa", idA), pki.Identity{Role: pki.RoleClient, Name: "c"}, true}, // client <-> worker: always
	} {
		if got := s.allowed(c.a, c.b); got != c.want {
			t.Errorf("%s/%s <-> %s/%s: %v, want %v", c.a.Name, c.a.ID, c.b.Name, c.b.ID, got, c.want)
		}
	}
	// an older hub's pairs: names only
	s.SetWorkerPairs([]WorkerPair{{A: "wa", B: "wb"}})
	if !s.allowed(w("wa", ""), w("wb", "")) {
		t.Error("legacy pair by name refused")
	}
	if s.allowed(w("wa", idA), w("wb", idB)) {
		t.Error("certificates with IDs matched a pair without IDs")
	}
}
