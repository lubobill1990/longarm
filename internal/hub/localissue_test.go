package hub

import (
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// TestIssueLocal: certificates issued by another process next to the
// running hub (longarm-hub issue, longarm-hub-relay-cert,
// longarm-hub-release-cert) carry their node's ID and are recorded, so the
// hub accepts them at once - for an existing node with history, a new node
// it adopts from the database, and a revoked node's name (a new node).
func TestIssueLocal(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	dial := func(iss *LocalIssued) *grpc.ClientConn {
		t.Helper()
		b, err := pki.ParseBundle(iss.CAPEM, iss.CertPEM, iss.KeyPEM)
		if err != nil {
			t.Fatal(err)
		}
		c, err := grpc.NewClient(h.Addr("grpc"), grpc.WithTransportCredentials(credentials.NewTLS(hubclient.TLSConfig(b))))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	subscribe := func(c *grpc.ClientConn) error {
		t.Helper()
		st, err := pb.NewHubNodeClient(c).RelaySync(tctx(t), &pb.RelaySyncRequest{Udp: "127.0.0.1:3478"})
		if err != nil {
			return err
		}
		_, err = st.Recv()
		return err
	}
	issue := func(role pki.Role, name string) *LocalIssued {
		t.Helper()
		iss, err := IssueLocal(h.cfg, LocalIssueOptions{Role: role, Name: name, Hosts: []string{name + ".example.net"}})
		if err != nil {
			t.Fatalf("issue %s/%s: %v", role, name, err)
		}
		return iss
	}
	idOf := func(iss *LocalIssued) string {
		leaf, _ := pki.ParseCertPEM(iss.CertPEM)
		id, _ := pki.IdentityFromCert(leaf)
		return id.ID
	}

	// no node yet: a new one, created in the database; the running hub
	// doesn't hold it until its first call
	a := issue(pki.RoleRelay, "xrelay")
	h.mu.Lock()
	_, held := h.nodes[a.NodeID]
	h.mu.Unlock()
	if !a.NewNode || idOf(a) != a.NodeID || held {
		t.Fatalf("new relay: %+v (held %v)", a, held)
	}
	if err := subscribe(dial(a)); err != nil {
		t.Fatalf("the new relay: %v", err)
	}
	h.mu.Lock()
	n := h.nodes[a.NodeID]
	h.mu.Unlock()
	if n == nil || n.rec.Name != "xrelay" || n.rec.Role != "relay" || n.rec.State != stateApproved {
		t.Fatalf("adopted node %+v", n)
	}

	// again (the relay's upgrade): the same node, the hub accepts it by ID
	b := issue(pki.RoleRelay, "xrelay")
	if b.NewNode || b.NodeID != a.NodeID || idOf(b) != a.NodeID || b.Serial == a.Serial {
		t.Fatalf("re-issued relay: %+v", b)
	}
	if err := subscribe(dial(b)); err != nil {
		t.Fatalf("the re-issued relay: %v", err)
	}
	if nid, hasID, err := h.store.CertRecord(b.Serial); err != nil || nid != a.NodeID || !hasID {
		t.Fatalf("the re-issued certificate's record: %q %v %v", nid, hasID, err)
	}

	// the name is a relay's: no worker certificate for it
	if _, err := IssueLocal(h.cfg, LocalIssueOptions{Role: pki.RoleWorker, Name: "xrelay"}); err == nil || !strings.Contains(err.Error(), "a relay") {
		t.Fatalf("a worker certificate for the relay's name: %v", err)
	}

	// revoked: the name gets a new node; the old certificates stay refused
	if _, err := h.Revoke("test", a.NodeID); err != nil {
		t.Fatal(err)
	}
	c := issue(pki.RoleRelay, "xrelay")
	if !c.NewNode || c.NodeID == a.NodeID {
		t.Fatalf("after the revocation: %+v", c)
	}
	if err := subscribe(dial(c)); err != nil {
		t.Fatalf("the relay of the new node: %v", err)
	}
	if err := subscribe(dial(b)); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("the revoked node's certificate: %v", err)
	}

	// release credentials: a second one the same day is the same node's
	r1 := issue(pki.RoleRelease, "ci-20261030")
	r2 := issue(pki.RoleRelease, "ci-20261030")
	if r2.NodeID != r1.NodeID {
		t.Fatalf("release credentials: %s, %s", r1.NodeID, r2.NodeID)
	}
	for _, r := range []*LocalIssued{r1, r2} {
		if _, err := pb.NewFleetClient(dial(r)).Whoami(tctx(t), &pb.WhoamiRequest{}); err != nil {
			t.Fatalf("release credential %s: %v", r.Serial, err)
		}
	}
	// an admin certificate (bootstrap: longarm-hub issue --role admin)
	ad := issue(pki.RoleAdmin, "ops")
	who, err := pb.NewFleetClient(dial(ad)).Whoami(tctx(t), &pb.WhoamiRequest{})
	if err != nil || who.Id != ad.NodeID {
		t.Fatalf("admin whoami: %v %v", who, err)
	}
}
