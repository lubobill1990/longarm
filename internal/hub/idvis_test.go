package hub

// Node IDs together with "What admins see" (docs/security.md): targets
// resolve among the nodes the caller may see only, renames never take a
// user's name, and IDs never leak hidden nodes.

import (
	"crypto/x509"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/tenancy"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

const (
	bobWorkerID  = "abcd1111-0000-4000-8000-000000000001"
	rootWorkerID = "abcd2222-0000-4000-8000-000000000002"
)

// idHub is ownedHub plus bob's private worker "bw" and root's own worker
// "rw", whose IDs share the prefix "abcd".
func idHub(t *testing.T) *tenancyHub {
	t.Helper()
	th, _ := ownedHub(t)
	h := th.h
	h.mu.Lock()
	for _, r := range []*store.Node{
		{ID: bobWorkerID, Name: "bw", Owner: "bob"},
		{ID: rootWorkerID, Name: "rw", Owner: "root"},
	} {
		r.Role, r.State, r.AccessVersion = "worker", stateApproved, 1
		if err := h.store.PutNode(r); err != nil {
			t.Fatal(err)
		}
		h.putTestNode(&node{rec: r})
	}
	h.mu.Unlock()
	return th
}

// sameAnswer: err (for target) and want (for an unknown target other) are
// the same code and, but for the target itself, the same message.
func sameAnswer(t *testing.T, what string, err error, target string, want error, other string) {
	t.Helper()
	norm := func(m, tgt string) string {
		m = strings.ReplaceAll(m, tgt, "T")
		return strings.ReplaceAll(m, strings.TrimPrefix(tgt, "id:"), "T")
	}
	got := norm(status.Convert(err).Message(), target)
	exp := norm(status.Convert(want).Message(), other)
	if status.Code(err) != status.Code(want) || got != exp || status.Code(err) == codes.OK {
		t.Errorf("%s: %v / unknown: %v", what, err, want)
	}
	for _, leak := range []string{"bob", bobWorkerID, "abcd1111"} {
		if !strings.Contains(target, leak) && strings.Contains(status.Convert(err).Message(), leak) {
			t.Errorf("%s: names %q: %v", what, leak, err)
		}
	}
}

// TestTargetsNeverRevealHiddenNodes: an admin (multi-tenancy) targeting
// another user's private worker - by name, full ID, ID prefix, id:prefix -
// gets exactly the answer of an unknown target, in every API that takes a
// target; an ID prefix shared with a hidden node is not ambiguous.
func TestTargetsNeverRevealHiddenNodes(t *testing.T) {
	th := idHub(t)
	ctx := tctx(t)
	adm := pb.NewHubAdminClient(th.root)
	fleet := pb.NewFleetClient(th.root)
	for _, p := range [][2]string{
		{"bw", "nosuch"},
		{bobWorkerID, "eeee1111-0000-4000-8000-000000000001"},
		{"abcd1111", "eeee1111"},
		{"id:abcd11", "id:eeee11"},
	} {
		_, e1 := adm.SetDrain(ctx, &pb.SetDrainRequest{Name: p[0], Draining: true})
		_, e2 := adm.SetDrain(ctx, &pb.SetDrainRequest{Name: p[1], Draining: true})
		sameAnswer(t, "drain "+p[0], e1, p[0], e2, p[1])
		_, e1 = adm.RenameNode(ctx, &pb.RenameNodeRequest{Target: p[0], NewName: "x1"})
		_, e2 = adm.RenameNode(ctx, &pb.RenameNodeRequest{Target: p[1], NewName: "x1"})
		sameAnswer(t, "rename "+p[0], e1, p[0], e2, p[1])
		_, e1 = fleet.RemoveWorker(ctx, &pb.NodeRequest{Name: p[0]})
		_, e2 = fleet.RemoveWorker(ctx, &pb.NodeRequest{Name: p[1]})
		sameAnswer(t, "remove "+p[0], e1, p[0], e2, p[1])
		_, e1 = fleet.GetJob(ctx, &pb.FleetJobRequest{Id: "J0", Worker: p[0]})
		_, e2 = fleet.GetJob(ctx, &pb.FleetJobRequest{Id: "J0", Worker: p[1]})
		sameAnswer(t, "job on "+p[0], e1, p[0], e2, p[1])
		_, e1 = adm.SubmitJob(ctx, &pb.HubSubmitRequest{Worker: p[0], Argv: []string{"true"}})
		_, e2 = adm.SubmitJob(ctx, &pb.HubSubmitRequest{Worker: p[1], Argv: []string{"true"}})
		sameAnswer(t, "submit to "+p[0], e1, p[0], e2, p[1])
		// the audit log of a hidden machine: nothing, nothing counted
		a, err := fleet.Audit(ctx, &pb.AuditRequest{Node: p[0]})
		if err != nil || len(a.Records) != 0 || a.HiddenTotal != 0 {
			t.Errorf("audit of %s: %v %v", p[0], a, err)
		}
	}
	// "abcd" is a prefix of both IDs: the admin's own worker, no ambiguity
	info, err := adm.SetDrain(ctx, &pb.SetDrainRequest{Name: "abcd", Draining: true})
	if err != nil || info.Name != "rw" || info.Id != rootWorkerID {
		t.Fatalf("drain abcd: %v %v", info, err)
	}
	th.h.mu.Lock()
	drained := th.h.nodes[bobWorkerID].rec.AdminDrain
	th.h.mu.Unlock()
	if drained {
		t.Fatal("bob's worker was drained")
	}
	// alice (a user) sees fleet worker w1 and her own aw: a prefix of
	// bob's ID is unknown to her too
	_, err = pb.NewFleetClient(th.alice).RenameWorker(ctx, &pb.RenameNodeRequest{Target: "abcd", NewName: "x2"})
	if status.Code(err) != codes.NotFound || strings.Contains(err.Error(), "bw") {
		t.Fatalf("alice renames by bob's prefix: %v", err)
	}
}

// TestRenameNeverTakesAUsersOrHiddenName: a rename never gives a node a
// Longarm user's name (workers never; devices only their own user's), and
// a name a hidden node has is "taken", nothing more.
func TestRenameNeverTakesAUsersOrHiddenName(t *testing.T) {
	th := idHub(t)
	h := th.h
	ctx := tctx(t)
	alice := pb.NewFleetClient(th.alice)
	for _, name := range []string{"bob", "carol", "alice"} { // alice: also her device's name
		_, err := alice.RenameWorker(ctx, &pb.RenameNodeRequest{Target: "aw", NewName: name})
		if status.Code(err) != codes.AlreadyExists || name != "alice" && !strings.Contains(err.Error(), "a user's name") {
			t.Errorf("worker aw -> %s: %v", name, err)
		}
	}
	// bob's private worker has "bw": taken, without its ID, role or owner
	_, err := alice.RenameWorker(ctx, &pb.RenameNodeRequest{Target: "aw", NewName: "bw"})
	if status.Code(err) != codes.AlreadyExists || strings.Contains(err.Error(), "abcd") || strings.Contains(err.Error(), "worker") {
		t.Fatalf("aw -> bw: %v", err)
	}
	// a visible holder is named (the fleet worker w1)
	if _, err := alice.RenameWorker(ctx, &pb.RenameNodeRequest{Target: "aw", NewName: "w1"}); status.Code(err) != codes.AlreadyExists ||
		!strings.Contains(err.Error(), "w1") {
		t.Fatalf("aw -> w1: %v", err)
	}
	// devices: bob's laptop may carry its own user's name, not another's
	if _, err := pb.NewFleetClient(th.bob).Whoami(ctx, &pb.WhoamiRequest{}); err != nil { // registers it
		t.Fatal(err)
	}
	h.mu.Lock()
	laptop := h.nodeLocked("bob-laptop")
	h.mu.Unlock()
	if laptop == nil {
		t.Fatal("bob-laptop not registered")
	}
	bob := tenancy.Caller{User: "bob"}
	if _, err := h.RenameNode("client/bob-laptop", bob, true, "bob-laptop", "carol"); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("bob's device -> carol: %v", err)
	}
	if info, err := h.RenameNode("client/bob-laptop", bob, true, "bob-laptop", "bob"); err != nil || info.Name != "bob" || info.User != "bob" {
		t.Fatalf("bob's device -> bob: %v %v", info, err)
	}
	// relays and release credentials are nobody's machines: never renamed
	h.mu.Lock()
	rel := &store.Node{Name: "relay-1", Role: "relay", State: stateApproved}
	h.putTestNode(&node{rec: rel})
	h.mu.Unlock()
	if _, err := h.RenameNode("admin/root", tenancy.Caller{Admin: true}, true, "relay-1", "relay-2"); status.Code(err) != codes.NotFound {
		t.Fatalf("relay renamed: %v", err)
	}
}

// TestProjectionHidesIDs: the directory knows nodes by ID and by the name
// in their certificates; hidden workers' IDs are blanked in jobs and free
// text; a revoked record never shadows a live node of the same name.
func TestProjectionHidesIDs(t *testing.T) {
	th := idHub(t)
	h := th.h
	alice := tenancy.Caller{User: "alice"}
	p := h.projector(alice)
	if !p.hidden(bobWorkerID) || p.hidden(rootWorkerID) == false {
		t.Fatalf("hidden by ID: bob's %v, root's %v", p.hidden(bobWorkerID), p.hidden(rootWorkerID))
	}
	if got := p.text(`{"id":"` + bobWorkerID + `"}`); strings.Contains(got, bobWorkerID) {
		t.Fatalf("text keeps a hidden ID: %s", got)
	}
	fj := p.fleetJob(&pb.FleetJob{Worker: "bw", WorkerId: bobWorkerID, Job: &pb.Job{Spec: &pb.JobSpec{Id: "J", Owner: "alice"}}})
	if fj.WorkerId != "" || fj.Worker != hiddenNode {
		t.Fatalf("job on a hidden worker: %v", fj)
	}
	// a revoked "bw" of alice's, saved after bob's live one: bob's stays
	h.indexNode(&store.Node{ID: "0000beef-0000-4000-8000-000000000003", Name: "bw", Role: "worker", State: stateRevoked, Owner: "alice"})
	if !h.projector(alice).hidden("bw") {
		t.Fatal("a revoked record took the name over from bob's live worker")
	}
	// a renamed node answers to its certificate's (old) name until it renews
	h.indexNode(&store.Node{ID: bobWorkerID, Name: "bw2", CertName: "bw", Role: "worker", State: stateApproved, Owner: "bob"})
	if p := h.projector(alice); !p.hidden("bw2") || !p.hidden("bw") {
		t.Fatal("renamed node not hidden by both names")
	}
}

// TestCertWithAnIDAnOlderHubRecorded: a certificate carrying a node ID this
// hub never assigned (issued by a newer longarm admin issue while an older
// hub ran) maps to the node the older hub recorded its serial for - as a
// certificate without an ID would - and renews into that node's ID at once;
// one recorded with its ID, or never recorded, maps to nothing.
func TestCertWithAnIDAnOlderHubRecorded(t *testing.T) {
	th := idHub(t)
	h := th.h
	issue := func(id string) (*x509.Certificate, pki.Identity) {
		certPEM, _, err := th.ca.Issue(pki.IssueOptions{Role: pki.RoleWorker, Name: "rw", ID: id})
		if err != nil {
			t.Fatal(err)
		}
		c, err := pki.ParseCertPEM(certPEM)
		if err != nil {
			t.Fatal(err)
		}
		ident, _ := pki.IdentityFromCert(c)
		return c, ident
	}
	lookup := func(c *x509.Certificate, id pki.Identity) *node {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.nodeForCertLocked(id, c)
	}
	c, id := issue("eeee0000-0000-4000-8000-000000000009")
	if n := lookup(c, id); n != nil {
		t.Fatalf("an unrecorded certificate with an unknown ID mapped to %s", n.rec.Name)
	}
	// recorded by an older hub (it couldn't read the ID: has_id 0)
	if err := h.store.AddCert(store.Cert{Serial: pki.Serial(c), Node: "rw", NodeID: rootWorkerID, NotAfter: c.NotAfter, IssuedAt: c.NotBefore}); err != nil {
		t.Fatal(err)
	}
	n := lookup(c, id)
	if n == nil || n.rec.ID != rootWorkerID {
		t.Fatalf("the recorded certificate: %v", n)
	}
	if !idMismatch(n, c) || forcedRenewBefore(n, c) <= time.Until(c.NotAfter) {
		t.Fatal("no renewal into the node's ID")
	}
	// recorded with its ID (by a hub that knows IDs): never remapped
	c2, id2 := issue("eeee0000-0000-4000-8000-00000000000a")
	if err := h.store.AddCert(store.Cert{Serial: pki.Serial(c2), Node: "rw", NodeID: rootWorkerID, NotAfter: c2.NotAfter, IssuedAt: c2.NotBefore, HasID: true}); err != nil {
		t.Fatal(err)
	}
	if n := lookup(c2, id2); n != nil {
		t.Fatalf("a certificate recorded with another ID mapped to %s", n.rec.Name)
	}
}
