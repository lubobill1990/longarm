package hub

import (
	"bytes"
	"crypto/tls"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/nodeid"
	"github.com/lubobill1990/longarm/internal/pki"
)

func TestTOTPVectors(t *testing.T) {
	// RFC 6238 appendix B (SHA-1, secret "12345678901234567890"), 6 digits.
	secret := b32.EncodeToString([]byte("12345678901234567890"))
	for _, c := range []struct {
		unix int64
		want string
	}{{59, "287082"}, {1111111109, "081804"}, {1234567890, "005924"}, {2000000000, "279037"}} {
		got, err := TOTP(secret, time.Unix(c.unix, 0))
		if err != nil || got != c.want {
			t.Errorf("TOTP(%d) = %s %v, want %s", c.unix, got, err, c.want)
		}
	}
	now := time.Unix(1234567890, 0)
	code, _ := TOTP(secret, now)
	step, err := checkTOTP(secret, code, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := checkTOTP(secret, code, step, now); err == nil {
		t.Fatal("replayed code accepted")
	}
	prev, _ := TOTP(secret, now.Add(-30*time.Second))
	if _, err := checkTOTP(secret, prev, 0, now); err != nil {
		t.Fatal("previous step not accepted")
	}
	old, _ := TOTP(secret, now.Add(-90*time.Second))
	if _, err := checkTOTP(secret, old, 0, now); err == nil {
		t.Fatal("old code accepted")
	}
}

func TestPassword(t *testing.T) {
	h := hashPassword("correct horse")
	if !strings.HasPrefix(h, "$argon2id$v=19$") || !checkPassword(h, "correct horse") || checkPassword(h, "wrong") || checkPassword("garbage", "x") {
		t.Fatal("argon2id round trip")
	}
	if h == hashPassword("correct horse") {
		t.Fatal("no salt")
	}
}

func TestJoinCodes(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		c := newJoinCode()
		if len(c) != 7 || c[3] != '-' || strings.ContainsAny(c, "ILOU") {
			t.Fatalf("code %q", c)
		}
		seen[c] = true
	}
	if len(seen) < 195 {
		t.Fatal("codes repeat")
	}
	if normalizeCode("k7p-q2m") != "K7PQ2M" || normalizeCode("o1l i") != "0111" || codeHash("K7P-Q2M") != codeHash("k7pq2m") {
		t.Fatal("normalize")
	}
	if sanitizeName("My-PC.corp.example") != "my-pc" || sanitizeName("WIN 11") != "win-11" {
		t.Fatalf("sanitize %q", sanitizeName("My-PC.corp.example"))
	}
}

func TestStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.SchemaVersion() != 6 { // 1 initial, 2 release channels/signatures (P2), 3 tenants, 4 port links, 5 audit attribution, 6 node IDs
		st.Close()
		t.Fatalf("schema %d", st.SchemaVersion())
	}
	now := time.Now()
	jc := &store.JoinCode{ID: "abc", Role: "worker", ExpiresAt: now.Add(time.Minute)}
	if err := st.AddJoinCode("h1", jc); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UseJoinCode("h1", "w1", now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UseJoinCode("h1", "w2", now); err != store.ErrNotFound {
		t.Fatalf("code used twice: %v", err)
	}
	st.AddJoinCode("h2", &store.JoinCode{ID: "def", ExpiresAt: now.Add(-time.Second)})
	if _, err := st.PeekJoinCode("h2", now); err != store.ErrNotFound {
		t.Fatal("expired code usable")
	}
	w1, other := nodeid.New(), nodeid.New()
	st.AddCert(store.Cert{Serial: "s1", Node: "w1", NodeID: w1, NotAfter: now.Add(time.Hour), IssuedAt: now})
	st.AddCert(store.Cert{Serial: "s1", Node: "w1", NodeID: w1, NotAfter: now.Add(time.Hour), IssuedAt: now}) // idempotent
	st.AddCert(store.Cert{Serial: "s2", Node: "w1", NodeID: w1, NotAfter: now.Add(-time.Hour), IssuedAt: now})
	st.AddCert(store.Cert{Serial: "s3", Node: "w1", NodeID: other, NotAfter: now.Add(time.Hour), IssuedAt: now}) // another node named w1
	if n, _ := st.RevokeNodeCerts(w1, now); n != 2 {
		t.Fatalf("revoked %d", n)
	}
	if r, _ := st.CertRevoked("s3"); r {
		t.Fatal("revoking a node revoked another node's certificate of the same name")
	}
	rc, _ := st.RevokedCerts(now)
	if len(rc) != 1 || rc[0].Serial != "s1" {
		t.Fatalf("revoked list %v (expired ones are not listed)", rc)
	}
	if r, _ := st.CertRevoked("s1"); !r {
		t.Fatal("CertRevoked")
	}
	st.PutNode(&store.Node{ID: nodeid.New(), Name: "w1", Role: "worker", State: "approved", Labels: map[string]string{"a": "b"}})
	st.Close()
	// Reopen: migrations are not re-applied, data is there.
	st, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	nodes, _ := st.Nodes()
	if len(nodes) != 1 || nodes[0].Labels["a"] != "b" {
		t.Fatalf("nodes %v", nodes)
	}
	if v, _ := st.Meta("schema_version"); v != "6" {
		t.Fatal(v)
	}
}

// TestALPNMux: TLS connections are routed by ALPN without terminating TLS,
// and the replayed bytes let the real server complete the handshake.
func TestALPNMux(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := newALPNMux(ln)
	relayLn, grpcLn := m.Route("longnet-relay/1"), m.Default()
	go m.Serve()
	defer m.Close()
	cert := selfSigned(t)
	serve := func(l net.Listener, tag string) {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2", "longnet-relay/1"}})
				defer tc.Close()
				io.WriteString(tc, tag)
			}()
		}
	}
	go serve(relayLn, "relay")
	go serve(grpcLn, "grpc")
	for _, c := range []struct{ alpn, want string }{{"longnet-relay/1", "relay"}, {"h2", "grpc"}, {"", "grpc"}} {
		cfg := &tls.Config{InsecureSkipVerify: true}
		if c.alpn != "" {
			cfg.NextProtos = []string{c.alpn}
		}
		conn, err := tls.Dial("tcp", ln.Addr().String(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(conn)
		conn.Close()
		if string(b) != c.want {
			t.Errorf("alpn %q -> %q, want %q", c.alpn, b, c.want)
		}
	}
	// Non-TLS bytes go to the default route unchanged.
	conn, _ := net.Dial("tcp", ln.Addr().String())
	conn.Write([]byte("GET / HTTP/1.0\r\n\r\n"))
	conn.Close()
	if got := parseALPN([]byte{1, 2, 3}); got != nil {
		t.Fatal("garbage parsed")
	}
	_, alpn := peekClientHello(bytes.NewReader([]byte("not tls at all")))
	if alpn != nil {
		t.Fatal("non-TLS has ALPN")
	}
	if err := grpcLn.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := grpcLn.Accept(); err == nil {
		t.Fatal("closed route still accepts")
	}
}

func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	ca, err := pki.InitCA(filepath.Join(t.TempDir(), "ca"), "t", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c, k, err := ca.Issue(pki.IssueOptions{Role: pki.RoleRelay, Name: "x", Hosts: []string{"127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(c, k)
	if err != nil {
		t.Fatal(err)
	}
	return pair
}

func TestExampleConfig(t *testing.T) {
	c, err := LoadConfig("../../deploy/hub/hub.toml.example")
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":7443" || c.RelayUDP != ":7443" || c.SchedParams().GateFreeGB != 20 || c.SchedParams().MemGB["build"] != 8 ||
		c.TTL("worker") != 90*24*time.Hour || c.RenewBefore("client") != 10*24*time.Hour || len(c.Web.TrustedProxies) != 2 {
		t.Fatalf("%+v", c)
	}
	d, err := LoadConfig("")
	if err != nil || d.RelayUDP != ":7443" || d.RelaySTUN2 != "auto" || d.HTTP != "127.0.0.1:7480" || d.Heartbeat.Offline.Duration != 10*time.Second {
		t.Fatalf("defaults %+v %v", d, err)
	}
	if c.Tailnet.Provider != ProviderTailscale || c.Tailnet.ControlURL() != DefaultTailscaleControlURL || !*c.Tailnet.EphemeralClients ||
		c.Tailnet.Tailscale.OAuthSecretFile != "/etc/longarm-hub/tailscale-oauth.secret" || c.Tailnet.KeyTTL.Duration != time.Hour {
		t.Fatalf("example tailnet %+v", c.Tailnet)
	}
	if c.RelaySTUN2 != ":7444" || len(c.RelaySTUNAdvertise) != 0 {
		t.Fatalf("example relay_stun2 %q advertise %v", c.RelaySTUN2, c.RelaySTUNAdvertise)
	}
	for _, bad := range []Config{{RelaySTUN2: "7444"}, {RelaySTUNAdvertise: []string{"stun.example.com"}}} {
		if err := bad.applyDefaults(); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	ok := Config{RelaySTUN2: "off", RelaySTUNAdvertise: []string{":7443", ":7444", "stun.l.google.com:19302"}}
	if err := ok.applyDefaults(); err != nil {
		t.Fatal(err)
	}
}

func TestRenewBeforeValidated(t *testing.T) {
	c := &Config{}
	c.Certs.ClientTTL.Duration, c.Certs.RenewBefore.Duration = 24*time.Hour, 24*time.Hour
	if err := c.applyDefaults(); err == nil {
		t.Fatal("renew_before >= client_ttl accepted")
	}
}
