package agentauth

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/pki"
)

// workerCert issues worker w1's certificate, its node key endorsed by
// endorser (nil: not endorsed).
func workerCert(t *testing.T, ca *pki.CA, endorserUser string, endorser ed25519.PrivateKey) *x509.Certificate {
	t.Helper()
	_, nodeKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ct := &pki.CSRTrust{NodeKey: nodeKey}
	if endorser != nil {
		if ct.Endorsement, err = pki.Endorse(endorser, endorserUser, pki.KindWorker, "w1", nodeKey.Public().(ed25519.PublicKey)); err != nil {
			t.Fatal(err)
		}
	}
	_, csrPEM, err := pki.NewKeyAndCSR("w1", ct)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, err := ca.SignCSR(csrPEM, pki.IssueOptions{Role: pki.RoleWorker, Name: "w1", Validity: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := pem.Decode(certPEM)
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// With end-to-end trust, "mine" is proven by this device's own endorsement
// of the worker only: a hub (or the worker, from hub-pushed metadata)
// claiming the caller owns it changes nothing.
func TestEndorsedByAndE2EOwnership(t *testing.T) {
	ca, err := pki.InitCA(t.TempDir(), "test CA", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	alicePub, alice, _ := ed25519.GenerateKey(rand.Reader)
	bobPub, bob, _ := ed25519.GenerateKey(rand.Reader)
	pol := &pki.TrustPolicy{Users: map[string]ed25519.PublicKey{"alice": alicePub, "bob": bobPub}}

	mine := workerCert(t, ca, "alice", alice)
	bobs := workerCert(t, ca, "bob", bob)
	none := workerCert(t, ca, "", nil)
	forged := workerCert(t, ca, "alice", bob) // signed by bob's key, claiming alice
	if !EndorsedBy(mine, pol, alicePub) {
		t.Fatal("alice's endorsed worker is not hers")
	}
	for name, c := range map[string]*x509.Certificate{"bob's": bobs, "unendorsed": none, "forged": forged} {
		if EndorsedBy(c, pol, alicePub) {
			t.Errorf("%s worker counted as alice's", name)
		}
	}
	if EndorsedBy(mine, nil, alicePub) || EndorsedBy(mine, pol, nil) {
		t.Error("no policy / no own key proves nothing")
	}

	// the hub claims bob's shared-isolation worker is alice's
	claimed := Target{Worker: "w1", Me: "alice", Owner: "alice", HubKnown: true, Isolation: "shared", E2E: true,
		EndorsedByMe: EndorsedBy(bobs, pol, alicePub)}
	_, err = Check(claimed, false, false)
	var ref *RefusedError
	if !errors.As(err, &ref) || !strings.Contains(err.Error(), "not endorsed by your user key") || !strings.Contains(err.Error(), "--agent-auth-shared-ok") {
		t.Fatalf("hub-claimed ownership under e2e trust: %v", err)
	}
	if d, err := Check(claimed, false, true); err != nil || !strings.Contains(d.Note, "WARNING") {
		t.Fatalf("with --agent-auth-shared-ok: %+v %v", d, err)
	}
	claimed.Isolation = "per-user"
	if _, err := Check(claimed, false, false); err != nil {
		t.Fatalf("per-user isolation: %v", err)
	}
	// alice's endorsed worker: hers, whatever the hub says
	own := Target{Worker: "w1", Me: "alice", Owner: "mallory", HubKnown: true, Isolation: "shared", E2E: true,
		EndorsedByMe: EndorsedBy(mine, pol, alicePub)}
	if _, err := Check(own, false, false); err != nil {
		t.Fatalf("endorsed own worker: %v", err)
	}
	// admin workers: only a proven own one, and only when named
	adm := Target{Worker: "w1-admin", Me: "alice", Owner: "alice", Admin: true, E2E: true}
	if _, err := Check(adm, true, true); err == nil {
		t.Fatal("unproven admin worker accepted")
	}
	adm.EndorsedByMe = true
	if _, err := Check(adm, true, false); err != nil {
		t.Fatalf("own endorsed admin worker: %v", err)
	}
	// without e2e trust the hub's word counts (it runs the CA)
	noE2E := Target{Worker: "w1", Me: "alice", Owner: "alice", HubKnown: true, Isolation: "shared"}
	if _, err := Check(noE2E, false, false); err != nil {
		t.Fatalf("hub-reported ownership without e2e trust: %v", err)
	}
}
