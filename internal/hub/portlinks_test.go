package hub

import (
	"crypto/x509"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/portlink"
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
