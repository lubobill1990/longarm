package main

import (
	"bytes"
	"crypto/ed25519"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/trust"
)

func TestTrustDeviceFlows(t *testing.T) {
	root := t.TempDir()
	store := func(n string) trust.Store { return trust.Store{Dir: filepath.Join(root, n)} }
	pc := pki.Identity{Role: pki.RoleClient, Name: "alice-pc", User: "alice"}
	laptop := pki.Identity{Role: pki.RoleClient, Name: "alice-laptop", User: "alice"}

	// init: a user key for the certificate's user, this device endorsed.
	a := store("a")
	line, err := trustInit(a, pc)
	if err != nil {
		t.Fatal(err)
	}
	if line.User != "alice" {
		t.Fatalf("user key line for %q", line.User)
	}
	if l, err := pki.ParseUserKeyLine(line.String()); err != nil || !bytes.Equal(l.Key, line.Key) {
		t.Fatalf("user key line does not parse: %v", err)
	}
	e, err := a.Endorsement()
	if err != nil || e == nil || e.Kind != pki.KindDevice || e.Name != "alice-pc" || e.User != "alice" {
		t.Fatalf("endorsement after init: %+v, %v", e, err)
	}
	if !a.RenewalRequested() {
		t.Fatal("init must ask for a renewal")
	}
	if _, err := trustInit(a, pc); err == nil {
		t.Fatal("a second init must refuse")
	}
	if _, err := trustInit(store("x"), pki.Identity{Role: pki.RoleClient, Name: "Big.Name"}); err == nil {
		t.Fatal("init for an invalid user")
	}

	// A second device: request, endorse on the first, accept.
	b := store("b")
	req, err := b.Request(pki.KindDevice, laptop.Name, laptop.Owner())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := trustEndorse(b, req.String()); err == nil || !strings.Contains(err.Error(), "no user key") {
		t.Fatalf("endorse without a user key: %v", err)
	}
	en, err := trustEndorse(a, req.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := trustAccept(b, en.String(), pc); err == nil {
		t.Fatal("accepting an endorsement for another device name")
	}
	if _, err := trustAccept(b, en.String(), laptop); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.Endorsement(); got == nil || got.Name != "alice-laptop" || !b.RenewalRequested() {
		t.Fatalf("endorsement on the second device: %+v", got)
	}

	// Another user's device request is refused; a worker request is fine.
	bobReq, _ := store("bob").Request(pki.KindDevice, "bob-pc", "bob")
	if _, err := trustEndorse(a, bobReq.String()); err == nil {
		t.Fatal("endorsed a device of another user")
	}
	wReq, _ := store("w1").Request(pki.KindWorker, "w1", "")
	we, err := trustEndorse(a, wReq.String())
	if err != nil || we.Kind != pki.KindWorker || we.Name != "w1" || we.User != "alice" {
		t.Fatalf("worker endorsement: %+v, %v", we, err)
	}
	if err := store("w1").SetEndorsement(we, pki.KindWorker, "w1"); err != nil {
		t.Fatal(err)
	}

	// An endorsement by a user other than the device's is refused.
	bobDev := pki.Identity{Role: pki.RoleClient, Name: "bob-pc", User: "bob"}
	be, _ := pki.Endorse(mustUserKey(t, a), "alice", pki.KindDevice, "bob-pc", mustNodeKey(t, store("bob")))
	if _, err := trustAccept(store("bob"), be.String(), bobDev); err == nil {
		t.Fatal("accepted alice's endorsement on bob's device")
	}

	// import-key: endorses an unendorsed device of the key's user.
	pem, err := a.ExportUserKey()
	if err != nil {
		t.Fatal(err)
	}
	c := store("c")
	user, endorsed, err := trustImportKey(c, pem, pki.Identity{Role: pki.RoleClient, Name: "alice-3", User: "alice"})
	if err != nil || user != "alice" || !endorsed {
		t.Fatalf("import: %q %v %v", user, endorsed, err)
	}
	if got, _ := c.Endorsement(); got == nil || got.Name != "alice-3" {
		t.Fatalf("endorsement after import: %+v", got)
	}
	// ... but not a device of another user, nor an endorsed one.
	if _, endorsed, err := trustImportKey(store("d"), pem, bobDev); err != nil || endorsed {
		t.Fatalf("import on bob's device: %v %v", endorsed, err)
	}
	if _, endorsed, err := trustImportKey(b, pem, laptop); err != nil || endorsed {
		t.Fatalf("import on an endorsed device: %v %v", endorsed, err)
	}

	// show (no certificate in this config dir).
	configDir = filepath.Join(root, "cfg")
	var out bytes.Buffer
	if err := trustShow(&out, a); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"user key:        alice", line.String(), "endorsement:     device alice-pc, by alice", "require_workers: off", "renewal pending"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("show lacks %q:\n%s", want, out.String())
		}
	}
}

func mustUserKey(t *testing.T, s trust.Store) ed25519.PrivateKey {
	k, _, err := s.UserKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func mustNodeKey(t *testing.T, s trust.Store) ed25519.PublicKey {
	k, err := s.NodeKey()
	if err != nil {
		t.Fatal(err)
	}
	return k.Public().(ed25519.PublicKey)
}
