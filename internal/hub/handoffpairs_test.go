package hub

import (
	"context"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/lubobill1990/longarm/internal/hub/store"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// TestHandoffPair: a handoff pair lets the relays carry traffic between two
// workers of the caller's own (or fleet workers it may use) until it
// expires or is released; another user's worker - shared with the caller
// or not, the caller an admin or not - is NotFound, word for word the
// unknown-name answer.
func TestHandoffPair(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	h.mu.Lock()
	for _, n := range []*store.Node{
		{Name: "aw1", Owner: "alice"}, {Name: "aw2", Owner: "alice"},
		{Name: "bw", Owner: "bob", Shares: &store.WorkerShares{Users: []string{"alice", "root"}}},
		{Name: "fw"}, // fleet
	} {
		n.Role, n.State = "worker", stateApproved
		h.putTestNode(&node{rec: n})
	}
	h.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pair := func(c *grpc.ClientConn, a, b string, ttl time.Duration, release bool) error {
		_, err := pb.NewFleetClient(c).HandoffPair(ctx, &pb.HandoffPairRequest{A: a, B: b, Ttl: durationpb.New(ttl), Release: release})
		return err
	}
	pairs := func() [][2]string {
		h.mu.Lock()
		defer h.mu.Unlock()
		var out [][2]string
		for _, p := range h.relayPolicyLocked().WorkerPairs {
			out = append(out, [2]string{p.A, p.B})
		}
		return out
	}

	if err := pair(th.alice, "aw2", "aw1", time.Minute, false); err != nil {
		t.Fatalf("alice's own workers: %v", err)
	}
	if got := pairs(); !slices.Equal(got, [][2]string{{"aw1", "aw2"}}) {
		t.Fatalf("relay policy pairs: %v", got)
	}
	if err := pair(th.alice, "aw1", "fw", time.Minute, false); err != nil {
		t.Fatalf("alice's worker and a fleet worker: %v", err)
	}
	for name, c := range map[string]struct {
		conn       *grpc.ClientConn
		a, b, want string // want: the worker named in the NotFound
	}{
		"alice: bob's worker shared with her": {th.alice, "aw1", "bw", "bw"},
		"bob: alice's workers":                {th.bob, "aw1", "aw2", "aw1"},
		"bob: his worker and alice's":         {th.bob, "bw", "aw1", "aw1"},
		"root (admin): alice's workers":       {th.root, "aw1", "aw2", "aw1"},
		"root (admin): bob's shared worker":   {th.root, "bw", "fw", "bw"},
		"alice: an unknown worker":            {th.alice, "aw1", "nope", "nope"},
	} {
		err := pair(c.conn, c.a, c.b, time.Minute, false)
		if want := `no worker "` + c.want + `"`; status.Code(err) != codes.NotFound || status.Convert(err).Message() != want {
			t.Errorf("%s: %v (want NotFound %q)", name, err, want)
		}
	}
	if err := pair(th.alice, "aw1", "aw1", time.Minute, false); status.Code(err) != codes.InvalidArgument {
		t.Errorf("one worker twice: %v", err)
	}
	if err := pair(th.alice, "aw1", "aw2", time.Hour, false); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a ttl over the limit: %v", err)
	}
	// bob can't release alice's pair
	if err := pair(th.bob, "aw1", "aw2", 0, true); status.Code(err) != codes.NotFound {
		t.Errorf("bob releases alice's pair: %v", err)
	}
	if err := pair(th.alice, "aw1", "aw2", 0, true); err != nil {
		t.Fatalf("release: %v", err)
	}
	if got := pairs(); !slices.Equal(got, [][2]string{{"aw1", "fw"}}) {
		t.Fatalf("after the release: %v", got)
	}
	// expiry: out of the policy at once, out of the map with the sweep
	if err := pair(th.alice, "aw1", "aw2", 50*time.Millisecond, false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := pairs(); !slices.Equal(got, [][2]string{{"aw1", "fw"}}) {
		t.Fatalf("an expired pair in the policy: %v", got)
	}
	h.mu.Lock()
	h.sweepHandoffPairsLocked(time.Now())
	n := len(h.handoffPairs)
	h.mu.Unlock()
	if n != 1 {
		t.Fatalf("after the sweep: %d pairs", n)
	}
	// the quota
	old := handoffPairPerUser
	handoffPairPerUser = 1
	defer func() { handoffPairPerUser = old }()
	if err := pair(th.alice, "aw2", "fw", time.Minute, false); status.Code(err) != codes.ResourceExhausted {
		t.Errorf("over the quota: %v", err)
	}
	if err := pair(th.alice, "aw1", "fw", time.Minute, false); err != nil {
		t.Errorf("renewing an existing pair over the quota: %v", err)
	}
}
