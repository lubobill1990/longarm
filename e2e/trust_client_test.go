package e2e

import (
	"crypto/ed25519"
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/trust"
)

// TestTrustClient: end-to-end trust on the client. longarm trust init makes
// a user key and renews the certificate through the hub with the device's
// endorsement; with require on, an unendorsed worker is refused with a
// message that says how to fix it, and accepted once its certificate
// carries an endorsement from longarm trust endorse.
func TestTrustClient(t *testing.T) {
	h := newHubEnv(t, "")
	e := h.env
	e.startLongarmd()
	e.mustLongarm(e.client, "info", "w1")

	out := e.mustLongarm(e.client, "trust", "init")
	if !strings.Contains(out, "certificate renewed") || !strings.Contains(out, "carries the endorsement") {
		t.Fatalf("trust init:\n%s", out)
	}
	var keyLine string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "longarm-user-key.v1.") {
			keyLine = l
		}
	}
	if _, err := pki.ParseUserKeyLine(keyLine); err != nil {
		t.Fatalf("no user key line in:\n%s", out)
	}
	if show := e.mustLongarm(e.client, "trust", "show"); !strings.Contains(show, "carries the endorsement (device alice by alice") {
		t.Fatalf("trust show:\n%s", show)
	}
	if r := e.longarm(e.client, "trust", "init"); r.code == 0 {
		t.Fatal("a second trust init succeeded")
	}

	e.mustLongarm(e.client, "trust", "require", "on")
	r := e.longarm(e.client, "info", "w1")
	if r.code == 0 || !strings.Contains(r.stderr, "worker w1 refused by end-to-end trust") ||
		!strings.Contains(r.stderr, "longarmd trust request") || strings.Contains(r.stderr, `\"`) {
		t.Fatalf("unendorsed worker: exit %d\n%s", r.code, r.stderr)
	}

	// The worker's side (longarmd trust request / accept and its renewal
	// through the hub) is longarmd's: here its trust files are written
	// directly and its certificate is signed by the CA.
	ws := trust.Store{Dir: filepath.Join(e.dataDir, "trust")}
	req, err := ws.Request(pki.KindWorker, "w1", "")
	if err != nil {
		t.Fatal(err)
	}
	en, err := pki.ParseEndorsement(strings.TrimSpace(e.mustLongarm(e.client, "trust", "endorse", req.String())))
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.SetEndorsement(en, pki.KindWorker, "w1"); err != nil {
		t.Fatal(err)
	}
	ct, err := ws.CSRTrust()
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, csrPEM, err := pki.NewKeyAndCSR("w1", ct)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := pki.LoadCA(filepath.Join(e.client, "ca"))
	if err != nil {
		t.Fatal(err)
	}
	certPEM, err := ca.SignCSR(csrPEM, pki.IssueOptions{Role: pki.RoleWorker, Name: "w1", Validity: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := pki.WriteBundleAtomic(filepath.Join(e.dataDir, "tls"), ca.CertPEM, certPEM, keyPEM); err != nil {
		t.Fatal(err)
	}
	e.killLongarmd()
	e.startLongarmd()
	if out := e.mustLongarm(e.client, "info", "w1"); !strings.Contains(out, "w1") {
		t.Fatalf("info w1:\n%s", out)
	}

	// A denied node key is refused although endorsed; require off accepts any.
	e.mustLongarm(e.client, "trust", "deny-node", base64.StdEncoding.EncodeToString(ct.NodeKey.Public().(ed25519.PublicKey)))
	if r := e.longarm(e.client, "info", "w1"); r.code == 0 || !strings.Contains(r.stderr, "denied") {
		t.Fatalf("denied worker: exit %d\n%s", r.code, r.stderr)
	}
	e.mustLongarm(e.client, "trust", "require", "off")
	e.mustLongarm(e.client, "info", "w1")
}
