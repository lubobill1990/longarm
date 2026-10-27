package portlink

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// device is a node key and its endorsement as a device of user by userKey.
func device(t *testing.T, userKey ed25519.PrivateKey, user, name string) (ed25519.PrivateKey, *pki.Endorsement) {
	t.Helper()
	_, nk, _ := ed25519.GenerateKey(rand.Reader)
	e, err := pki.Endorse(userKey, user, pki.KindDevice, name, nk.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	return nk, e
}

// TestConsent: a link the owner's endorsed device signed is accepted by a
// worker pinning the owner's key; one without consent (the hub alone), with
// a spec changed after signing, not matching the link, outliving the
// consent, signed for/by someone else or by a denied device is refused.
func TestConsent(t *testing.T) {
	_, alice, _ := ed25519.GenerateKey(rand.Reader)
	_, bob, _ := ed25519.GenerateKey(rand.Reader)
	p := &pki.TrustPolicy{Users: map[string]ed25519.PublicKey{"alice": alice.Public().(ed25519.PublicKey), "bob": bob.Public().(ed25519.PublicKey)}}
	nk, e := device(t, alice, "alice", "alice-laptop")
	exp := time.Now().Add(time.Hour).Unix()
	spec := &pb.PortLinkSpec{Id: NewID(), From: "wa", To: "wb", Port: 8080, ListenPort: 18080, ExpiresUnix: exp}
	c, err := SignConsent(nk, e, spec)
	if err != nil {
		t.Fatal(err)
	}
	link := func() *pb.PortLink {
		return &pb.PortLink{Id: spec.Id, Owner: "alice", From: "wa", To: "wb", Port: 8080, ListenPort: 18080,
			ExpiresAt: timestamppb.New(time.Unix(exp, 0)), Consent: proto.Clone(c).(*pb.PortLinkConsent)}
	}
	if err := VerifyConsent(p, link()); err != nil {
		t.Fatalf("signed by the owner: %v", err)
	}
	shorter := link()
	shorter.ExpiresAt = timestamppb.New(time.Unix(exp-60, 0))
	if err := VerifyConsent(p, shorter); err != nil {
		t.Fatalf("ends earlier than consented: %v", err)
	}
	refused := func(name string, l *pb.PortLink, p *pki.TrustPolicy, want string) {
		t.Helper()
		if err := VerifyConsent(p, l); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", name, err, want)
		}
	}
	forged := link()
	forged.Consent = nil
	refused("hub-made (no consent)", forged, p, "no owner's consent")
	tampered := link()
	s := &pb.PortLinkSpec{}
	proto.Unmarshal(tampered.Consent.Spec, s)
	s.Port = 22
	tampered.Consent.Spec, _ = proto.Marshal(s)
	tampered.Port = 22
	refused("spec changed after signing", tampered, p, "bad signature")
	other := link()
	other.Port = 22
	refused("link differs from the consent", other, p, "not what its owner signed")
	longer := link()
	longer.ExpiresAt = timestamppb.New(time.Unix(exp+3600, 0))
	refused("outlives the consent", longer, p, "outlives")
	bnk, be := device(t, bob, "bob", "bob-laptop")
	bc, _ := SignConsent(bnk, be, spec)
	bobs := link()
	bobs.Consent = bc
	refused("signed by bob for alice's link", bobs, p, "pinned key")
	// a device endorsed by a key that is not alice's pinned one
	_, mallory, _ := ed25519.GenerateKey(rand.Reader)
	mnk, me := device(t, mallory, "alice", "alice-laptop")
	mc, _ := SignConsent(mnk, me, spec)
	minted := link()
	minted.Consent = mc
	refused("endorsed by an unpinned key", minted, p, "pinned key")
	refused("owner's key not pinned", link(), &pki.TrustPolicy{Users: map[string]ed25519.PublicKey{"bob": bob.Public().(ed25519.PublicKey)}}, "pinned key")
	denied := &pki.TrustPolicy{Users: p.Users, DenyNodes: [][]byte{e.NodeKey}}
	refused("denied device", link(), denied, "denied device")
	if _, err := SignConsent(nk, be, spec); err == nil {
		t.Error("signed with another device's endorsement")
	}
}

// TestVerifyRevokedHub: a grant signed by a revoked hub certificate is
// refused when the worker's revocation list is passed.
func TestVerifyRevokedHub(t *testing.T) {
	root := t.TempDir()
	ca, err := pki.InitCA(filepath.Join(root, "ca"), "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	hub := issue(t, ca, filepath.Join(root, "hub"), pki.IssueOptions{Role: pki.RoleHub, Name: "hub"})
	now := time.Now()
	g, err := Sign(hub.Certificate(), &pb.PortLinkGrant{Id: NewID(), Owner: "alice", From: "wa", To: "wb", Port: 1, ListenPort: 2,
		ExpiresUnix: now.Add(time.Hour).Unix(), IssuedUnix: now.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	rl, _ := pki.LoadRevocationList("")
	if _, err := VerifyRevocable(pool, g, now, rl.Revoked); err != nil {
		t.Fatalf("not revoked: %v", err)
	}
	leaf, _ := x509.ParseCertificate(g.Chain[0])
	if err := rl.Replace(1, map[string]time.Time{pki.Serial(leaf): leaf.NotAfter}); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyRevocable(pool, g, now, rl.Revoked); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("revoked hub certificate: %v", err)
	}
}
