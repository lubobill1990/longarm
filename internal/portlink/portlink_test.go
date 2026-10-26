package portlink

import (
	"crypto/tls"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

func issue(t *testing.T, ca *pki.CA, dir string, o pki.IssueOptions) *pki.Bundle {
	t.Helper()
	c, k, err := ca.Issue(o)
	if err != nil {
		t.Fatal(err)
	}
	if err := pki.WriteBundle(dir, ca.CertPEM, c, k); err != nil {
		t.Fatal(err)
	}
	b, err := pki.LoadBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func grant(now time.Time) *pb.PortLinkGrant {
	return &pb.PortLinkGrant{Id: NewID(), Owner: "alice", From: "wa", To: "wb", Port: 8080, ListenPort: 18080,
		IssuedUnix: now.Unix(), ExpiresUnix: now.Add(time.Hour).Unix()}
}

func TestSignVerify(t *testing.T) {
	root := t.TempDir()
	ca, err := pki.InitCA(filepath.Join(root, "ca"), "test", time.Hour*24)
	if err != nil {
		t.Fatal(err)
	}
	hub := issue(t, ca, filepath.Join(root, "hub"), pki.IssueOptions{Role: pki.RoleHub, Name: "hub", Hosts: []string{"127.0.0.1"}})
	worker := issue(t, ca, filepath.Join(root, "w"), pki.IssueOptions{Role: pki.RoleWorker, Name: "wa"})
	other, err := pki.InitCA(filepath.Join(root, "ca2"), "other", time.Hour*24)
	if err != nil {
		t.Fatal(err)
	}
	otherHub := issue(t, other, filepath.Join(root, "hub2"), pki.IssueOptions{Role: pki.RoleHub, Name: "hub"})
	now := time.Now()

	g := grant(now)
	s, err := Sign(hub.Certificate(), g)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Verify(worker.Pool, s, now)
	if err != nil || !proto.Equal(got, g) {
		t.Fatalf("verify: %v %v", got, err)
	}

	bad := func(name string, s *pb.SignedPortLinkGrant, at time.Time, want string) {
		t.Helper()
		if _, err := Verify(worker.Pool, s, at); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: want error with %q, got %v", name, want, err)
		}
	}
	bad("expired", s, now.Add(2*time.Hour), "expired")
	bad("no grant", nil, now, "no link grant")

	// tampering: another port, another worker, a flipped signature byte
	for _, mod := range []func(*pb.PortLinkGrant){
		func(g *pb.PortLinkGrant) { g.Port = 22 },
		func(g *pb.PortLinkGrant) { g.From = "evil" },
		func(g *pb.PortLinkGrant) { g.ExpiresUnix += 3600 * 24 * 365 },
	} {
		g2 := proto.Clone(g).(*pb.PortLinkGrant)
		mod(g2)
		b, _ := proto.Marshal(g2)
		s2 := proto.Clone(s).(*pb.SignedPortLinkGrant)
		s2.Grant = b
		bad("tampered grant", s2, now, "bad signature")
	}
	s3 := proto.Clone(s).(*pb.SignedPortLinkGrant)
	s3.Signature[len(s3.Signature)/2] ^= 0xff
	bad("flipped signature", s3, now, "")

	// signed by a worker certificate (a compromised worker forging grants)
	sw, err := Sign(worker.Certificate(), g)
	if err != nil {
		t.Fatal(err)
	}
	bad("worker-signed", sw, now, "not by the hub")
	// a hub of another CA
	so, err := Sign(otherHub.Certificate(), g)
	if err != nil {
		t.Fatal(err)
	}
	bad("foreign CA", so, now, "certificate")
	// the real hub's signature with a foreign certificate attached
	s4 := proto.Clone(s).(*pb.SignedPortLinkGrant)
	s4.Chain = so.Chain
	bad("swapped chain", s4, now, "certificate")

	// issued in the future
	gf := grant(now.Add(time.Hour))
	sf, _ := Sign(hub.Certificate(), gf)
	bad("future", sf, now, "future")
	// incomplete grant
	gi := grant(now)
	gi.Id = "x"
	si, _ := Sign(hub.Certificate(), gi)
	bad("bad id", si, now, "incomplete")

	if _, err := Sign(&tls.Certificate{}, g); err == nil {
		t.Fatal("signed without a key")
	}
}

func TestEnv(t *testing.T) {
	if Env(nil) != nil {
		t.Fatal("env without links")
	}
	env := Env([]Link{{To: "wb", Port: 8080, ListenPort: 18080}, {To: "db-1.x", Port: 5432, ListenPort: 15432}})
	if env[EnvLinks] != "db-1.x:5432=127.0.0.1:15432,wb:8080=127.0.0.1:18080" {
		t.Fatalf("LONGARM_LINKS = %q", env[EnvLinks])
	}
	if env["LONGARM_LINK_WB_8080"] != "127.0.0.1:18080" || env["LONGARM_LINK_DB_1_X_5432"] != "127.0.0.1:15432" || len(env) != 3 {
		t.Fatalf("env %v", env)
	}
	if !IsEnv("LONGARM_LINK_WB_8080") || !IsEnv(EnvLinks) || IsEnv("PATH") {
		t.Fatal("IsEnv")
	}
	if !ValidID(NewID()) || ValidID("lk-../../x") {
		t.Fatal("ids")
	}
}

func TestEnvClash(t *testing.T) {
	env := Env([]Link{{To: "a-b", Port: 80, ListenPort: 18080}, {To: "a_b", Port: 80, ListenPort: 18081}})
	if _, ok := env["LONGARM_LINK_A_B_80"]; ok || env[EnvLinks] != "a-b:80=127.0.0.1:18080,a_b:80=127.0.0.1:18081" {
		t.Fatalf("env %v", env)
	}
}
