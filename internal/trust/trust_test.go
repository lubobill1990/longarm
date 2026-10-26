package trust

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lubobill1990/longarm/internal/pki"
)

func TestStoreKeysAndEndorsement(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "trust")}
	k1, err := s.NodeKey()
	if err != nil {
		t.Fatal(err)
	}
	if k2, _ := s.NodeKey(); !k1.Equal(k2) {
		t.Fatal("node key not stable")
	}
	if e, err := s.Endorsement(); e != nil || err != nil {
		t.Fatalf("no endorsement yet: %v %v", e, err)
	}
	uk, err := s.CreateUserKey("alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUserKey("alice"); err == nil {
		t.Fatal("user key replaced")
	}
	if k, u, err := s.UserKey(); err != nil || u != "alice" || !k.Equal(uk) {
		t.Fatalf("user key: %s %v", u, err)
	}

	np := k1.Public().(ed25519.PublicKey)
	wrong, _ := pki.Endorse(uk, "alice", pki.KindDevice, "laptop", make(ed25519.PublicKey, 32))
	if err := s.SetEndorsement(wrong, pki.KindDevice, "laptop"); err == nil || !strings.Contains(err.Error(), "another node") {
		t.Fatalf("endorsement of another node: %v", err)
	}
	asWorker, _ := pki.Endorse(uk, "alice", pki.KindWorker, "laptop", np)
	if err := s.SetEndorsement(asWorker, pki.KindDevice, "laptop"); err == nil {
		t.Fatal("worker endorsement accepted for a device")
	}
	e, _ := pki.Endorse(uk, "alice", pki.KindDevice, "laptop", np)
	if err := s.SetEndorsement(e, pki.KindDevice, "laptop"); err != nil {
		t.Fatal(err)
	}
	if !s.RenewalRequested() {
		t.Fatal("no renewal requested after a new endorsement")
	}
	s.RenewalDone()
	if s.RenewalRequested() {
		t.Fatal("renewal flag stuck")
	}
	tr, err := s.CSRTrust()
	if err != nil || tr.Endorsement == nil || !tr.NodeKey.Equal(k1) {
		t.Fatalf("CSR trust: %+v %v", tr, err)
	}

	// export / import to another device
	b, err := s.ExportUserKey()
	if err != nil {
		t.Fatal(err)
	}
	o := Store{Dir: filepath.Join(t.TempDir(), "trust")}
	if u, err := o.ImportUserKey(b); err != nil || u != "alice" {
		t.Fatalf("import: %s %v", u, err)
	}
	if u, err := o.ImportUserKey(b); err != nil || u != "alice" {
		t.Fatalf("re-import of the same key: %s %v", u, err)
	}
	other := Store{Dir: filepath.Join(t.TempDir(), "trust")}
	other.CreateUserKey("alice")
	if _, err := other.ImportUserKey(b); err == nil {
		t.Fatal("a different user key replaced")
	}
	if _, err := o.ImportUserKey([]byte("garbage")); err == nil {
		t.Fatal("garbage imported")
	}
	if fi, err := os.Stat(filepath.Join(o.Dir, UserKeyFile)); err != nil || (fi.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/') {
		t.Fatalf("user key mode: %v %v", fi, err)
	}
}

func TestStorePolicy(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "trust")}
	if p, err := s.Policy(false); err != nil || p.Active() {
		t.Fatalf("empty worker policy: %+v %v", p, err)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	if err := s.AddUser(pki.UserKeyLine{User: "alice", Key: pub}); err != nil {
		t.Fatal(err)
	}
	p, err := s.Policy(false)
	if err != nil || !p.Active() || !p.Users["alice"].Equal(pub) {
		t.Fatalf("worker policy: %+v %v", p, err)
	}
	if err := s.DenyNode(base64.StdEncoding.EncodeToString(pub)); err != nil {
		t.Fatal(err)
	}
	if err := s.DenyNode("nope"); err == nil {
		t.Fatal("bad node key accepted")
	}
	if p, _ := s.Policy(false); len(p.DenyNodes) != 1 {
		t.Fatalf("deny: %+v", p)
	}
	if err := s.RemoveUser("bob"); err == nil {
		t.Fatal("removed an unknown user")
	}
	if err := s.RemoveUser("alice"); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Policy(false); p.Active() {
		t.Fatal("still active without users")
	}

	// a broken file is an error (longarmd refuses to start), not "off"
	os.WriteFile(filepath.Join(s.Dir, PolicyFile), []byte("[users]\nalice = \"AAAA\"\n"), 0o644)
	if _, err := s.Policy(false); err == nil {
		t.Fatal("a broken key was accepted")
	}
	os.WriteFile(filepath.Join(s.Dir, PolicyFile), []byte("[users]\n\"Not Valid\" = \""+base64.StdEncoding.EncodeToString(pub)+"\"\n"), 0o644)
	if _, err := s.Policy(false); err == nil {
		t.Fatal("an invalid user name was accepted")
	}
	// a user key line works as the value, if it names the same user
	os.WriteFile(filepath.Join(s.Dir, PolicyFile), []byte("[users]\nbob = \""+pki.UserKeyLine{User: "alice", Key: pub}.String()+"\"\n"), 0o644)
	if _, err := s.Policy(false); err == nil {
		t.Fatal("alice's key line accepted for bob")
	}
}

func TestClientPolicy(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "trust")}
	if p, err := s.Policy(true); p != nil || err != nil {
		t.Fatalf("client without require_workers: %+v %v", p, err)
	}
	if err := s.SetRequireWorkers(true); err == nil {
		t.Fatal("require_workers without any key accepted")
	}
	if p, _ := s.Policy(true); p != nil {
		t.Fatal("require_workers stayed on after the failed change")
	}
	uk, _ := s.CreateUserKey("alice")
	if err := s.SetRequireWorkers(true); err != nil {
		t.Fatal(err)
	}
	p, err := s.Policy(true)
	if err != nil || !p.Active() || !p.Users["alice"].Equal(uk.Public()) {
		t.Fatalf("own key: %+v %v", p, err)
	}
	// a configured key for the own user must be the own key
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	s.AddUser(pki.UserKeyLine{User: "alice", Key: pub})
	if _, err := s.Policy(true); err == nil {
		t.Fatal("conflicting key for the own user accepted")
	}
	if p, err := s.Policy(false); err != nil || !p.Users["alice"].Equal(pub) {
		t.Fatalf("worker view: %+v %v", p, err)
	}
	if Summary(nil) != "off" || !strings.Contains(Summary(p), "alice(") {
		t.Fatal(Summary(p))
	}
}
