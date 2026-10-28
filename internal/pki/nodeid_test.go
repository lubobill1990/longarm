package pki

import (
	"crypto/ed25519"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/nodeid"
)

// TestNodeIDInCertificate: the ID rides in a urn:longarm:node: URI SAN next
// to the longarm:// one (which older binaries read and which keeps the
// name); SameNode compares IDs when both have one.
func TestNodeIDInCertificate(t *testing.T) {
	f := newTrustFixture(t)
	id := nodeid.New()
	c, _ := f.issue(IssueOptions{Role: RoleWorker, Name: "box", ID: id}, nil)
	got, err := IdentityFromCert(c)
	if err != nil || got.ID != id || got.Name != "box" || got.Role != RoleWorker {
		t.Fatalf("identity %+v %v", got, err)
	}
	var urn, legacy bool
	for _, u := range c.URIs {
		switch {
		case u.String() == "urn:longarm:node:"+id:
			urn = true
		case u.Scheme == URIScheme && u.Host == "worker" && u.Path == "/box":
			legacy = true
		}
	}
	if !urn || !legacy {
		t.Fatalf("URI SANs %v", c.URIs)
	}
	old, _ := f.issue(IssueOptions{Role: RoleWorker, Name: "box"}, nil)
	if o, _ := IdentityFromCert(old); o.ID != "" {
		t.Fatalf("certificate without an id: %+v", o)
	}
	if _, _, err := f.ca.Issue(IssueOptions{Role: RoleWorker, Name: "box", ID: "not-a-uuid"}); err == nil {
		t.Fatal("bad id issued")
	}
	renamed := Identity{Role: RoleWorker, Name: "box2", ID: id}
	if !got.SameNode(renamed) || got.SameNode(Identity{Role: RoleWorker, Name: "box", ID: nodeid.New()}) {
		t.Fatal("SameNode by id")
	}
	if !(Identity{Role: RoleWorker, Name: "box"}).SameNode(got) || (Identity{Role: RoleWorker, Name: "other"}).SameNode(got) {
		t.Fatal("SameNode without an id on one side")
	}
}

// TestEndorsementBindsID: an endorsement with an ID signs a v2 message
// (its own domain, covering the ID) and nothing else: stripping the ID
// leaves no valid endorsement (no downgrade to a name-bound one), the ID
// can't be changed; it survives a rename and can't be moved to another ID.
func TestEndorsementBindsID(t *testing.T) {
	f := newTrustFixture(t)
	p := f.policy()
	id := nodeid.New()
	nk := newNodeKey()
	e, err := EndorseID(f.alice, "alice", KindWorker, "box", id, nk.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	// round trip through the text form
	e2, err := ParseEndorsement(e.String())
	if err != nil || e2.ID != id {
		t.Fatalf("parse: %+v %v", e2, err)
	}
	// downgrade by stripping the ID: no name-only signature is left
	stripped := *e
	stripped.ID = ""
	if stripped.Verify() == nil {
		t.Fatal("an id endorsement with its id removed verifies as a name-bound one")
	}
	// ... also through the text form (fields removed from the JSON)
	var m map[string]any
	b, _ := json.Marshal(e)
	json.Unmarshal(b, &m)
	delete(m, "id")
	b, _ = json.Marshal(m)
	if _, err := ParseEndorsement(endorsementPrefix + base64.RawURLEncoding.EncodeToString(b)); err == nil {
		t.Fatal("a stripped endorsement parses")
	}
	// the hub can't change the ID
	moved := *e
	moved.ID = nodeid.New()
	if moved.Verify() == nil {
		t.Fatal("endorsement moved to another id")
	}
	// nor make a v1 endorsement into a v2 one
	v1, _ := Endorse(f.alice, "alice", KindWorker, "box", nk.Public().(ed25519.PublicKey))
	up := *v1
	up.ID = id
	if up.Verify() == nil {
		t.Fatal("a name-bound endorsement with an id added verifies")
	}
	tr := &CSRTrust{NodeKey: nk, Endorsement: e}
	// the stripped endorsement in a certificate: refused by name-bound rules too
	strippedCert, _ := f.issue(IssueOptions{Role: RoleWorker, Name: "box", ID: id}, &CSRTrust{NodeKey: nk, Endorsement: &stripped})
	if _, err := p.Check(strippedCert); err == nil {
		t.Fatal("a stripped endorsement passed the check")
	}

	good, _ := f.issue(IssueOptions{Role: RoleWorker, Name: "box", ID: id}, tr)
	if _, err := p.Check(good); err != nil {
		t.Fatalf("endorsed by id: %v", err)
	}
	// renamed: the same node, a new name in its certificate - still endorsed
	renamed, _ := f.issue(IssueOptions{Role: RoleWorker, Name: "box-renamed", ID: id}, tr)
	if _, err := p.Check(renamed); err != nil {
		t.Fatalf("renamed node: %v", err)
	}
	// another ID (a CA that wants the node to pass for another one): refused
	other, _ := f.issue(IssueOptions{Role: RoleWorker, Name: "box", ID: nodeid.New()}, tr)
	if _, err := p.Check(other); err == nil || !strings.Contains(err.Error(), "endorsed as node") {
		t.Fatalf("other id: %v", err)
	}
	// no ID in the certificate: refused (the endorsement names one)
	noID, _ := f.issue(IssueOptions{Role: RoleWorker, Name: "box"}, tr)
	if _, err := p.Check(noID); err == nil {
		t.Fatal("certificate without an id passed an id endorsement")
	}
}

// TestLegacyEndorsementStaysNameBound: an endorsement made before node IDs
// keeps working for the node it was made for (by name) once its
// certificate carries an ID, stops at a rename, and never lets a new node
// that got the old name (after a revocation) pass: it has another node key.
func TestLegacyEndorsementStaysNameBound(t *testing.T) {
	f := newTrustFixture(t)
	p := f.policy()
	nk := newNodeKey()
	tr := f.endorsed("alice", KindWorker, "box", nk) // no id: before node IDs
	oldID := nodeid.New()
	migrated, _ := f.issue(IssueOptions{Role: RoleWorker, Name: "box", ID: oldID}, tr)
	if _, err := p.Check(migrated); err != nil {
		t.Fatalf("legacy endorsement on an id certificate: %v", err)
	}
	renamed, _ := f.issue(IssueOptions{Role: RoleWorker, Name: "box2", ID: oldID}, tr)
	if _, err := p.Check(renamed); err == nil {
		t.Fatal("a name-bound endorsement followed a rename")
	}

	// The node is revoked; a new node gets the name "box" (and a new ID).
	// It can't present the old endorsement: the extension binds the old
	// node key to the certificate key ...
	newKey := newNodeKey()
	if _, err := TrustExtension(newKey, newKey.Public(), tr.Endorsement); err == nil || !strings.Contains(err.Error(), "another node key") {
		t.Fatal("trust extension for another node key")
	}
	// ... and a CA copying the old node's extension into the new node's
	// certificate fails the bind (the new TLS key isn't the one signed).
	ext := findTrustExt(migrated.Extensions)
	if ext == nil {
		t.Fatal("no trust extension")
	}
	imp := f.forge(IssueOptions{Role: RoleWorker, Name: "box", ID: nodeid.New()}, []pkix.Extension{*ext})
	if _, err := p.Check(imp); err == nil {
		t.Fatal("impersonation: the old node's endorsement passed for a new node of the same name")
	}
	// The new node's own endorsement request (with its ID) is for its key only.
	e, err := EndorseID(f.alice, "alice", KindWorker, "box", nodeid.New(), newKey.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(e); !strings.Contains(string(b), `"id"`) || strings.Contains(string(b), `"sig2"`) {
		t.Fatalf("endorsement json: %s", b)
	}
}

// TestLegacyEndorsementWindow: a name-bound endorsement on a certificate
// with a node ID counts only if made before LegacyEndorsementCutoff; on a
// certificate without an ID it stays name-bound.
func TestLegacyEndorsementWindow(t *testing.T) {
	f := newTrustFixture(t)
	p := f.policy()
	nk := newNodeKey()
	at := func(issued time.Time) *CSRTrust {
		e, err := Endorse(f.alice, "alice", KindWorker, "box", nk.Public().(ed25519.PublicKey))
		if err != nil {
			t.Fatal(err)
		}
		e.Issued = issued.Unix()
		e.Sig = ed25519.Sign(f.alice, e.signed())
		return &CSRTrust{NodeKey: nk, Endorsement: e}
	}
	before, after := at(LegacyEndorsementCutoff.Add(-time.Hour)), at(LegacyEndorsementCutoff.Add(time.Hour))
	withID := IssueOptions{Role: RoleWorker, Name: "box", ID: nodeid.New()}
	if c, _ := f.issue(withID, before); func() error { _, err := p.Check(c); return err }() != nil {
		t.Fatal("a name-bound endorsement from before the cutoff on an id certificate")
	}
	if c, _ := f.issue(withID, after); func() error { _, err := p.Check(c); return err }() == nil {
		t.Fatal("a name-bound endorsement from after the cutoff counted on an id certificate")
	}
	if c, _ := f.issue(IssueOptions{Role: RoleWorker, Name: "box"}, after); func() error { _, err := p.Check(c); return err }() != nil {
		t.Fatal("a certificate without an id: the name-bound endorsement")
	}
}

// TestCheckNodeID: a certificate answering for an expected node ID must
// carry it; one without an ID only if its serial is one the hub reports
// for the node.
func TestCheckNodeID(t *testing.T) {
	f := newTrustFixture(t)
	id := nodeid.New()
	withID, _ := f.issue(IssueOptions{Role: RoleWorker, Name: "box", ID: id}, nil)
	other, _ := f.issue(IssueOptions{Role: RoleWorker, Name: "box", ID: nodeid.New()}, nil)
	legacy, _ := f.issue(IssueOptions{Role: RoleWorker, Name: "box"}, nil)
	check := func(c *x509.Certificate, t2 DialTarget) error {
		ident, _ := IdentityFromCert(c)
		return CheckNodeID(c, ident, t2)
	}
	if check(withID, DialTarget{ID: id}) != nil || check(other, DialTarget{ID: id}) == nil {
		t.Fatal("by id")
	}
	if check(legacy, DialTarget{ID: id}) == nil {
		t.Fatal("a certificate without an id accepted for an expected id")
	}
	if check(legacy, DialTarget{ID: id, LegacySerials: []string{Serial(legacy)}}) != nil {
		t.Fatal("the reported legacy serial refused")
	}
	if check(legacy, DialTarget{}) != nil {
		t.Fatal("no expected id: nothing to check")
	}
}

// forge issues a certificate with arbitrary extensions for a fresh key, as
// whoever holds the CA key could.
func (f *trustFixture) forge(o IssueOptions, exts []pkix.Extension) *x509.Certificate {
	f.t.Helper()
	key, err := newKey()
	if err != nil {
		f.t.Fatal(err)
	}
	o.extra = exts
	certPEM, err := f.ca.sign(&key.PublicKey, o)
	if err != nil {
		f.t.Fatal(err)
	}
	c, err := ParseCertPEM(certPEM)
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

// TestCheckTarget: with end-to-end trust, a worker targeted by name must be
// endorsed under that name, one targeted by ID prefix with an ID that has
// it (the hub's mapping of names to nodes is not taken on trust).
func TestCheckTarget(t *testing.T) {
	id := "3f2a9c1e-4b5d-4c3e-8f00-1234567890ab"
	withID := &Endorsement{Name: "box", ID: id}
	legacy := &Endorsement{Name: "box"}
	for _, c := range []struct {
		e  *Endorsement
		t  DialTarget
		ok bool
	}{
		{withID, DialTarget{}, false}, // no target: only explicitly unconstrained
		{withID, DialTarget{Any: true}, true},
		{withID, DialTarget{ID: id}, true},
		{withID, DialTarget{ID: "77aa0000-0000-4000-8000-000000000001", Any: true}, false},
		{legacy, DialTarget{ID: id}, true}, // name-bound: the certificate's ID is CheckNodeID's
		{withID, DialTarget{Name: "box"}, true},
		{withID, DialTarget{Name: "other"}, false},
		{withID, DialTarget{IDPrefix: "3f2a9c1e"}, true},
		{withID, DialTarget{IDPrefix: "3f2a9c1f"}, false},
		{legacy, DialTarget{Name: "box"}, true},
		{legacy, DialTarget{IDPrefix: "3f2a9c1e"}, false},
	} {
		if err := CheckTarget(c.e, c.t); (err == nil) != c.ok {
			t.Errorf("%+v %+v: %v", c.e, c.t, err)
		}
	}
}
