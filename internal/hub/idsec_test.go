package hub

import (
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

func bundleConn(t *testing.T, h *Hub, b *pki.Bundle) *grpc.ClientConn {
	t.Helper()
	c, err := grpc.NewClient(h.Addr("grpc"), grpc.WithTransportCredentials(credentials.NewTLS(hubclient.TLSConfig(b))))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// TestUnknownCertificatesNeverByName: a certificate the hub never saw is
// never mapped to a node by name, and registers a new node only for a
// name without history: after a revocation (and a new node with the
// name), a copy of a revoked node's identity never works again; a
// re-issue with the node's ID (longarm admin issue --id) maps by ID.
func TestUnknownCertificatesNeverByName(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	// a fresh name: registers (hand-issued certificates before a hub)
	if _, err := pb.NewFleetClient(dialAs(t, th, pki.IssueOptions{Role: pki.RoleClient, Name: "fresh"})).Whoami(tctx(t), &pb.WhoamiRequest{}); err != nil {
		t.Fatalf("a fresh name: %v", err)
	}
	// "rw": a worker that joins, then is revoked; a new "rw" joins
	join := func() *hubclient.JoinResult {
		jc, err := pb.NewFleetClient(th.alice).CreateWorkerCode(tctx(t), &pb.CreateWorkerCodeRequest{Name: "rw"})
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
	// an unseen second certificate of the same identity, without an ID
	// (as one issued by hand before node IDs): refused while the node lives,
	// and after its revocation - never a fresh node, never the replacement
	unseen := dialAs(t, th, pki.IssueOptions{Role: pki.RoleWorker, Name: "rw"})
	whoami := func(c *grpc.ClientConn) error {
		_, err := pb.NewFleetClient(c).Whoami(tctx(t), &pb.WhoamiRequest{})
		return err
	}
	if err := whoami(unseen); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unseen certificate of a live node: %v", err)
	}
	if _, err := h.Revoke("test", "id:"+first.Resp.Id); err != nil {
		t.Fatal(err)
	}
	if err := whoami(unseen); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unseen certificate of a revoked node: %v", err)
	}
	second := join()
	if err := whoami(unseen); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unseen certificate after the name was reused: %v", err)
	}
	h.mu.Lock()
	n := len(h.nodes)
	h.mu.Unlock()
	// the explicit mapping: a certificate re-issued with the node's ID
	reissued := dialAs(t, th, pki.IssueOptions{Role: pki.RoleWorker, Name: "rw", ID: second.Resp.Id})
	if _, err := pb.NewFleetClient(reissued).Whoami(tctx(t), &pb.WhoamiRequest{}); status.Code(err) == codes.Unauthenticated {
		t.Fatalf("re-issued with the id: %v", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.nodes) != n {
		t.Fatal("a node was registered for a name with history")
	}
}

// TestDeviceUserPinned: a device whose certificates named no user acts as
// the user of its name; that user is pinned (at registration, join and
// renewal) so a new name never makes it another user, and a device whose
// user can't be carried under another name is not renamed.
func TestDeviceUserPinned(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	// "carol": a hand-issued client certificate (no user): pinned at registration
	if _, err := pb.NewFleetClient(th.carol).Whoami(tctx(t), &pb.WhoamiRequest{}); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	n := h.activeByNameLocked("carol")
	user := n.rec.User
	h.mu.Unlock()
	if user != "carol" {
		t.Fatalf("carol's user %q", user)
	}
	// renamed to a name that looks like another user's: still carol
	if _, err := h.RenameNode("admin/root", "root", true, "carol", "d-bob"); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	rec := *h.activeByNameLocked("d-bob").rec
	h.mu.Unlock()
	_, csr, _ := pki.NewKeyAndCSR("", nil)
	certPEM, err := h.issue(&rec, csr)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := pki.ParseCertPEM(certPEM)
	id, _ := pki.IdentityFromCert(leaf)
	if id.Name != "d-bob" || id.Owner() != "carol" {
		t.Fatalf("renewed identity %+v acts as %s", id, id.Owner())
	}
	// a device named like no valid user ("Big-PC": its user is "Big-PC")
	if _, err := pb.NewFleetClient(dialAs(t, th, pki.IssueOptions{Role: pki.RoleClient, Name: "Big-PC"})).Whoami(tctx(t), &pb.WhoamiRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.RenameNode("admin/root", "root", true, "Big-PC", "big-pc"); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("rename of a device whose user can't be carried: %v", err)
	}
	// a join code without a user: the device's user is pinned to its name
	jc, err := h.CreateJoinCode("test", &pb.CreateJoinCodeRequest{Role: "client", Name: "dana"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hubclient.Join(tctx(t), h.Addr("grpc"), jc.Code, hubclient.JoinOptions{Fingerprint: pki.Fingerprint(th.ca.Cert)}); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if u := h.activeByNameLocked("dana").rec.User; u != "dana" {
		t.Fatalf("dana's user %q", u)
	}
}

// TestRenameSupersedesOldCertificates: once a renamed node uses the
// certificate issued with its new name, its older certificates (the old
// name, which another node may take now) are revoked; the hub reports the
// serials of certificates without an ID a node may still answer with.
func TestRenameSupersedesOldCertificates(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	jc, err := h.CreateJoinCode("test", &pb.CreateJoinCodeRequest{Role: "client", Name: "dev1", User: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := hubclient.Join(tctx(t), h.Addr("grpc"), jc.Code, hubclient.JoinOptions{Fingerprint: pki.Fingerprint(th.ca.Cert)})
	if err != nil {
		t.Fatal(err)
	}
	old := writeTestBundle(t, h, r)
	if _, err := h.RenameNode("admin/root", "root", true, "dev1", "dev2"); err != nil {
		t.Fatal(err)
	}
	hc, err := hubclient.Dial(h.Addr("grpc"), old)
	if err != nil {
		t.Fatal(err)
	}
	defer hc.Close()
	certPEM, keyPEM, caPEM, err := hubclient.Renew(tctx(t), hc, "dev2", nil)
	if err != nil {
		t.Fatal(err)
	}
	nb, err := pki.ParseBundle(caPEM, certPEM, keyPEM)
	if err != nil || nb.Identity.Name != "dev2" || nb.Identity.ID != r.Resp.Id {
		t.Fatalf("renewed %+v %v", nb.Identity, err)
	}
	if h.revoked.Revoked(old.Leaf()) {
		t.Fatal("the old certificate revoked before the new one is in use")
	}
	if _, err := pb.NewFleetClient(bundleConn(t, h, nb)).Whoami(tctx(t), &pb.WhoamiRequest{}); err != nil {
		t.Fatal(err)
	}
	if !h.revoked.Revoked(old.Leaf()) || h.revoked.Revoked(nb.Leaf()) {
		t.Fatalf("after the new certificate's first use: old revoked %v, new %v", h.revoked.Revoked(old.Leaf()), h.revoked.Revoked(nb.Leaf()))
	}

	// legacy serials: alice's hand-issued certificate (no ID) is reported
	// for her node until the node is revoked
	if _, err := pb.NewFleetClient(th.alice).Whoami(tctx(t), &pb.WhoamiRequest{}); err != nil {
		t.Fatal(err)
	}
	legacy := func() []string {
		for _, n := range h.ListNodes(true) {
			if n.Name == "alice" && n.State == stateApproved {
				return n.LegacySerials
			}
		}
		return nil
	}
	if len(legacy()) != 1 {
		t.Fatalf("alice's legacy serials %v", legacy())
	}
	if _, err := h.Revoke("test", "alice"); err != nil {
		t.Fatal(err)
	}
	for _, n := range h.ListNodes(true) {
		if n.Name == "alice" && len(n.LegacySerials) != 0 {
			t.Fatalf("a revoked node's legacy serials %v", n.LegacySerials)
		}
	}
}
