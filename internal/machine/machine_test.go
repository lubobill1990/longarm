package machine

import (
	"testing"

	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

func node(name, owner, host string, admin bool) *pb.NodeInfo {
	n := &pb.NodeInfo{Name: name, Role: "worker", State: "approved", Owner: owner, Hostname: host, AdminWorker: admin}
	return n
}

// TestPairs: an admin worker pairs with its session worker - by the hub's
// companion, else by <worker>-admin, the same owner and the same host - and
// never with a stranger's worker, another host's, a revoked one or twice.
func TestPairs(t *testing.T) {
	explicit := node("cpc-renamed-admin", "bob", "CPC-X", true)
	explicit.Companion = "cpc-x"
	revoked := node("old", "alice", "old", false)
	revoked.State = "revoked"
	nodes := []*pb.NodeInfo{
		node("cpc-alice-x7k2q", "alice", "cpc-alice-x7k2q.example.net", false),
		node("cpc-alice-x7k2q-admin", "alice", "CPC-ALICE-X7K2Q", true),
		node("cpc-x", "bob", "cpc-x", false),
		explicit,
		node("shared", "", "shared", false), // a fleet worker
		node("shared-admin", "alice", "shared", true),
		node("dev", "alice", "dev", false),
		node("dev-admin", "alice", "other-host", true),
		revoked,
		node("old-admin", "alice", "old", true),
		node("solo-admin", "alice", "solo", true), // an admin worker alone
		{Name: "carol-laptop-admin", Role: "client", State: "approved", Owner: "carol"},
	}
	got := Pairs(nodes)
	want := map[string]string{"cpc-alice-x7k2q-admin": "cpc-alice-x7k2q", "cpc-renamed-admin": "cpc-x"}
	if len(got) != len(want) {
		t.Fatalf("pairs %v, want %v", got, want)
	}
	for a, s := range want {
		if got[a] != s {
			t.Errorf("pairs[%s] = %q, want %q (all: %v)", a, got[a], s, got)
		}
	}
	// the admin label alone marks an admin worker (an older hub)
	byLabel := node("box-admin", "eve", "", false)
	byLabel.Labels = map[string]string{"longarm.io/admin": "true"}
	if p := Pairs([]*pb.NodeInfo{node("box", "eve", "box", false), byLabel}); p["box-admin"] != "box" {
		t.Errorf("labelled admin worker: %v", p)
	}
	if Candidate(Worker{Name: "-admin", Admin: true}) != "" || Candidate(Worker{Name: "w"}) != "w-admin" {
		t.Error("Candidate")
	}
}
