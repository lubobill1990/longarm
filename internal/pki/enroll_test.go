package pki

import (
	"crypto/tls"
	"crypto/x509"
	"path/filepath"
	"testing"
	"time"
)

func TestSignCSRUsesGivenIdentity(t *testing.T) {
	ca := mustCA(t, filepath.Join(t.TempDir(), "ca"))
	keyPEM, csrPEM, err := NewKeyAndCSR("evil-admin", nil) // the CSR subject is ignored
	if err != nil {
		t.Fatal(err)
	}
	certPEM, err := ca.SignCSR(csrPEM, IssueOptions{Role: RoleWorker, Name: "w7", Validity: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("cert does not match the CSR key: %v", err)
	}
	leaf, _ := ParseCertPEM(certPEM)
	id, err := IdentityFromCert(leaf)
	if err != nil || id != (Identity{Role: RoleWorker, Name: "w7"}) || leaf.Subject.CommonName != "w7" {
		t.Fatalf("identity %v %v cn=%s", id, err, leaf.Subject.CommonName)
	}
	_ = pair
	if _, err := ca.SignCSR([]byte("garbage"), IssueOptions{Role: RoleWorker, Name: "w7"}); err == nil {
		t.Fatal("garbage CSR accepted")
	}
	// hub certificates are server certificates with SANs
	hc, _, err := ca.Issue(IssueOptions{Role: RoleHub, Name: "hub", Hosts: []string{"203.0.113.10", "hub.example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	h, _ := ParseCertPEM(hc)
	if len(h.IPAddresses) != 1 || len(h.DNSNames) != 2 {
		t.Fatalf("hub SANs %v %v", h.IPAddresses, h.DNSNames)
	}
	if NormalizeFingerprint("AB:cd ef") != "abcdef" || len(Fingerprint(ca.Cert)) != 64 {
		t.Fatal("fingerprint helpers")
	}
}

func TestLiveBundleFollowsReload(t *testing.T) {
	root := t.TempDir()
	ca := mustCA(t, filepath.Join(root, "ca"))
	dir := filepath.Join(root, "tls")
	issue(t, ca, dir, RoleWorker, "w1")
	r, err := NewReloader(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	live := r.LiveBundle()
	first := Serial(live.Leaf())
	sc, err := live.ServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond) // distinct mtimes
	c, k, _ := ca.Issue(IssueOptions{Role: RoleWorker, Name: "w1"})
	if err := WriteBundleAtomic(dir, ca.CertPEM, c, k); err != nil {
		t.Fatal(err)
	}
	if changed, err := r.Check(); !changed || err != nil {
		t.Fatalf("reload: %v %v", changed, err)
	}
	got, _ := sc.GetCertificate(nil)
	if Serial(got.Leaf) == first || Serial(live.Leaf()) == first {
		t.Fatal("live bundle still presents the old certificate")
	}
	cc, _ := live.ClientConfig("w1").GetClientCertificate(nil)
	if Serial(cc.Leaf) != Serial(got.Leaf) {
		t.Fatal("client config not live")
	}
}

func TestRevocationList(t *testing.T) {
	root := t.TempDir()
	ca := mustCA(t, filepath.Join(root, "ca"))
	b := issue(t, ca, filepath.Join(root, "c"), RoleClient, "alice")
	path := filepath.Join(root, "revoked.json")
	rl, err := LoadRevocationList(path)
	if err != nil || rl.Revoked(b.Cert.Leaf) {
		t.Fatal(err)
	}
	err = rl.Replace(3, map[string]time.Time{
		Serial(b.Cert.Leaf): b.NotAfter,
		"dead":              time.Now().Add(-time.Hour), // expired entries are dropped
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rl.Revoked(b.Cert.Leaf) || rl.Len() != 1 {
		t.Fatal("not revoked")
	}
	if err := rl.VerifyNotRevoked(nil, [][]*x509.Certificate{{b.Cert.Leaf}}); err == nil {
		t.Fatal("VerifyNotRevoked accepted a revoked cert")
	}
	again, err := LoadRevocationList(path)
	if err != nil || !again.Revoked(b.Cert.Leaf) || again.Version() != 3 {
		t.Fatalf("persisted list: %v", err)
	}
}

func TestParseCSRBounds(t *testing.T) {
	_, csr, err := NewKeyAndCSR("n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseCSR(append(append([]byte{}, csr...), "\r\n"...)); err != nil {
		t.Fatalf("trailing whitespace: %v", err)
	}
	for what, in := range map[string][]byte{
		"trailing data": append(append([]byte{}, csr...), 'x'),
		"two blocks":    append(append([]byte{}, csr...), csr...),
		"too large":     append(append([]byte{}, csr...), make([]byte, MaxCSR)...),
	} {
		if _, err := ParseCSR(in); err == nil {
			t.Errorf("%s accepted", what)
		}
	}
}
