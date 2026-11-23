package hub

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"longarm/internal/hub/store"
	"longarm/internal/nodeid"
	"longarm/internal/pki"
	pb "longarm/proto/longarm/v1"
)

func TestWorkerCodeOwnership(t *testing.T) {
	th := newTenancyHub(t)
	admin := th.dial(pki.IssueOptions{Role: pki.RoleAdmin, Name: "alice-admin", User: "alice"})
	for _, owner := range []string{"", "alice"} {
		jc, err := pb.NewHubAdminClient(admin).CreateJoinCode(tctx(t), &pb.CreateJoinCodeRequest{Role: "worker", Owner: owner})
		if err != nil || jc.Owner != "alice" {
			t.Fatalf("own code: %v %v", jc, err)
		}
	}
	if _, err := pb.NewHubAdminClient(admin).CreateJoinCode(tctx(t), &pb.CreateJoinCodeRequest{Role: "worker", Owner: "bob"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("other owner: %v", err)
	}
	for _, owner := range []string{"", "missing"} {
		_, err := th.h.CreateJoinCode("local", &pb.CreateJoinCodeRequest{Role: "worker", Owner: owner})
		want := codes.FailedPrecondition
		if owner != "" {
			want = codes.NotFound
		}
		if status.Code(err) != want {
			t.Fatalf("host owner %q: %v", owner, err)
		}
	}
	if _, err := th.h.CreateJoinCode("local", &pb.CreateJoinCodeRequest{Role: "worker", Owner: "bob"}); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"", "missing", "alice"} {
		iss, err := IssueLocal(th.h.cfg, LocalIssueOptions{Role: pki.RoleWorker, Name: "worker-1", Owner: owner})
		if owner != "alice" {
			if err == nil {
				t.Fatalf("host issue accepted owner %q", owner)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		leaf, _ := pki.ParseCertPEM(iss.CertPEM)
		id, _ := pki.IdentityFromCert(leaf)
		if _, err := th.h.checkNode(id, leaf); err != nil {
			t.Fatal(err)
		}
		rec, err := th.h.store.Node(iss.NodeID)
		if err != nil || rec.Owner != "alice" || rec.AccessVersion != 1 {
			t.Fatalf("issued worker: %+v %v", rec, err)
		}
	}
}

func TestClaimWorker(t *testing.T) {
	th, _ := ownedHub(t)
	h := th.h
	out := make(chan *pb.HubCommand, 128)
	h.mu.Lock()
	n := h.nodeLocked("w1")
	id := n.rec.ID
	n.sess = &nodeSession{id: 100, out: out, cancel: func() {}}
	h.mu.Unlock()
	if _, err := h.ClaimWorker("test", "", id); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("no user: %v", err)
	}
	for _, target := range []string{"aw", "alice", "missing"} {
		_, err := pb.NewFleetClient(th.bob).ClaimWorker(tctx(t), &pb.ClaimWorkerRequest{Worker: target})
		if status.Code(err) != codes.NotFound {
			t.Fatalf("claim %s: %v", target, err)
		}
	}
	info, err := pb.NewFleetClient(th.alice).ClaimWorker(tctx(t), &pb.ClaimWorkerRequest{Worker: id})
	if err != nil || info.Id != id || info.Owner != "alice" {
		t.Fatalf("claim: %v %v", info, err)
	}
	if wa := lastAccess(out); wa == nil || wa.Owner != "alice" || wa.Version != 1 {
		t.Fatalf("access push: %v", wa)
	}
	rec, err := h.store.Node(id)
	if err != nil || rec.Owner != "alice" || rec.AccessVersion != 1 {
		t.Fatalf("persisted claim: %+v %v", rec, err)
	}
	if _, err := h.ClaimWorker("test", "alice", id); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("already owned: %v", err)
	}
	if nodeNames(t, th.bob)["w1"] != nil {
		t.Fatal("bob still sees claimed worker")
	}
	for _, tc := range []struct {
		name  string
		admin bool
		state string
	}{{"revoked-worker", false, stateRevoked}, {"admin-worker", true, stateApproved}} {
		h.mu.Lock()
		n := &node{rec: &store.Node{ID: nodeid.New(), Name: tc.name, Role: "worker", State: tc.state, Admin: tc.admin}}
		h.putTestNode(n)
		h.mu.Unlock()
		if _, err := h.ClaimWorker("test", "alice", n.rec.ID); err == nil {
			t.Fatalf("claimed %s", tc.name)
		}
	}
	rows, err := h.store.Audit("", "hub", time.Time{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		found = found || r.Method == "workers/claim"
	}
	if !found {
		t.Fatal("missing claim audit")
	}
}

func TestAdopt(t *testing.T) {
	dir := t.TempDir()
	if _, err := InitCA(dir); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{DataDir: dir}
	if err := cfg.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	device, worker, skipped := nodeid.New(), nodeid.New(), nodeid.New()
	for _, rec := range []*store.Node{
		{ID: device, Name: "device-1", Role: "admin", User: "old-user", State: stateApproved},
		{ID: worker, Name: "worker-1", Role: "worker", State: stateApproved, AccessVersion: 4},
		{ID: skipped, Name: "revoked-worker", Role: "worker", State: stateRevoked},
	} {
		if err := st.PutNode(rec); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range [][]string{{"missing=bob"}, {"worker-1=Bad User"}, {"worker-1=bob", worker + "=alice"}} {
		if _, err := Adopt(st, "alice", m); err == nil {
			t.Fatalf("accepted mappings %v", m)
		}
	}
	h, err := Open(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	external := nodeid.New()
	if err := st.PutNode(&store.Node{ID: external, Name: "offline-worker", Role: "worker", State: stateApproved}); err != nil {
		t.Fatal(err)
	}
	oldCode, err := h.CreateJoinCode("local", &pb.CreateJoinCodeRequest{Role: "worker"})
	if err != nil || oldCode.Owner != "" {
		t.Fatalf("ownerless code before adoption: %v %v", oldCode, err)
	}
	plan, err := Adopt(st, "alice", []string{"device-1=bob"})
	if err != nil || len(plan.Devices) != 1 || plan.Workers[worker] != "alice" {
		t.Fatalf("plan %+v %v", plan, err)
	}
	if _, err := Adopt(st, "alice", nil); err == nil {
		t.Fatal("adopted twice")
	}
	h.mu.Lock()
	if err := h.syncAdoptionLocked(); err != nil {
		t.Fatal(err)
	}
	if n := h.nodes[worker]; n.rec.Owner != "alice" || n.rec.AccessVersion != 5 {
		t.Fatalf("worker %+v", n.rec)
	}
	if n := h.nodes[external]; n == nil || n.rec.Owner != "alice" {
		t.Fatalf("adoption missed an offline node issued outside the hub: %+v", n)
	}
	old := pki.Identity{Role: pki.RoleAdmin, Name: "device-1", User: "old-user"}
	if user := h.userOfLocked(old, device); user != "bob" {
		t.Fatalf("old certificate beats pinned user: %s", user)
	}
	if certUser(h.nodes[device].rec) != "bob" {
		t.Fatal("renewal does not carry adopted user")
	}
	if h.nodes[skipped].rec.Owner != "" {
		t.Fatal("adopted revoked worker")
	}
	h.mu.Unlock()
	_, csr, err := pki.NewKeyAndCSR("device-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	renewed, err := h.renew(caller{id: old, nodeID: device}, csr)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := pki.ParseCertPEM(renewed.CertPem)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := pki.IdentityFromCert(leaf)
	if err != nil || identity.User != "bob" {
		t.Fatalf("renewed identity: %+v %v", identity, err)
	}
	if _, err := (&enrollServer{h: h}).Join(tctx(t), &pb.JoinRequest{Code: oldCode.Code, CsrPem: csr, Name: "new-worker"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("old ownerless code enrolled after adoption: %v", err)
	}
}

func TestShareRemovalWithoutPermission(t *testing.T) {
	th, _ := ownedHub(t)
	h := th.h
	seedWorkerShares(t, h, &pb.ShareWorkerRequest{Worker: "aw", AddUsers: []string{"bob"}, AddTeams: []string{"t1"}, SetEveryone: true, Everyone: true})
	for _, req := range []*pb.ShareWorkerRequest{
		{Worker: "aw", AddUsers: []string{"carol"}},
		{Worker: "aw", AddTeams: []string{"t2"}},
		{Worker: "aw", SetEveryone: true, Everyone: true},
		{Worker: "aw", RemoveUsers: []string{"bob"}, AddTeams: []string{"t2"}},
	} {
		if _, err := pb.NewFleetClient(th.alice).ShareWorker(tctx(t), req); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("new grant accepted: %v", err)
		}
	}
	info, err := pb.NewFleetClient(th.alice).ShareWorker(tctx(t), &pb.ShareWorkerRequest{
		Worker: "aw", RemoveUsers: []string{"bob"}, RemoveTeams: []string{"t1"}, SetEveryone: true})
	if err != nil || info.Shares != nil {
		t.Fatalf("removing legacy shares without can_share: %v %v", info, err)
	}
}
