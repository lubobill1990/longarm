package hubsync

import (
	"crypto/x509"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/relay"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

func issue(t *testing.T, ca *pki.CA, role pki.Role, name string) *pki.Bundle {
	t.Helper()
	certPEM, keyPEM, err := ca.Issue(pki.IssueOptions{Role: role, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	b, err := pki.ParseBundle(ca.CertPEM, certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestPolicyCacheAndTTL: no session before a policy; a policy installs
// revocations and worker pairs and is cached on disk, so a restarted relay
// (hub down) starts with it; worker pairs end after the TTL without the
// hub, revocations stay.
func TestPolicyCacheAndTTL(t *testing.T) {
	dir := t.TempDir()
	ca, err := pki.InitCA(filepath.Join(dir, "ca"), "test", 0)
	if err != nil {
		t.Fatal(err)
	}
	rb := issue(t, ca, pki.RoleRelay, "relay1").Live()
	alice := issue(t, ca, pki.RoleClient, "alice").Certificate().Leaf
	bob := issue(t, ca, pki.RoleClient, "bob").Certificate().Leaf
	chains := func(c *x509.Certificate) [][]*x509.Certificate { return [][]*x509.Certificate{{c}} }
	state := filepath.Join(dir, "state")
	newSync := func() *Sync {
		tc, err := relay.ServerTLS(rb)
		if err != nil {
			t.Fatal(err)
		}
		s, err := New(Config{Hub: "127.0.0.1:1", Bundle: rb, StateDir: state, Server: &relay.Server{TLS: tc}})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	s := newSync()
	if err := s.VerifyPeerCertificate(nil, chains(alice)); err == nil {
		t.Fatal("a relay without a policy accepted a session")
	}
	// a hub that never revoked anything (version 0): still a policy, cached
	s.apply(&pb.RelayPolicy{Time: timestamppb.Now(), Revocations: &pb.Revocations{}})
	if st := newSync().Status(); !st.Ready || st.Revoked != 0 {
		t.Fatalf("restart after a version-0 policy: %+v", st)
	}
	s.apply(&pb.RelayPolicy{Time: timestamppb.Now(), Ttl: durationpb.New(time.Hour),
		Revocations: &pb.Revocations{Version: 3, Certs: []*pb.RevokedCert{{Serial: pki.Serial(bob), NotAfter: timestamppb.New(bob.NotAfter)}}},
		WorkerPairs: []*pb.WorkerPair{{A: "w1", B: "w2"}}})
	st := s.Status()
	if !st.Ready || st.Revoked != 1 || st.WorkerPairs != 1 || !st.PairsActive {
		t.Fatalf("after a policy: %+v", st)
	}
	if err := s.VerifyPeerCertificate(nil, chains(alice)); err != nil {
		t.Fatalf("alice refused: %v", err)
	}
	if err := s.VerifyPeerCertificate(nil, chains(bob)); err == nil {
		t.Fatal("revoked bob accepted")
	}

	// a restart while the hub is away: the cached policy
	s2 := newSync()
	st = s2.Status()
	if !st.Ready || st.Revoked != 1 || st.WorkerPairs != 1 || !st.PairsActive || st.Connected {
		t.Fatalf("from the cache: %+v", st)
	}
	if err := s2.VerifyPeerCertificate(nil, chains(bob)); err == nil {
		t.Fatal("revoked bob accepted after a restart")
	}

	// no word from the hub for longer than the TTL: no worker pairs, the
	// revocation list stays
	s2.mu.Lock()
	s2.contact = time.Now().Add(-2 * time.Hour)
	s2.mu.Unlock()
	s2.applyPairs(time.Now())
	st = s2.Status()
	if st.PairsActive || !st.Ready || st.Revoked != 1 {
		t.Fatalf("after the TTL: %+v", st)
	}
	if err := s2.VerifyPeerCertificate(nil, chains(alice)); err != nil {
		t.Fatalf("alice refused after the TTL: %v", err)
	}
}
