package hub

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/nodeid"
	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// putTestNode registers n (h.mu held), giving it an ID if it has none.
func (h *Hub) putTestNode(n *node) {
	if n.rec.ID == "" {
		n.rec.ID = nodeid.New()
	}
	h.nodes[n.rec.ID] = n
}

// TestResolveTargets: names, full IDs, unique prefixes; ambiguity lists
// the candidates; explicit forms; visibility.
func TestResolveTargets(t *testing.T) {
	h := &Hub{nodes: map[string]*node{}}
	add := func(id, name, owner, state string) {
		h.putTestNode(&node{rec: &store.Node{ID: id, Name: name, Role: "worker", Owner: owner, State: state}})
	}
	add("3f2a9c1e-4b5d-4c3e-8f00-1234567890ab", "box", "alice", stateApproved)
	add("3f2a9c1f-0000-4c3e-8f00-1234567890ab", "laptop", "bob", stateApproved)
	add("77aa0000-0000-4000-8000-000000000001", "box", "alice", stateRevoked) // an earlier "box"
	want := func(target, id string) {
		t.Helper()
		n, err := h.resolveLocked(target, nil, "worker")
		if err != nil || n.rec.ID != id {
			t.Errorf("%q: %v %v, want %s", target, n, err, id)
		}
	}
	want("box", "3f2a9c1e-4b5d-4c3e-8f00-1234567890ab") // the active "box", not the revoked one
	want("3f2a9c1e-4b5d-4c3e-8f00-1234567890ab", "3f2a9c1e-4b5d-4c3e-8f00-1234567890ab")
	want("3f2a9c1f", "3f2a9c1f-0000-4c3e-8f00-1234567890ab")
	want("77aa", "77aa0000-0000-4000-8000-000000000001")
	want("id:3f2a9c1e", "3f2a9c1e-4b5d-4c3e-8f00-1234567890ab")
	want("name:laptop", "3f2a9c1f-0000-4c3e-8f00-1234567890ab")

	_, err := h.resolveLocked("3f2a", nil, "worker")
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("ambiguous prefix: %v", err)
	}
	for _, s := range []string{"3f2a9c1e", "3f2a9c1f", "box", "laptop", "alice", "bob", "approved"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("the ambiguity error lacks %q: %v", s, err)
		}
	}
	if _, err := h.resolveLocked("nope", nil, "worker"); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown: %v", err)
	}
	// not visible = not there
	onlyBob := func(n *node) bool { return n.rec.Owner == "bob" }
	if _, err := h.resolveLocked("box", onlyBob, "worker"); status.Code(err) != codes.NotFound {
		t.Fatalf("invisible by name: %v", err)
	}
	if _, err := h.resolveLocked("3f2a9c1e-4b5d-4c3e-8f00-1234567890ab", onlyBob, "worker"); status.Code(err) != codes.NotFound {
		t.Fatalf("invisible by id: %v", err)
	}
	if n, err := h.resolveLocked("3f2a", onlyBob, "worker"); err != nil || n.rec.Name != "laptop" {
		t.Fatalf("a prefix unique among the visible: %v %v", n, err)
	}
}

// TestNameRules: rule 2 at every door - join codes, joins, renames - and
// auto-naming from host names that look like IDs.
func TestNameRules(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	for _, bad := range []string{"cafe", "1234", "dead-beef", nodeid.New()} {
		if _, err := h.CreateJoinCode("test", &pb.CreateJoinCodeRequest{Name: bad}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("join code named %q: %v", bad, err)
		}
		if _, err := h.RenameNode("test", "", true, "w1", bad); status.Code(err) != codes.InvalidArgument {
			t.Errorf("rename to %q: %v", bad, err)
		}
	}
	if got := nodeid.SafeName(sanitizeName("1234.corp.example"), autoNamePrefix("worker")); got != "w-1234" {
		t.Fatalf("auto name %q", got)
	}
	if got := h.freeDeviceName("beef"); got != "d-beef" {
		t.Fatalf("device name of user beef: %q", got)
	}
	// a worker joining from host "abcd" without a name
	jc, err := pb.NewFleetClient(th.alice).CreateWorkerCode(tctx(t), &pb.CreateWorkerCodeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	_, csr, _ := pki.NewKeyAndCSR("", nil)
	resp, err := enrollClient(t, h).Join(tctx(t), &pb.JoinRequest{Code: jc.Code, CsrPem: csr, Hostname: "abcd"})
	if err != nil || resp.Name != "w-abcd" || !nodeid.Valid(resp.Id) {
		t.Fatalf("join from host abcd: %+v %v", resp, err)
	}
	if id, _ := identityOf(t, resp); id.ID != resp.Id || id.Name != "w-abcd" {
		t.Fatalf("certificate identity %+v", id)
	}
}

// TestRenameNode: owners rename their workers, admins any node; the ID
// stays, jobs and lookups follow, collisions are refused, everything is
// audited; the old name stays reserved while the certificate has it.
func TestRenameNode(t *testing.T) {
	th, _ := ownedHub(t)
	h := th.h
	h.mu.Lock()
	aw := h.nodeLocked("aw")
	id := aw.rec.ID
	h.applyJobsLocked(aw, &pb.JobsReport{Jobs: []*pb.Job{{Spec: &pb.JobSpec{Id: "JR", Owner: "alice"}, State: pb.JobState_JOB_STATE_RUNNING}}})
	h.mu.Unlock()

	// bob may not (not his, not even visible)
	if _, err := h.RenameWorker("client/bob", "bob", false, "aw", "x1"); status.Code(err) != codes.NotFound {
		t.Fatalf("bob renames alice's worker: %v", err)
	}
	// taken (w1, a fleet worker, is active), and ID-shaped names
	if _, err := h.RenameWorker("client/alice", "alice", false, "aw", "w1"); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("rename onto w1: %v", err)
	}
	info, err := pb.NewFleetClient(th.alice).RenameWorker(tctx(t), &pb.RenameNodeRequest{Target: nodeid.Short(id), NewName: "aw2"})
	if err != nil || info.Name != "aw2" || info.Id != id || info.CertName != "aw" {
		t.Fatalf("rename by id prefix: %+v %v", info, err)
	}
	// dispatch: by the new name, the ID, a prefix; the old name is gone ...
	for _, target := range []string{"aw2", id, id[:8], "id:" + id[:6]} {
		fj, err := h.GetJob("JR", target)
		if err != nil || fj.WorkerId != id || fj.Worker != "aw2" {
			t.Errorf("job via %q: %+v %v", target, fj, err)
		}
	}
	if _, err := h.GetJob("JR", "aw"); status.Code(err) != codes.NotFound {
		t.Errorf("job via the old name: %v", err)
	}
	// ... but reserved while aw2's certificate says "aw"
	h.mu.Lock()
	taken := h.nameTakenLocked("aw", "")
	h.mu.Unlock()
	if taken == nil || taken.rec.ID != id {
		t.Fatal("the old certificate name is free while the certificate has it")
	}
	// admins rename any node (a device too)
	if info, err := h.RenameNode("admin/root", "root", true, "w1", "fleet-1"); err != nil || info.Name != "fleet-1" {
		t.Fatalf("admin rename: %v %v", info, err)
	}
	rows, _ := h.store.Audit("", "hub", time.Time{}, 100)
	ok := 0
	for _, r := range rows {
		if r.Method == "nodes/rename" && strings.Contains(r.Line, id) && strings.Contains(r.Line, `"new_name":"aw2"`) {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("rename audit: %d records", ok)
	}
	// persisted
	nodes, _ := h.store.Nodes()
	for _, n := range nodes {
		if n.ID == id && (n.Name != "aw2" || n.CertName != "aw") {
			t.Fatalf("stored %+v", n)
		}
	}
}

// TestRevokeThenReuseName: revocation is by ID; the name is free at once
// for a new node (a new ID), whose certificates the old revocation doesn't
// touch, while the old node's ID stays refused.
func TestRevokeThenReuseName(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	join := func() *hubclient.JoinResult {
		t.Helper()
		jc, err := pb.NewFleetClient(th.alice).CreateWorkerCode(tctx(t), &pb.CreateWorkerCodeRequest{Name: "reuse"})
		if err != nil {
			t.Fatal(err)
		}
		r, err := hubclient.Join(tctx(t), h.Addr("grpc"), jc.Code, hubclient.JoinOptions{Fingerprint: pki.Fingerprint(th.ca.Cert)})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	first := join()
	if _, err := pb.NewFleetClient(th.alice).CreateWorkerCode(tctx(t), &pb.CreateWorkerCodeRequest{Name: "reuse"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Revoke("test", "reuse"); err != nil {
		t.Fatal(err)
	}
	second := join() // the name is free again
	if second.Resp.Name != "reuse" || second.Resp.Id == first.Resp.Id || !nodeid.Valid(second.Resp.Id) {
		t.Fatalf("second join %+v (first %s)", second.Resp, first.Resp.Id)
	}
	c1, _ := pki.ParseCertPEM(first.Resp.CertPem)
	c2, _ := pki.ParseCertPEM(second.Resp.CertPem)
	if !h.revoked.Revoked(c1) || h.revoked.Revoked(c2) {
		t.Fatalf("revoked: old %v new %v", h.revoked.Revoked(c1), h.revoked.Revoked(c2))
	}
	if !h.revoked.RevokedID(first.Resp.Id) || h.revoked.RevokedID(second.Resp.Id) {
		t.Fatal("the revocation list's node IDs")
	}
	// the list workers get: the old node's ID, not the name
	msg := h.revocationsMsg().GetRevocations()
	var ids []string
	for _, n := range msg.Nodes {
		ids = append(ids, n.Id)
	}
	if len(ids) != 1 || ids[0] != first.Resp.Id {
		t.Fatalf("revoked nodes %v", ids)
	}
	// "reuse" names the new node; the old one by its ID
	h.mu.Lock()
	byName, _ := h.resolveLocked("reuse", nil, "worker")
	byID, _ := h.resolveLocked(first.Resp.Id[:8], nil, "worker")
	h.mu.Unlock()
	if byName.rec.ID != second.Resp.Id || byID.rec.State != stateRevoked {
		t.Fatal("resolution after the reuse")
	}
	// the new node works; the old certificate does not
	conn := func(r *hubclient.JoinResult) *grpc.ClientConn {
		b := writeTestBundle(t, h, r)
		c, err := grpc.NewClient(h.Addr("grpc"), grpc.WithTransportCredentials(credentials.NewTLS(hubclient.TLSConfig(b))))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	who, err := pb.NewFleetClient(conn(second)).Whoami(tctx(t), &pb.WhoamiRequest{})
	if err != nil || who.Id != second.Resp.Id {
		t.Fatalf("new node whoami: %v %v", who, err)
	}
	if _, err := pb.NewFleetClient(conn(first)).Whoami(tctx(t), &pb.WhoamiRequest{}); err == nil {
		t.Fatal("the revoked node's certificate still works")
	}
}

// TestLegacyCertificates: certificates without an ID (issued before node
// IDs) map to their node by serial - through a rename - and a certificate
// the hub never saw, named like a live node, is not that node.
func TestLegacyCertificates(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	fc := pb.NewFleetClient(th.alice) // "alice": a legacy client certificate, registered on first use
	who, err := fc.Whoami(tctx(t), &pb.WhoamiRequest{})
	if err != nil || !nodeid.Valid(who.Id) || who.Name != "alice" {
		t.Fatalf("whoami: %v %v", who, err)
	}
	if _, err := h.RenameNode("admin/root", "root", true, "alice", "alice-pc"); err != nil {
		t.Fatal(err)
	}
	who2, err := fc.Whoami(tctx(t), &pb.WhoamiRequest{})
	if err != nil || who2.Id != who.Id || who2.User != "alice" {
		t.Fatalf("whoami after the rename: %v %v (the device keeps its user)", who2, err)
	}
	// its renewal now carries the ID and the new name; the user stays alice
	h.mu.Lock()
	n := h.nodes[who.Id]
	rec := *n.rec
	h.mu.Unlock()
	if rec.User != "alice" {
		t.Fatalf("the renamed device's user: %q", rec.User)
	}
	_, csr, _ := pki.NewKeyAndCSR("", nil)
	certPEM, err := h.issue(&rec, csr)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := pki.ParseCertPEM(certPEM)
	id, _ := pki.IdentityFromCert(leaf)
	if id.ID != who.Id || id.Name != "alice-pc" || id.Owner() != "alice" {
		t.Fatalf("renewed identity %+v", id)
	}
	// a hand-issued certificate the hub never saw, named "alice": refused
	// (the renamed device's certificate still has that name)
	c := dialAs(t, th, pki.IssueOptions{Role: pki.RoleClient, Name: "alice"})
	if _, err := pb.NewFleetClient(c).Whoami(tctx(t), &pb.WhoamiRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("an unknown certificate named like a live node: %v", err)
	}
}

func dialAs(t *testing.T, th *tenancyHub, o pki.IssueOptions) *grpc.ClientConn {
	t.Helper()
	certPEM, keyPEM, err := th.ca.Issue(o)
	if err != nil {
		t.Fatal(err)
	}
	b, err := pki.ParseBundle(th.ca.CertPEM, certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	c, err := grpc.NewClient(th.h.Addr("grpc"), grpc.WithTransportCredentials(credentials.NewTLS(hubclient.TLSConfig(b))))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}
