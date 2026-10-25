package pki

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func mustCA(t *testing.T, dir string) *CA {
	t.Helper()
	ca, err := InitCA(dir, "test-ca", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func issue(t *testing.T, ca *CA, dir string, role Role, name string) *Bundle {
	t.Helper()
	c, k, err := ca.Issue(IssueOptions{Role: role, Name: name, Hosts: []string{"127.0.0.1", "example.ts.net"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteBundle(dir, ca.CertPEM, c, k); err != nil {
		t.Fatal(err)
	}
	b, err := LoadBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRolesRoundTrip(t *testing.T) {
	root := t.TempDir()
	ca := mustCA(t, filepath.Join(root, "ca"))
	if _, err := InitCA(filepath.Join(root, "ca"), "again", time.Hour); err == nil {
		t.Fatal("InitCA must refuse to overwrite")
	}
	ca2, err := LoadCA(filepath.Join(root, "ca"))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []Role{RoleWorker, RoleClient, RoleAdmin, RoleRelay, RoleRelease} {
		b := issue(t, ca2, filepath.Join(root, string(r)), r, "n-"+string(r))
		if b.Identity != (Identity{Role: r, Name: "n-" + string(r)}) {
			t.Errorf("identity = %v", b.Identity)
		}
		leaf := b.Cert.Leaf
		if leaf.Subject.OrganizationalUnit[0] != string(r) {
			t.Errorf("OU = %v", leaf.Subject.OrganizationalUnit)
		}
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: b.Pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
			t.Errorf("%s: verify: %v", r, err)
		}
		wantDays := 30
		if r == RoleRelay {
			wantDays = 90
			if leaf.VerifyHostname("n-relay") != nil {
				t.Errorf("relay SANs: %v", leaf.DNSNames)
			}
		}
		if r == RoleRelease {
			wantDays = 180
			if leaf.VerifyHostname("n-release") == nil || slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth) {
				t.Errorf("release certs are client-only: %v %v", leaf.DNSNames, leaf.ExtKeyUsage)
			}
		}
		if r == RoleWorker {
			wantDays = 90
			if leaf.VerifyHostname("n-worker") != nil || leaf.VerifyHostname("127.0.0.1") != nil {
				t.Errorf("worker SANs: %v %v", leaf.DNSNames, leaf.IPAddresses)
			}
		}
		if d := time.Until(leaf.NotAfter); d < time.Duration(wantDays-1)*24*time.Hour || d > time.Duration(wantDays)*24*time.Hour {
			t.Errorf("%s validity %v", r, d)
		}
	}
	_ = ca
	if _, err := ParseRole("root"); err == nil {
		t.Fatal("bad role accepted")
	}
	if _, _, err := ca.Issue(IssueOptions{Role: RoleClient, Name: "bad/name"}); err == nil {
		t.Fatal("bad name accepted")
	}
}

// handshake runs a TLS handshake between a server bundle and a client config.
func handshake(t *testing.T, srv *tls.Config, cli *tls.Config) (Identity, error) {
	t.Helper()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	type res struct {
		id  Identity
		err error
	}
	ch := make(chan res, 1)
	go func() {
		s := tls.Server(a, srv)
		err := s.Handshake()
		if err != nil {
			ch <- res{err: err}
			return
		}
		id, err := IdentityFromCert(s.ConnectionState().PeerCertificates[0])
		ch <- res{id, err}
	}()
	c := tls.Client(b, cli)
	cerr := c.Handshake()
	if cerr != nil {
		b.Close()
	}
	r := <-ch
	if cerr != nil {
		return Identity{}, cerr
	}
	return r.id, r.err
}

func TestMTLS(t *testing.T) {
	root := t.TempDir()
	ca := mustCA(t, filepath.Join(root, "ca"))
	worker := issue(t, ca, filepath.Join(root, "w"), RoleWorker, "w1")
	client := issue(t, ca, filepath.Join(root, "c"), RoleClient, "alice")
	admin := issue(t, ca, filepath.Join(root, "a"), RoleAdmin, "root-user")
	otherCA := mustCA(t, filepath.Join(root, "ca2"))
	stranger := issue(t, otherCA, filepath.Join(root, "s"), RoleAdmin, "mallory")
	strangerWorker := issue(t, otherCA, filepath.Join(root, "sw"), RoleWorker, "w1")

	srv, err := worker.ServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ServerConfig(); err == nil {
		t.Fatal("client cert must not serve")
	}

	id, err := handshake(t, srv, client.ClientConfig("w1"))
	if err != nil || id != (Identity{RoleClient, "alice"}) {
		t.Fatalf("client handshake: %v %v", id, err)
	}
	id, err = handshake(t, srv, admin.ClientConfig("w1"))
	if err != nil || id.Role != RoleAdmin {
		t.Fatalf("admin handshake: %v %v", id, err)
	}
	// Different CA as client: the server rejects it.
	if _, err := handshake(t, srv, stranger.ClientConfig("w1")); err == nil {
		t.Fatal("cert from a different CA was accepted")
	}
	// Different CA as server: the client rejects it.
	ssrv, _ := strangerWorker.ServerConfig()
	if _, err := handshake(t, ssrv, client.ClientConfig("w1")); err == nil {
		t.Fatal("server from a different CA was accepted")
	}
	// Wrong worker name.
	if _, err := handshake(t, srv, client.ClientConfig("w2")); err == nil || !strings.Contains(err.Error(), "w2") {
		t.Fatalf("expected name mismatch, got %v", err)
	}
	// A client certificate posing as a server is rejected by role check.
	fake := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{client.Cert},
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: client.Pool}
	cc := admin.ClientConfig("alice")
	cc.InsecureSkipVerify = true // isolate the VerifyConnection role check
	if _, err := handshake(t, fake, cc); err == nil || !strings.Contains(err.Error(), "not a worker") {
		t.Fatalf("expected role rejection, got %v", err)
	}
}
