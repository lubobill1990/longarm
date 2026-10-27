package hub

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/portlink"
	"github.com/lubobill1990/longarm/internal/tenancy"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// linkNode adds worker name with a fake session that records pushes.
func linkNode(h *Hub, name string, endpoints ...string) chan *pb.HubCommand {
	out := make(chan *pb.HubCommand, 64)
	h.mu.Lock()
	defer h.mu.Unlock()
	n := h.nodes[name]
	if n == nil {
		n = &node{rec: &store.Node{Name: name, Role: "worker", State: stateApproved}}
		h.nodes[name] = n
	}
	n.rec.Endpoints = endpoints
	n.sess = &nodeSession{id: 1000 + uint64(len(h.nodes)), out: out, cancel: func() {}}
	n.lastReport = time.Now().Add(time.Hour) // the hub's offline sweep must not end the fake session
	return out
}

// lastLinks drains out and returns the last port link list pushed.
func lastLinks(t *testing.T, out chan *pb.HubCommand) *pb.PortLinks {
	t.Helper()
	var last *pb.PortLinks
	for {
		select {
		case c := <-out:
			if pl := c.GetPortLinks(); pl != nil {
				last = pl
			}
		default:
			if last == nil {
				t.Fatal("no port link list pushed")
			}
			return last
		}
	}
}

// TestPortLinks: creation rules, ownership (others' links are NotFound),
// quotas, the lists and signed grants pushed to both workers, removal,
// expiry, links bound to jobs, persistence, audit.
func TestPortLinks(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	out1 := linkNode(h, "w1", "tcp://10.0.0.1:7701")
	out2 := linkNode(h, "w2", "tcp://10.0.0.2:7701", "longnet://hub:7443?name=w2")
	fleet := func(c *grpc.ClientConn) pb.FleetClient { return pb.NewFleetClient(c) }
	ctx := tctx(t)
	create := func(c *grpc.ClientConn, r *pb.CreatePortLinkRequest) (*pb.PortLink, error) {
		return fleet(c).CreatePortLink(ctx, r)
	}

	l, err := create(th.alice, &pb.CreatePortLinkRequest{From: "w1", To: "w2", Port: 8080, ListenPort: 18080})
	if err != nil {
		t.Fatal(err)
	}
	if l.Owner != "alice" || !portlink.ValidID(l.Id) || l.CreatedBy != "client/alice" || l.Status.GetState() != "pending" ||
		l.ExpiresAt.AsTime().Sub(l.CreatedAt.AsTime()) != 8*time.Hour {
		t.Fatalf("link %v", l)
	}

	// pushed: w1 (listens) gets a grant and w2's endpoints, w2 the link only
	p1, p2 := lastLinks(t, out1), lastLinks(t, out2)
	if len(p1.Links) != 1 || len(p2.Links) != 1 || p2.Links[0].Grant != nil || len(p2.Links[0].TargetEndpoints) != 0 {
		t.Fatalf("pushed %v / %v", p1, p2)
	}
	a := p1.Links[0]
	if a.Link.Id != l.Id || strings.Join(a.TargetEndpoints, " ") != "tcp://10.0.0.2:7701 longnet://hub:7443?name=w2" {
		t.Fatalf("assignment %v", a)
	}
	g, err := portlink.Verify(poolOf(th.ca), a.Grant, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if g.Id != l.Id || g.Owner != "alice" || g.From != "w1" || g.To != "w2" || g.Port != 8080 || g.ListenPort != 18080 ||
		g.ExpiresUnix != l.ExpiresAt.AsTime().Unix() {
		t.Fatalf("grant %v", g)
	}
	// w2's endpoints change: w1 gets them anew
	h.mu.Lock()
	h.nodes["w2"].rec.Endpoints = []string{"tcp://10.0.0.22:7701"}
	h.portLinkTargetChangedLocked("w2")
	h.mu.Unlock()
	if p := lastLinks(t, out1); p.Links[0].TargetEndpoints[0] != "tcp://10.0.0.22:7701" {
		t.Fatalf("new endpoints not pushed: %v", p)
	}

	// status reports from w1 (not from w2) show up in the list
	h.mu.Lock()
	h.applyPortLinkReportLocked(h.nodes["w2"], &pb.PortLinkReport{Links: []*pb.PortLinkStatus{{Id: l.Id, State: "error", Error: "forged"}}})
	h.applyPortLinkReportLocked(h.nodes["w1"], &pb.PortLinkReport{Links: []*pb.PortLinkStatus{{Id: l.Id, State: "listening", Connections: 3}}})
	h.mu.Unlock()
	ls, err := fleet(th.alice).ListPortLinks(ctx, &pb.ListPortLinksRequest{})
	if err != nil || len(ls.Links) != 1 || ls.Links[0].Status.State != "listening" || ls.Links[0].Status.Connections != 3 {
		t.Fatalf("alice's list %v %v", ls, err)
	}

	// others: invisible, not removable; admins see and may remove all
	if ls, _ := fleet(th.bob).ListPortLinks(ctx, &pb.ListPortLinksRequest{}); len(ls.GetLinks()) != 0 {
		t.Fatalf("bob sees %v", ls)
	}
	if _, err := fleet(th.bob).ListPortLinks(ctx, &pb.ListPortLinksRequest{All: true}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("bob lists all: %v", err)
	}
	if _, err := fleet(th.bob).RemovePortLink(ctx, &pb.RemovePortLinkRequest{Id: l.Id}); status.Code(err) != codes.NotFound {
		t.Fatalf("bob removes alice's link: %v", err)
	}
	if ls, err := fleet(th.root).ListPortLinks(ctx, &pb.ListPortLinksRequest{All: true}); err != nil || len(ls.Links) != 1 {
		t.Fatalf("root lists all: %v %v", ls, err)
	}
	// the listen port is taken - also for bob, who learns nothing about the link
	if _, err := create(th.alice, &pb.CreatePortLinkRequest{From: "w1", To: "w2", Port: 9090, ListenPort: 18080}); status.Code(err) != codes.AlreadyExists || !strings.Contains(err.Error(), l.Id) {
		t.Fatalf("same listen port: %v", err)
	}
	if _, err := create(th.bob, &pb.CreatePortLinkRequest{From: "w1", To: "w2", Port: 9090, ListenPort: 18080}); status.Code(err) != codes.AlreadyExists || strings.Contains(err.Error(), l.Id) {
		t.Fatalf("bob, same listen port: %v", err)
	}

	// invalid requests
	for _, r := range []*pb.CreatePortLinkRequest{
		{From: "w1", To: "w1", Port: 80},
		{From: "w1", To: "nope", Port: 80},
		{From: "w1", To: "alice", Port: 80}, // a client, not a worker
		{From: "w1", To: "w2", Port: 0},
		{From: "w1", To: "w2", Port: 80}, // listen port 80 (default = target)
		{From: "w1", To: "w2", Port: 8080, ListenPort: 70000},
		{From: "w1", To: "w2", Port: 8080, ListenPort: 28080, Ttl: durationpb.New(time.Second)},
		{From: "w1", To: "w2", Port: 8080, ListenPort: 28080, Ttl: durationpb.New(30 * 24 * time.Hour)},
		{From: "w1", To: "w2", Port: 8080, ListenPort: 28080, JobId: "JB"}, // bob's job
		{From: "w1", To: "w2", Port: 8080, ListenPort: 28080, JobId: "NOPE"},
	} {
		if _, err := create(th.alice, r); err == nil {
			t.Errorf("created %v", r)
		}
	}
	h.mu.Lock()
	h.nodes["alice"] = &node{rec: &store.Node{Name: "alice", Role: "client", State: stateApproved}}
	h.mu.Unlock()
	if _, err := create(th.alice, &pb.CreatePortLinkRequest{From: "w1", To: "alice", Port: 8080, ListenPort: 28080}); status.Code(err) != codes.NotFound {
		t.Fatalf("link to a client: %v", err)
	}

	// a link bound to a job ends with the job
	lj, err := create(th.alice, &pb.CreatePortLinkRequest{From: "w1", To: "w2", Port: 5432, JobId: "JA"})
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.applyJobsLocked(h.nodes["w1"], &pb.JobsReport{Jobs: []*pb.Job{{Spec: &pb.JobSpec{Id: "JA", Owner: "alice"}, State: pb.JobState_JOB_STATE_DONE}}})
	_, still := h.portLinks[lj.Id]
	h.mu.Unlock()
	if still {
		t.Fatal("link outlived its job")
	}
	if p := lastLinks(t, out1); len(p.Links) != 1 || p.Links[0].Link.Id != l.Id {
		t.Fatalf("after job end w1 has %v", p)
	}
	if _, err := create(th.alice, &pb.CreatePortLinkRequest{From: "w1", To: "w2", Port: 5432, JobId: "JA"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("link to an ended job: %v", err)
	}

	// a shared source worker would expose a per-user target's port to everybody there
	if _, err := create(th.alice, &pb.CreatePortLinkRequest{From: "w2", To: "w1", Port: 3000, ListenPort: 13000}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("shared -> per-user: %v", err)
	}
	h.mu.Lock()
	h.nodes["w2"].rec.Isolation = tenancy.PerUser
	h.mu.Unlock()

	// expiry
	le, err := create(th.alice, &pb.CreatePortLinkRequest{From: "w2", To: "w1", Port: 3000, ListenPort: 13000, Ttl: durationpb.New(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.sweepPortLinksLocked(time.Now().Add(2 * time.Minute))
	_, still = h.portLinks[le.Id]
	h.mu.Unlock()
	if still {
		t.Fatal("expired link kept")
	}

	// quota
	h.cfg.Links.MaxPerUser = 2
	if _, err := create(th.alice, &pb.CreatePortLinkRequest{From: "w2", To: "w1", Port: 3000, ListenPort: 13000}); err != nil {
		t.Fatal(err)
	}
	if _, err := create(th.alice, &pb.CreatePortLinkRequest{From: "w2", To: "w1", Port: 3001, ListenPort: 13001}); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("over quota: %v", err)
	}
	if _, err := create(th.bob, &pb.CreatePortLinkRequest{From: "w2", To: "w1", Port: 3001, ListenPort: 13001}); err != nil {
		t.Fatalf("bob's own quota: %v", err)
	}

	// persistence: a hub opened on the same data has the links (not the expired one)
	h2, err := Open(h.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	n2 := len(h2.portLinks)
	_, has := h2.portLinks[l.Id]
	h2.Close()
	if n2 != 3 || !has {
		t.Fatalf("reloaded %d links", n2)
	}

	// removal by the owner; then the user's and the node's links go with them
	if _, err := fleet(th.alice).RemovePortLink(ctx, &pb.RemovePortLinkRequest{Id: l.Id}); err != nil {
		t.Fatal(err)
	}
	if p := lastLinks(t, out2); len(p.Links) != 2 {
		t.Fatalf("w2 after removal: %v", p)
	}
	if _, err := h.RemoveUser("root", &pb.RemoveUserRequest{Name: "bob"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Revoke("root", "w1"); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	left := len(h.portLinks)
	h.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d links left after removing bob and revoking w1", left)
	}

	// audit on the hub
	rows, err := h.store.Audit("hub", time.Time{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, r := range rows {
		if strings.HasPrefix(r.Method, "links/") {
			lines = append(lines, r.Line)
		}
	}
	all := strings.Join(lines, "\n")
	for _, want := range []string{`"method":"links/create"`, `"reason":"job ended"`, `"reason":"expired"`, `"reason":"removed"`,
		`"reason":"user removed"`, `"reason":"node revoked"`} {
		if !strings.Contains(all, want) {
			t.Errorf("audit lacks %s:\n%s", want, all)
		}
	}
}

func poolOf(ca *pki.CA) *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.Cert)
	return p
}

// TestPortLinksIsolationRecheck: a link from a shared worker to one that
// turns per-user later is removed (it would expose the owner's private port
// to everybody on the source), on the worker's hello and at hub start; links
// an admin created stay.
func TestPortLinksIsolationRecheck(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	out2 := linkNode(h, "w2", "tcp://10.0.0.2:7701")
	out3 := linkNode(h, "w3", "tcp://10.0.0.3:7701")
	ctx := tctx(t)
	create := func(c *grpc.ClientConn, port int32) *pb.PortLink {
		t.Helper()
		l, err := pb.NewFleetClient(c).CreatePortLink(ctx, &pb.CreatePortLinkRequest{From: "w2", To: "w3", Port: port, ListenPort: 10000 + port})
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	la, lr := create(th.alice, 3000), create(th.root, 3001)
	lastLinks(t, out2)
	lastLinks(t, out3)

	// w3 reports per-user isolation
	h.mu.Lock()
	n3 := h.nodes["w3"]
	h.mu.Unlock()
	h.handleReport(n3, n3.sess, &pb.NodeReport{Msg: &pb.NodeReport_Hello{Hello: &pb.NodeHello{Isolation: tenancy.PerUser, Endpoints: []string{"tcp://10.0.0.3:7701"}}}})
	h.mu.Lock()
	_, kept := h.portLinks[la.Id]
	_, keptAdmin := h.portLinks[lr.Id]
	h.mu.Unlock()
	if kept || !keptAdmin {
		t.Fatalf("after w3 turned per-user: alice's link kept %v, root's %v", kept, keptAdmin)
	}
	for name, out := range map[string]chan *pb.HubCommand{"w2": out2, "w3": out3} {
		if p := lastLinks(t, out); len(p.Links) != 1 || p.Links[0].Link.Id != lr.Id {
			t.Fatalf("%s's list after the recheck: %v", name, p)
		}
	}
	rows, err := h.store.Audit("hub", time.Time{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		found = found || (r.Method == "links/remove" && strings.Contains(r.Line, la.Id) && strings.Contains(r.Line, "not allowed any more"))
	}
	if !found {
		t.Fatal("removal not audited with a reason")
	}

	// at start: w3's isolation changed while the hub was down
	h.mu.Lock()
	n3.rec.Isolation = ""
	h.mu.Unlock()
	lb := create(th.alice, 3002)
	h.mu.Lock()
	n3.rec.Isolation = tenancy.PerUser
	if err := errors.Join(h.store.PutNode(n3.rec), h.store.PutNode(h.nodes["w2"].rec)); err != nil {
		h.mu.Unlock()
		t.Fatal(err)
	}
	h.mu.Unlock()
	h2, err := Open(h.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	_, kept = h2.portLinks[lb.Id]
	_, keptAdmin = h2.portLinks[lr.Id]
	h2.Close()
	if kept || !keptAdmin {
		t.Fatalf("reopened hub: alice's link kept %v, root's %v", kept, keptAdmin)
	}
}

// TestPortLinksOwnedWorkers: a link's owner must be allowed to use both
// workers - admins are no exception on a user-owned worker - and a link
// ends when its owner loses a worker (unshared).
func TestPortLinksOwnedWorkers(t *testing.T) {
	th, _ := ownedHub(t)
	h := th.h
	linkNode(h, "w2", "tcp://10.0.0.2:7701")
	ctx := tctx(t)
	create := func(c *grpc.ClientConn, from, to string) (*pb.PortLink, error) {
		return pb.NewFleetClient(c).CreatePortLink(ctx, &pb.CreatePortLinkRequest{From: from, To: to, Port: 8080, ListenPort: 18080})
	}
	for name, c := range map[string]*grpc.ClientConn{"bob": th.bob, "root (admin)": th.root} {
		for _, dir := range [][2]string{{"w2", "aw"}, {"aw", "w2"}} {
			if _, err := create(c, dir[0], dir[1]); status.Code(err) != codes.NotFound || !strings.Contains(err.Error(), `no worker "aw"`) {
				t.Fatalf("%s %s->%s: %v", name, dir[0], dir[1], err)
			}
		}
	}
	al, err := create(th.alice, "w2", "aw")
	if err != nil {
		t.Fatalf("alice: %v", err)
	}
	if _, err := h.ShareWorker("test", "", true, &pb.ShareWorkerRequest{Worker: "aw", AddUsers: []string{"bob"}}); err != nil {
		t.Fatal(err)
	}
	bl, err := pb.NewFleetClient(th.bob).CreatePortLink(ctx, &pb.CreatePortLinkRequest{From: "w2", To: "aw", Port: 8081, ListenPort: 18081})
	if err != nil {
		t.Fatalf("bob after the share: %v", err)
	}
	if _, err := h.ShareWorker("test", "", true, &pb.ShareWorkerRequest{Worker: "aw", RemoveUsers: []string{"bob"}}); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	_, bobs := h.portLinks[bl.Id]
	_, alices := h.portLinks[al.Id]
	h.mu.Unlock()
	if bobs || !alices {
		t.Fatalf("after the unshare: bob's link kept %v, alice's kept %v", bobs, alices)
	}
}

// TestPortLinksConsent: a link created with its owner's signed consent
// takes id and expiry from it and keeps it (pushed to the workers); a
// consent for another user, another link or too long a time is refused.
func TestPortLinksConsent(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	out1 := linkNode(h, "w1", "tcp://10.0.0.1:7701")
	linkNode(h, "w2", "tcp://10.0.0.2:7701")
	_, uk, _ := ed25519.GenerateKey(rand.Reader)
	sign := func(user string, spec *pb.PortLinkSpec) *pb.PortLinkConsent {
		_, nk, _ := ed25519.GenerateKey(rand.Reader)
		e, err := pki.Endorse(uk, user, pki.KindDevice, user+"-laptop", nk.Public().(ed25519.PublicKey))
		if err != nil {
			t.Fatal(err)
		}
		c, err := portlink.SignConsent(nk, e, spec)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	exp := time.Now().Add(2 * time.Hour).Unix()
	spec := &pb.PortLinkSpec{Id: portlink.NewID(), From: "w1", To: "w2", Port: 8080, ListenPort: 18080, ExpiresUnix: exp}
	req := func(c *pb.PortLinkConsent, port int32) *pb.CreatePortLinkRequest {
		return &pb.CreatePortLinkRequest{From: "w1", To: "w2", Port: port, ListenPort: 18080, Consent: c}
	}
	fleet := pb.NewFleetClient(th.alice)
	if _, err := fleet.CreatePortLink(tctx(t), req(sign("bob", spec), 8080)); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("bob's consent: %v", err)
	}
	if _, err := fleet.CreatePortLink(tctx(t), req(sign("alice", spec), 8081)); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("consent for another port: %v", err)
	}
	long := proto.Clone(spec).(*pb.PortLinkSpec)
	long.ExpiresUnix = time.Now().Add(1000 * time.Hour).Unix()
	if _, err := fleet.CreatePortLink(tctx(t), req(sign("alice", long), 8080)); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("consent beyond max_ttl: %v", err)
	}
	c := sign("alice", spec)
	l, err := fleet.CreatePortLink(tctx(t), req(c, 8080))
	if err != nil {
		t.Fatal(err)
	}
	if l.Id != spec.Id || l.ExpiresAt.AsTime().Unix() != exp || !proto.Equal(l.Consent, c) {
		t.Fatalf("link %v", l)
	}
	if p := lastLinks(t, out1); len(p.Links) != 1 || !proto.Equal(p.Links[0].Link.Consent, c) {
		t.Fatalf("pushed %v", p)
	}
	if _, err := fleet.CreatePortLink(tctx(t), &pb.CreatePortLinkRequest{From: "w1", To: "w2", Port: 8080, ListenPort: 18090,
		Consent: sign("alice", &pb.PortLinkSpec{Id: spec.Id, From: "w1", To: "w2", Port: 8080, ListenPort: 18090, ExpiresUnix: exp})}); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("a second link with the same id: %v", err)
	}
}
