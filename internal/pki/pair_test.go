package pki

import (
	"crypto/ed25519"
	"strings"
	"testing"
	"time"
)

func TestPairSecret(t *testing.T) {
	s := NewPairSecret()
	if len(s) != 23 || strings.Count(s, "-") != 3 {
		t.Fatalf("secret %q", s)
	}
	if NewPairSecret() == s {
		t.Fatal("two equal secrets")
	}
	n, err := ParsePairSecret(strings.ToLower(strings.ReplaceAll(s, "-", " ")))
	if err != nil || n != strings.ReplaceAll(s, "-", "") {
		t.Fatalf("parse %q: %q %v", s, n, err)
	}
	if n, err := ParsePairSecret("IL0O0-00000-00000-00000"); err != nil || n != "11000000000000000000" {
		t.Fatalf("Crockford aliases: %q %v", n, err)
	}
	for _, bad := range []string{"", "ABCDE", "ABCDE-ABCDE-ABCDE-ABCDU", "ABCDE-ABCDE-ABCDE-ABCDE-A", "ÄBCDE-ABCDE-ABCDE-ABCDE"} {
		if _, err := ParsePairSecret(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestPairingMACs(t *testing.T) {
	s := NewPairSecret()
	p, err := DerivePairing(s)
	if err != nil {
		t.Fatal(err)
	}
	q, err := DerivePairing(strings.ToLower(s))
	if err != nil || q.ID != p.ID || !ValidPairID(p.ID) {
		t.Fatalf("id %q vs %q (%v)", p.ID, q.ID, err)
	}
	userKey, _, _ := ed25519.GenerateKey(nil)
	nodeKey, _, _ := ed25519.GenerateKey(nil)
	exp := time.Unix(1900000000, 0)

	tag := p.UserTag(KindWorker, "alice", userKey, exp)
	if err := q.CheckUserTag(tag, KindWorker, "alice", userKey, exp); err != nil {
		t.Fatal(err)
	}
	other, _, _ := ed25519.GenerateKey(nil)
	for name, err := range map[string]error{
		"swapped user key": q.CheckUserTag(tag, KindWorker, "alice", other, exp),
		"another user":     q.CheckUserTag(tag, KindWorker, "mallory", userKey, exp),
		"another kind":     q.CheckUserTag(tag, KindDevice, "alice", userKey, exp),
		"later expiry":     q.CheckUserTag(tag, KindWorker, "alice", userKey, exp.Add(time.Hour)),
	} {
		if err != ErrPairMismatch {
			t.Errorf("%s: %v", name, err)
		}
	}

	proof := q.NodeProof(KindWorker, nodeKey)
	if err := p.CheckNodeProof(proof, KindWorker, nodeKey); err != nil {
		t.Fatal(err)
	}
	if p.CheckNodeProof(proof, KindWorker, other) != ErrPairMismatch || p.CheckNodeProof(proof, KindDevice, nodeKey) != ErrPairMismatch {
		t.Fatal("node proof accepted for another key or kind")
	}
	r, _ := DerivePairing(NewPairSecret())
	if r.ID == p.ID || r.CheckNodeProof(proof, KindWorker, nodeKey) != ErrPairMismatch || r.CheckUserTag(tag, KindWorker, "alice", userKey, exp) != ErrPairMismatch {
		t.Fatal("another secret verifies the MACs")
	}
}
