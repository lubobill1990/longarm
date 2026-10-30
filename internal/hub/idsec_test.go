package hub

import (
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/tenancy"
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
	if _, err := h.RenameNode("admin/root", tenancy.Caller{Admin: true}, true, "carol", "d-bob"); err != nil {
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
	if _, err := h.RenameNode("admin/root", tenancy.Caller{Admin: true}, true, "Big-PC", "big-pc"); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("rename of a device whose user can't be carried: %v", err)
	}
	// a join code without a user: with users separated it enrolls nothing
	// (#60); a device that has one acts as that user (carol above, the
	// migration: TestMigrateNodeIDs)
	if _, err := h.CreateJoinCode("test", &pb.CreateJoinCodeRequest{Role: "client", Name: "dana"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("userless client code: %v", err)
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
	if _, err := h.RenameNode("admin/root", tenancy.Caller{Admin: true}, true, "dev1", "dev2"); err != nil {
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

// TestRetirePendingSurvivesRenewals: after a rename, renewals with the
// original certificate keep the pending retirement; once a certificate with
// the new name is presented, every other certificate is revoked.
func TestRetirePendingSurvivesRenewals(t *testing.T) {
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
	if _, err := h.RenameNode("admin/root", tenancy.Caller{Admin: true}, true, "dev1", "dev2"); err != nil {
		t.Fatal(err)
	}
	hc, err := hubclient.Dial(h.Addr("grpc"), old)
	if err != nil {
		t.Fatal(err)
	}
	defer hc.Close()
	var renewed []*pki.Bundle
	for range 2 { // two renewals, both with the original certificate
		certPEM, keyPEM, caPEM, err := hubclient.Renew(tctx(t), hc, "dev2", nil)
		if err != nil {
			t.Fatal(err)
		}
		nb, err := pki.ParseBundle(caPEM, certPEM, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		renewed = append(renewed, nb)
	}
	h.mu.Lock()
	pending := h.nodes[r.Resp.Id].rec.RetirePending
	h.mu.Unlock()
	if !pending || h.revoked.Revoked(old.Leaf()) {
		t.Fatalf("after two renewals with the old certificate: pending %v, old revoked %v", pending, h.revoked.Revoked(old.Leaf()))
	}
	if _, err := pb.NewFleetClient(bundleConn(t, h, renewed[1])).Whoami(tctx(t), &pb.WhoamiRequest{}); err != nil {
		t.Fatal(err)
	}
	if !h.revoked.Revoked(old.Leaf()) || !h.revoked.Revoked(renewed[0].Leaf()) || h.revoked.Revoked(renewed[1].Leaf()) {
		t.Fatal("the adopted certificate must be the only one left")
	}
}

// TestLegacyNameRevocation: revoking a node whose history has certificates
// without a node ID revokes its (role, name) for ID-less certificates -
// on workers too (the revocation list) - so a copy the hub never saw stops
// working; certificates with an ID of that name are not affected.
func TestLegacyNameRevocation(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	if _, err := pb.NewFleetClient(th.alice).Whoami(tctx(t), &pb.WhoamiRequest{}); err != nil { // registered, ID-less
		t.Fatal(err)
	}
	unseenPEM, _, _ := th.ca.Issue(pki.IssueOptions{Role: pki.RoleClient, Name: "alice"})
	unseen, _ := pki.ParseCertPEM(unseenPEM)
	withIDPEM, _, _ := th.ca.Issue(pki.IssueOptions{Role: pki.RoleClient, Name: "alice", ID: "3f2a9c1e-4b5d-4c3e-8f00-1234567890ab"})
	withID, _ := pki.ParseCertPEM(withIDPEM)
	if h.revoked.Revoked(unseen) {
		t.Fatal("revoked before the revocation")
	}
	if _, err := h.Revoke("test", "alice"); err != nil {
		t.Fatal(err)
	}
	if !h.revoked.Revoked(unseen) || h.revoked.Revoked(withID) {
		t.Fatalf("unseen legacy copy revoked %v, an ID certificate of the name %v", h.revoked.Revoked(unseen), h.revoked.Revoked(withID))
	}
	// what workers get
	var found bool
	for _, n := range h.revocationsMsg().GetRevocations().Names {
		found = found || (n.Role == "client" && n.Name == "alice")
	}
	if !found {
		t.Fatal("the legacy name revocation is not in the workers' list")
	}
	// a worker's list (from the message) refuses the copy the same way
	wl, _ := pki.LoadRevocationList("")
	names := map[string]time.Time{}
	for _, n := range h.revocationsMsg().GetRevocations().Names {
		names[pki.RevokedNameKey(pki.Role(n.Role), n.Name)] = n.NotAfter.AsTime()
	}
	if err := wl.ReplaceAllNames(1, nil, nil, names); err != nil {
		t.Fatal(err)
	}
	if !wl.Revoked(unseen) || wl.Revoked(withID) {
		t.Fatal("worker list")
	}
}

// TestTombstonesAtStart: legacy name revocations in the database at the
// hub's start (the migration's, for nodes revoked before the upgrade) last
// at least as long as the CA, and reach the workers' list.
func TestTombstonesAtStart(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	if err := h.store.AddRevokedName(store.RevokedName{Role: "worker", Name: "gone", NotAfter: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	err := h.loadRevocations()
	msg := h.revocationsMsgLocked().GetRevocations()
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	var until time.Time
	for _, n := range msg.Names {
		if n.Role == "worker" && n.Name == "gone" {
			until = n.NotAfter.AsTime()
		}
	}
	if until.Before(h.ca.Cert.NotAfter.Add(-time.Second)) {
		t.Fatalf("tombstone until %s, the CA until %s", until, h.ca.Cert.NotAfter)
	}
	legacyPEM, _, _ := th.ca.Issue(pki.IssueOptions{Role: pki.RoleWorker, Name: "gone"})
	legacy, _ := pki.ParseCertPEM(legacyPEM)
	if !h.revoked.Revoked(legacy) {
		t.Fatal("an ID-less certificate of a tombstoned name accepted")
	}
}
