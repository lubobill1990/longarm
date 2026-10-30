package client

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/trust"
)

// requireWorkers turns on the client's worker check with alice's user key
// (held by the client) and returns that key.
func (e *raceEnv) requireWorkers() ed25519.PrivateKey {
	ts := TrustStore(e.d.cfg.Dir)
	k, err := ts.CreateUserKey("alice")
	if err != nil {
		e.t.Fatal(err)
	}
	if err := ts.SetRequireWorkers(true); err != nil {
		e.t.Fatal(err)
	}
	p, err := ts.Policy(true)
	if err != nil {
		e.t.Fatal(err)
	}
	e.d.bundle.Trust.Store(p)
	return k
}

// endorsedWorker is a bundle for worker name whose certificate carries an
// endorsement by userKey.
func (e *raceEnv) endorsedWorker(name string, userKey ed25519.PrivateKey) *pki.Bundle {
	ws := trust.Store{Dir: filepath.Join(e.t.TempDir(), "trust")}
	nk, err := ws.NodeKey()
	if err != nil {
		e.t.Fatal(err)
	}
	en, err := pki.Endorse(userKey, "alice", pki.KindWorker, name, nk.Public().(ed25519.PublicKey))
	if err != nil {
		e.t.Fatal(err)
	}
	keyPEM, csrPEM, err := pki.NewKeyAndCSR(name, &pki.CSRTrust{NodeKey: nk, Endorsement: en})
	if err != nil {
		e.t.Fatal(err)
	}
	certPEM, err := e.ca.SignCSR(csrPEM, pki.IssueOptions{Role: pki.RoleWorker, Name: name, Validity: time.Hour})
	if err != nil {
		e.t.Fatal(err)
	}
	dir := filepath.Join(e.t.TempDir(), "tls")
	if err := pki.WriteBundle(dir, e.ca.CertPEM, certPEM, keyPEM); err != nil {
		e.t.Fatal(err)
	}
	b, err := pki.LoadBundle(dir)
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

func TestRaceRefusesUnendorsedWorker(t *testing.T) {
	e := newRaceEnv(t)
	plain := e.serve(0)
	w := Worker{Name: "w1", CertName: "w1", Transport: "tcp", Address: plain.Addr().String(), TrustName: "w1"}
	if _, _, err := e.race(w); err != nil {
		t.Fatalf("no policy: %v", err)
	}

	k := e.requireWorkers()
	_, legs, err := e.race(w)
	var te *TrustError
	if !errors.As(err, &te) || te.Worker != "w1" {
		t.Fatalf("want a TrustError, got %v (%s)", err, RaceSummary(legs))
	}
	if msg := err.Error(); !strings.Contains(msg, "longarmd trust request") || !strings.Contains(msg, "longarm trust endorse") {
		t.Fatalf("message without the fix: %s", msg)
	}
	if Retryable(status.Error(codes.Unavailable, err.Error())) {
		t.Fatal("a trust refusal must not be retried")
	}

	// An unendorsed impostor on a fast leg loses against the endorsed worker.
	e.worker = e.endorsedWorker("w1", k)
	good := e.serve(100 * time.Millisecond)
	w.Alt = []Endpoint{{Transport: "tcp", Address: good.Addr().String()}}
	won, legs, err := e.race(w)
	if err != nil || won.Address != good.Addr().String() {
		t.Fatalf("won %v, %v (%s)", won, err, RaceSummary(legs))
	}

	// Another user's endorsement doesn't count.
	_, other, _ := ed25519.GenerateKey(nil)
	e.worker = e.endorsedWorker("w1", other)
	foreign := e.serve(0)
	_, _, err = e.race(Worker{Name: "w1", CertName: "w1", Transport: "tcp", Address: foreign.Addr().String(), TrustName: "w1"})
	if !errors.As(err, &te) || !strings.Contains(err.Error(), "not trusted") {
		t.Fatalf("foreign endorsement: %v", err)
	}
}

func TestTrustRefusalThroughGRPC(t *testing.T) {
	te := &TrustError{Worker: "w1", Reason: `worker/w1 is endorsed by a key that is not trusted for user "bob"`}
	// What a gRPC call reports when the dialer failed.
	err := status.Error(codes.Unavailable, fmt.Sprintf("connection error: desc = %q", "transport: Error while dialing: "+te.Error()))
	got, ok := TrustRefusal(err)
	if !ok || got != te.Error() {
		t.Fatalf("TrustRefusal = %q, %v\nwant %q", got, ok, te.Error())
	}
	if _, ok := TrustRefusal(errors.New("all 1 transport legs failed: connection refused")); ok {
		t.Fatal("not a trust refusal")
	}
	if Retryable(err) {
		t.Fatal("retryable")
	}
}

func TestNewDialerBrokenTrustFailsClosed(t *testing.T) {
	e := newRaceEnv(t)
	dir := e.d.cfg.Dir
	if err := os.MkdirAll(filepath.Join(dir, TrustDir), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, TrustDir, trust.PolicyFile), []byte("require_workers = true\n[users]\nbob = \"nonsense\"\n"), 0o644)
	if _, err := NewDialer(e.d.cfg); err == nil || !strings.Contains(err.Error(), "end-to-end trust") {
		t.Fatalf("NewDialer with a broken trust.toml: %v", err)
	}
}
