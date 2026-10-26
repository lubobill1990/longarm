package pki

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// trustFixture is a CA, alice's user key and helpers to issue certificates
// the way the hub does (SignCSR) or the way a thief of the CA key could.
type trustFixture struct {
	t     *testing.T
	ca    *CA
	alice ed25519.PrivateKey
}

func newTrustFixture(t *testing.T) *trustFixture {
	ca, err := InitCA(filepath.Join(t.TempDir(), "ca"), "test CA", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, alice, _ := ed25519.GenerateKey(rand.Reader)
	return &trustFixture{t: t, ca: ca, alice: alice}
}

func newNodeKey() ed25519.PrivateKey {
	_, k, _ := ed25519.GenerateKey(rand.Reader)
	return k
}

// issue runs a CSR with tr through SignCSR; it returns the certificate and
// its TLS key pair.
func (f *trustFixture) issue(o IssueOptions, tr *CSRTrust) (*x509.Certificate, tls.Certificate) {
	f.t.Helper()
	keyPEM, csrPEM, err := NewKeyAndCSR(o.Name, tr)
	if err != nil {
		f.t.Fatal(err)
	}
	certPEM, err := f.ca.SignCSR(csrPEM, o)
	if err != nil {
		f.t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		f.t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(pair.Certificate[0])
	pair.Leaf = leaf
	return leaf, pair
}

func (f *trustFixture) endorsed(user, kind, name string, nk ed25519.PrivateKey) *CSRTrust {
	f.t.Helper()
	e, err := Endorse(f.alice, user, kind, name, nk.Public().(ed25519.PublicKey))
	if err != nil {
		f.t.Fatal(err)
	}
	return &CSRTrust{NodeKey: nk, Endorsement: e}
}

func (f *trustFixture) policy() *TrustPolicy {
	return &TrustPolicy{Users: map[string]ed25519.PublicKey{"alice": f.alice.Public().(ed25519.PublicKey)}}
}

func TestTrustCheck(t *testing.T) {
	f := newTrustFixture(t)
	p := f.policy()
	laptopKey := newNodeKey()
	laptop := IssueOptions{Role: RoleAdmin, Name: "alice-laptop", User: "alice"}
	good, _ := f.issue(laptop, f.endorsed("alice", KindDevice, "alice-laptop", laptopKey))
	if u, err := p.Check(good); err != nil || u != "alice" {
		t.Fatalf("endorsed device: %q %v", u, err)
	}
	w1Key := newNodeKey()
	w1, _ := f.issue(IssueOptions{Role: RoleWorker, Name: "w1"}, f.endorsed("alice", KindWorker, "w1", w1Key))
	if _, err := p.Check(w1); err != nil {
		t.Fatalf("endorsed worker: %v", err)
	}

	mallory := newNodeKey()
	other := newNodeKey()
	cases := []struct {
		name string
		cert func() *x509.Certificate
		want string
	}{
		{"the CA mints alice a certificate (no proof)", func() *x509.Certificate {
			c, _ := f.issue(laptop, nil)
			return c
		}, "no end-to-end trust proof"},
		{"bound but not endorsed", func() *x509.Certificate {
			c, _ := f.issue(laptop, &CSRTrust{NodeKey: other})
			return c
		}, "not endorsed"},
		{"the CA copies alice's extension into its own certificate", func() *x509.Certificate {
			ext := findTrustExt(good.Extensions)
			o := laptop
			o.extra = append(o.extra, *ext)
			certPEM, _, err := f.ca.Issue(o)
			if err != nil {
				t.Fatal(err)
			}
			c, _ := ParseCertPEM(certPEM)
			return c
		}, "did not sign this certificate's key"},
		{"endorsed by an untrusted key", func() *x509.Certificate {
			e, _ := Endorse(mallory, "alice", KindDevice, "alice-laptop", other.Public().(ed25519.PublicKey))
			c, _ := f.issue(laptop, &CSRTrust{NodeKey: other, Endorsement: e})
			return c
		}, "not trusted for user"},
		{"alice's endorsement reused under another name", func() *x509.Certificate {
			c, _ := f.issue(IssueOptions{Role: RoleAdmin, Name: "evil", User: "alice"}, f.endorsed("alice", KindDevice, "alice-laptop", other))
			return c
		}, "endorsed as \"alice-laptop\""},
		{"a device endorsement on a worker", func() *x509.Certificate {
			c, _ := f.issue(IssueOptions{Role: RoleWorker, Name: "w2"}, f.endorsed("alice", KindDevice, "w2", other))
			return c
		}, "not a worker"},
		{"a worker endorsement on an admin", func() *x509.Certificate {
			c, _ := f.issue(IssueOptions{Role: RoleAdmin, Name: "w3", User: "alice"}, f.endorsed("alice", KindWorker, "w3", other))
			return c
		}, "not a device"},
		{"alice's key endorsing bob's device", func() *x509.Certificate {
			c, _ := f.issue(IssueOptions{Role: RoleClient, Name: "bob-pc", User: "bob"}, f.endorsed("alice", KindDevice, "bob-pc", other))
			return c
		}, "a device of bob, endorsed by alice"},
		{"a tampered endorsement", func() *x509.Certificate {
			tr := f.endorsed("alice", KindDevice, "alice-laptop", other)
			tr.Endorsement.Issued++
			c, _ := f.issue(laptop, tr)
			return c
		}, "bad signature"},
	}
	for _, c := range cases {
		if _, err := p.Check(c.cert()); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}

	if _, err := (&TrustPolicy{DenyAll: true}).Check(good); err == nil || !(&TrustPolicy{DenyAll: true}).Active() {
		t.Error("a deny-all policy accepted an endorsed certificate")
	}
	p.DenyNodes = [][]byte{laptopKey.Public().(ed25519.PublicKey)}
	if _, err := p.Check(good); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Errorf("denied node: %v", err)
	}
	if (&TrustPolicy{}).Active() || (*TrustPolicy)(nil).Active() || !p.Active() {
		t.Error("Active")
	}
}

// A CSR whose extension binds another key is refused by the hub.
func TestSignCSRTrust(t *testing.T) {
	f := newTrustFixture(t)
	nk := newNodeKey()
	// an extension bound to another key than the CSR's: refused
	other, _ := newKey()
	ext, err := TrustExtension(nk, &other.PublicKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := newKey()
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{ExtraExtensions: []pkix.Extension{ext}}, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.ca.SignCSR(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), IssueOptions{Role: RoleWorker, Name: "w1"}); err == nil ||
		!strings.Contains(err.Error(), "did not sign") {
		t.Fatalf("a CSR with a foreign binding: %v", err)
	}
	keyPEM, csrPEM, err := NewKeyAndCSR("w1", &CSRTrust{NodeKey: nk})
	if err != nil || keyPEM == nil {
		t.Fatal(err)
	}
	if _, err := f.ca.SignCSR(csrPEM, IssueOptions{Role: RoleWorker, Name: "w1"}); err != nil {
		t.Fatal("a valid CSR:", err)
	}
	// renewal without trust material: no extension
	c, _ := f.issue(IssueOptions{Role: RoleWorker, Name: "w1"}, nil)
	if tx, err := TrustOf(c); tx != nil || err != nil {
		t.Fatalf("no extension expected: %v %v", tx, err)
	}
	// an endorsement for another node key is refused when building
	e, _ := Endorse(f.alice, "alice", KindWorker, "w1", newNodeKey().Public().(ed25519.PublicKey))
	if _, _, err := NewKeyAndCSR("w1", &CSRTrust{NodeKey: nk, Endorsement: e}); err == nil {
		t.Fatal("mismatched endorsement accepted")
	}
}

func TestTrustLines(t *testing.T) {
	f := newTrustFixture(t)
	l := UserKeyLine{User: "alice", Key: f.alice.Public().(ed25519.PublicKey)}
	if got, err := ParseUserKeyLine(" " + l.String() + "\n"); err != nil || got.User != "alice" || !ed25519.PublicKey(got.Key).Equal(f.alice.Public()) {
		t.Fatalf("user key line: %+v %v", got, err)
	}
	nk := newNodeKey()
	r := EndorseRequest{Kind: KindWorker, Name: "w1", NodeKey: nk.Public().(ed25519.PublicKey)}
	if got, err := ParseEndorseRequest(r.String()); err != nil || got.Name != "w1" {
		t.Fatalf("request: %+v %v", got, err)
	}
	e, _ := Endorse(f.alice, "alice", KindWorker, "w1", r.NodeKey)
	if got, err := ParseEndorsement(e.String()); err != nil || got.Name != "w1" {
		t.Fatalf("endorsement: %+v %v", got, err)
	}
	bad := e.String()
	bad = bad[:len(bad)-4] + "AAAA"
	if _, err := ParseEndorsement(bad); err == nil {
		t.Fatal("a damaged endorsement line parsed")
	}
	if _, err := ParseUserKeyLine(r.String()); err == nil {
		t.Fatal("a request parsed as a user key")
	}
}

// The policy is enforced in the TLS handshake, both ways.
func TestTrustHandshake(t *testing.T) {
	f := newTrustFixture(t)
	pool := x509.NewCertPool()
	pool.AddCert(f.ca.Cert)
	bundle := func(o IssueOptions, tr *CSRTrust, src *TrustSource) *Bundle {
		leaf, pair := f.issue(o, tr)
		id, _ := IdentityFromCert(leaf)
		return &Bundle{Pool: pool, Cert: pair, Identity: id, NotAfter: leaf.NotAfter, Trust: src}
	}
	on := &TrustSource{}
	on.Store(f.policy())
	var denied []string
	on.Denied = func(err error) { denied = append(denied, err.Error()) }

	handshake := func(server, client *Bundle, asked ...string) (serr, cerr error) {
		name := server.Identity.Name
		if len(asked) > 0 {
			name = asked[0]
		}
		scfg, err := server.ServerConfig()
		if err != nil {
			t.Fatal(err)
		}
		// real sockets: net.Pipe is unbuffered and both sides write alerts
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		done := make(chan error, 1)
		go func() {
			a, err := ln.Accept()
			if err != nil {
				done <- err
				return
			}
			s := tls.Server(a, scfg)
			err = s.Handshake()
			if err == nil {
				s.Read(make([]byte, 1))
				s.Write([]byte{0})
			}
			done <- err
			s.Close()
		}()
		b, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		b.SetDeadline(time.Now().Add(10 * time.Second))
		c := tls.Client(b, client.ClientConfig(name))
		cerr = c.Handshake()
		if cerr == nil {
			c.Write([]byte{0}) // TLS 1.3: the client learns of a refused certificate on first use
			c.Read(make([]byte, 1))
		}
		c.Close()
		return <-done, cerr
	}

	wGood := bundle(IssueOptions{Role: RoleWorker, Name: "w1"}, f.endorsed("alice", KindWorker, "w1", newNodeKey()), on)
	cGood := bundle(IssueOptions{Role: RoleAdmin, Name: "alice-laptop", User: "alice"}, f.endorsed("alice", KindDevice, "alice-laptop", newNodeKey()), on)
	if serr, cerr := handshake(wGood, cGood); serr != nil || cerr != nil {
		t.Fatalf("endorsed both ways: %v / %v", serr, cerr)
	}

	// a CA-minted admin certificate for alice: the worker refuses it
	cForged := bundle(IssueOptions{Role: RoleAdmin, Name: "alice-laptop", User: "alice"}, nil, nil)
	if serr, _ := handshake(wGood, cForged); serr == nil || !strings.Contains(serr.Error(), "no end-to-end trust proof") {
		t.Fatalf("forged client accepted: %v", serr)
	}
	if len(denied) != 1 {
		t.Fatalf("Denied hook: %v", denied)
	}

	// a CA-minted worker impersonating w1: a client that requires trust refuses it
	wForged := bundle(IssueOptions{Role: RoleWorker, Name: "w1"}, nil, nil)
	if _, cerr := handshake(wForged, cGood); cerr == nil || !strings.Contains(cerr.Error(), "end-to-end trust") {
		t.Fatalf("forged worker accepted: %v", cerr)
	}

	// another endorsed worker, with a CA-issued DNS name of w1: a client
	// asking for w1 refuses it (the user signed "w2", not "w1")
	w2 := bundle(IssueOptions{Role: RoleWorker, Name: "w2", Hosts: []string{"w1"}}, f.endorsed("alice", KindWorker, "w2", newNodeKey()), on)
	if _, cerr := handshake(w2, cGood, "w1"); cerr == nil || !strings.Contains(cerr.Error(), `asked for worker "w1"`) {
		t.Fatalf("w2 posing as w1: %v", cerr)
	}

	// policies off (nil source / no users): today's behavior
	off := &TrustSource{}
	off.Store(&TrustPolicy{})
	if serr, cerr := handshake(bundle(IssueOptions{Role: RoleWorker, Name: "w2"}, nil, off), bundle(IssueOptions{Role: RoleClient, Name: "c"}, nil, nil)); serr != nil || cerr != nil {
		t.Fatalf("no policy: %v / %v", serr, cerr)
	}

	// a policy change applies to the next handshake
	on.Store(&TrustPolicy{Users: map[string]ed25519.PublicKey{"bob": newNodeKey().Public().(ed25519.PublicKey)}})
	if serr, _ := handshake(wGood, cGood); serr == nil {
		t.Fatal("alice still trusted after the policy changed")
	}
}
