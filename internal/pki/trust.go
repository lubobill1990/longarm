package pki

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/lubobill1990/longarm/internal/nodeid"
)

// End-to-end trust (docs/security.md "End-to-end trust"): the hub's CA
// says who a certificate belongs to, but whoever holds the CA key (the hub,
// its operators, a thief of its database) can mint a certificate for any
// name. With end-to-end trust a worker also demands proof that the
// certificate's node was endorsed by a user key its owner pinned locally,
// and a client demands the same of the workers it talks to. User keys never
// leave the users' devices; the hub only carries the proof.
//
// The proof is a certificate extension (OIDTrust), requested in the CSR by
// the node and copied verbatim by the hub:
//
//   - every node has a long-lived Ed25519 node key next to its TLS bundle;
//     certificate renewals (new TLS keys) keep it;
//   - bind: the node key's signature over the certificate's public key, so
//     the extension can't be moved to a certificate for another key;
//   - endorsement: a user key's signature over the node key, the node's
//     kind (worker or device), its name and the user - and, since node IDs
//     (docs/security.md "Node identity"), a second signature (sig2) that
//     also covers the node's immutable ID. Peers that know IDs hold an
//     endorsement carrying an ID to the certificate's ID (the name is only
//     what the user called the node then: renaming keeps it valid); an
//     endorsement without an ID (made before IDs) still binds the name, so
//     it is valid only while the certificate has that name. Either way the
//     endorsed thing is the node key: a new node that gets an old node's
//     name (after a revocation) has another node key and none of the old
//     node's trust.
//
// A hub that strips or alters the extension only makes the node unusable
// for peers that require it (fail closed); it can't make one up.

// OIDTrust is the extension carrying a TrustExt. The arc under 2.25 is the
// top 31 bits of a random UUID: Go's x509 parser refuses longer arcs. (A
// registered enterprise number would be the clean home; certificates are
// short-lived, so a move only needs both OIDs accepted for one TTL.)
var OIDTrust = asn1.ObjectIdentifier{2, 25, 1972799524, 1}

// Endorsement kinds: what the endorsed node is.
const (
	KindWorker = "worker" // a longarmd: clients may run work there
	KindDevice = "device" // a client or admin certificate of the user
)

// maxTrustExt bounds the extension (a hub copies it from a CSR).
const maxTrustExt = 4096

// Endorsement is a user key's statement "the node with NodeKey, called
// Name (with ID), is my <Kind>".
type Endorsement struct {
	Kind    string `json:"kind"`
	User    string `json:"user"`
	Name    string `json:"name"`
	NodeKey []byte `json:"node_key"`
	UserKey []byte `json:"user_key"`
	Issued  int64  `json:"issued"`
	Sig     []byte `json:"sig"` // over message(): no ID (what peers before node IDs check)
	// ID is the endorsed node's ID; Sig2 signs message2(), which covers
	// it. Both or neither.
	ID   string `json:"id,omitempty"`
	Sig2 []byte `json:"sig2,omitempty"`
}

func (e *Endorsement) message() []byte {
	b64 := base64.StdEncoding.EncodeToString
	return fmt.Appendf(nil, "longarm-endorsement-v1\n%s\n%s\n%s\n%s\n%s\n%d\n",
		e.Kind, e.User, e.Name, b64(e.NodeKey), b64(e.UserKey), e.Issued)
}

func (e *Endorsement) message2() []byte {
	b64 := base64.StdEncoding.EncodeToString
	return fmt.Appendf(nil, "longarm-endorsement-v2\n%s\n%s\n%s\n%s\n%s\n%s\n%d\n",
		e.Kind, e.User, e.ID, e.Name, b64(e.NodeKey), b64(e.UserKey), e.Issued)
}

// Endorse signs nodeKey as user's node of kind, called name (no ID: an
// endorsement bound to the name; EndorseID binds the ID).
func Endorse(userKey ed25519.PrivateKey, user, kind, name string, nodeKey ed25519.PublicKey) (*Endorsement, error) {
	return EndorseID(userKey, user, kind, name, "", nodeKey)
}

// EndorseID signs nodeKey as user's node of kind with ID id (called name
// now). With id "" it is Endorse.
func EndorseID(userKey ed25519.PrivateKey, user, kind, name, id string, nodeKey ed25519.PublicKey) (*Endorsement, error) {
	if id != "" && !nodeid.Valid(id) {
		return nil, fmt.Errorf("endorsement: invalid node id %q", id)
	}
	if kind != KindWorker && kind != KindDevice {
		return nil, fmt.Errorf("endorsement kind %q: worker or device", kind)
	}
	if !ValidUser(user) {
		return nil, fmt.Errorf("endorsement: invalid user %q", user)
	}
	if !ValidName(name) {
		return nil, fmt.Errorf("endorsement: invalid name %q", name)
	}
	if len(nodeKey) != ed25519.PublicKeySize {
		return nil, errors.New("endorsement: invalid node key")
	}
	e := &Endorsement{Kind: kind, User: user, Name: name, NodeKey: bytes.Clone(nodeKey),
		UserKey: userKey.Public().(ed25519.PublicKey), Issued: time.Now().Unix(), ID: strings.ToLower(id)}
	e.Sig = ed25519.Sign(userKey, e.message())
	if e.ID != "" {
		e.Sig2 = ed25519.Sign(userKey, e.message2())
	}
	return e, nil
}

// Verify checks the signatures by the key the endorsement names (whether
// that key is trusted is the caller's question): sig, and sig2 when the
// endorsement carries an ID.
func (e *Endorsement) Verify() error {
	if len(e.UserKey) != ed25519.PublicKeySize || len(e.NodeKey) != ed25519.PublicKeySize {
		return errors.New("endorsement: bad key size")
	}
	if !ed25519.Verify(e.UserKey, e.message(), e.Sig) {
		return errors.New("endorsement: bad signature")
	}
	if e.ID != "" || len(e.Sig2) > 0 {
		if !nodeid.Valid(e.ID) || e.ID != strings.ToLower(e.ID) {
			return errors.New("endorsement: bad node id")
		}
		if !ed25519.Verify(e.UserKey, e.message2(), e.Sig2) {
			return errors.New("endorsement: bad signature over the node id")
		}
	}
	return nil
}

// TrustExt is the extension's content.
type TrustExt struct {
	V           int          `json:"v"`
	NodeKey     []byte       `json:"node_key"`
	Bind        []byte       `json:"bind"`
	Endorsement *Endorsement `json:"endorsement,omitempty"`
}

func bindMessage(spkiDER []byte) []byte {
	h := sha256.Sum256(spkiDER)
	return append([]byte("longarm-bind-v1\n"), h[:]...)
}

// TrustExtension builds the extension for a certificate (CSR) of pub:
// nodeKey binds itself to pub; e (optional) is the node key's endorsement.
func TrustExtension(nodeKey ed25519.PrivateKey, pub crypto.PublicKey, e *Endorsement) (pkix.Extension, error) {
	spki, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return pkix.Extension{}, err
	}
	np := nodeKey.Public().(ed25519.PublicKey)
	if e != nil && !bytes.Equal(e.NodeKey, np) {
		return pkix.Extension{}, errors.New("the endorsement is for another node key")
	}
	b, err := json.Marshal(TrustExt{V: 1, NodeKey: np, Bind: ed25519.Sign(nodeKey, bindMessage(spki)), Endorsement: e})
	if err != nil {
		return pkix.Extension{}, err
	}
	val, err := asn1.Marshal(b) // an OCTET STRING holding the JSON
	if err != nil {
		return pkix.Extension{}, err
	}
	if len(val) > maxTrustExt {
		return pkix.Extension{}, errors.New("trust extension too large")
	}
	return pkix.Extension{Id: OIDTrust, Value: val}, nil
}

// findTrustExt returns the trust extension among exts (nil: none).
func findTrustExt(exts []pkix.Extension) *pkix.Extension {
	for i := range exts {
		if exts[i].Id.Equal(OIDTrust) {
			return &exts[i]
		}
	}
	return nil
}

// parseTrustExt decodes the extension and checks that it binds pub.
func parseTrustExt(ext pkix.Extension, pub crypto.PublicKey) (*TrustExt, error) {
	if len(ext.Value) > maxTrustExt {
		return nil, errors.New("trust extension too large")
	}
	var b []byte
	if rest, err := asn1.Unmarshal(ext.Value, &b); err != nil || len(rest) > 0 {
		return nil, errors.New("trust extension: not an OCTET STRING")
	}
	var t TrustExt
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("trust extension: %w", err)
	}
	if t.V != 1 || len(t.NodeKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("trust extension: unsupported version %d or bad node key", t.V)
	}
	spki, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	if !ed25519.Verify(t.NodeKey, bindMessage(spki), t.Bind) {
		return nil, errors.New("trust extension: the node key did not sign this certificate's key")
	}
	if t.Endorsement != nil && !bytes.Equal(t.Endorsement.NodeKey, t.NodeKey) {
		return nil, errors.New("trust extension: the endorsement is for another node key")
	}
	return &t, nil
}

// TrustOf returns the certificate's verified trust extension (nil, nil:
// the certificate has none).
func TrustOf(c *x509.Certificate) (*TrustExt, error) {
	ext := findTrustExt(c.Extensions)
	if ext == nil {
		return nil, nil
	}
	return parseTrustExt(*ext, c.PublicKey)
}

// csrTrustExt returns the CSR's trust extension after checking that it
// binds the CSR's key (nil: none). The hub can't judge the endorsement
// (it doesn't know the user keys) and doesn't need to: peers do.
func csrTrustExt(csr *x509.CertificateRequest) (*pkix.Extension, error) {
	ext := findTrustExt(csr.Extensions)
	if ext == nil {
		return nil, nil
	}
	if _, err := parseTrustExt(*ext, csr.PublicKey); err != nil {
		return nil, err
	}
	return ext, nil
}

// TrustOfCSR returns the CSR's verified trust extension (nil, nil: none),
// e.g. to show its node key before approving an enrollment.
func TrustOfCSR(csr *x509.CertificateRequest) (*TrustExt, error) {
	ext := findTrustExt(csr.Extensions)
	if ext == nil {
		return nil, nil
	}
	return parseTrustExt(*ext, csr.PublicKey)
}

// TrustPolicy is the set of user keys a node accepts endorsements from.
type TrustPolicy struct {
	// Users maps a Longarm user to the public key whose endorsements count
	// for that user.
	Users map[string]ed25519.PublicKey
	// DenyNodes are node keys refused even when endorsed (a lost device).
	DenyNodes [][]byte
	// DenyAll: endorsements are required but no key is trusted (a client
	// with require_workers after its last trusted user was removed):
	// everybody is refused - never "off".
	DenyAll bool
}

// Active reports whether the policy demands endorsements.
func (p *TrustPolicy) Active() bool { return p != nil && (len(p.Users) > 0 || p.DenyAll) }

// Check verifies that c's node is endorsed by a trusted user key as what c
// claims to be: a worker endorsed as a worker, a client or admin endorsed
// as a device of the certificate's user - under its ID when the
// endorsement carries one (the certificate must carry the same ID), else
// under its name. It returns the endorsing user.
func (p *TrustPolicy) Check(c *x509.Certificate) (string, error) {
	e, err := p.CheckEndorsement(c)
	if err != nil {
		return "", err
	}
	return e.User, nil
}

// CheckEndorsement is Check returning the verified endorsement.
func (p *TrustPolicy) CheckEndorsement(c *x509.Certificate) (*Endorsement, error) {
	id, err := IdentityFromCert(c)
	if err != nil {
		return nil, err
	}
	if len(p.Users) == 0 {
		return nil, fmt.Errorf("%s: no user key is trusted (require_workers without trusted users)", id)
	}
	t, err := TrustOf(c)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", id, err)
	}
	if t == nil {
		return nil, fmt.Errorf("%s has no end-to-end trust proof (not endorsed, or its hub dropped the extension)", id)
	}
	e := t.Endorsement
	if e == nil {
		return nil, fmt.Errorf("%s is not endorsed by a user key (longarm trust endorse)", id)
	}
	for _, d := range p.DenyNodes {
		if bytes.Equal(d, t.NodeKey) {
			return nil, fmt.Errorf("%s: node key is denied", id)
		}
	}
	key, ok := p.Users[e.User]
	if !ok || !bytes.Equal(key, e.UserKey) {
		return nil, fmt.Errorf("%s is endorsed by a key that is not trusted for user %q", id, e.User)
	}
	if err := e.Verify(); err != nil {
		return nil, fmt.Errorf("%s: %w", id, err)
	}
	switch {
	case e.ID != "" && id.ID == "":
		return nil, fmt.Errorf("%s: endorsed as node %s, but the certificate carries no node id", id, e.ID)
	case e.ID != "" && e.ID != id.ID:
		return nil, fmt.Errorf("%s: endorsed as node %s, the certificate is node %s", id, e.ID, id.ID)
	case e.ID == "" && e.Name != id.Name:
		return nil, fmt.Errorf("%s: endorsed as %q", id, e.Name)
	}
	switch id.Role {
	case RoleWorker:
		if e.Kind != KindWorker {
			return nil, fmt.Errorf("%s: endorsed as a %s, not a worker", id, e.Kind)
		}
	case RoleClient, RoleAdmin:
		if e.Kind != KindDevice {
			return nil, fmt.Errorf("%s: endorsed as a %s, not a device", id, e.Kind)
		}
		if e.User != id.Owner() {
			return nil, fmt.Errorf("%s: a device of %s, endorsed by %s", id, id.Owner(), e.User)
		}
	default:
		return nil, fmt.Errorf("%s: role %s can't be endorsed", id, id.Role)
	}
	return e, nil
}

// CheckTarget applies what the user asked for to a worker's verified
// endorsement e: a worker targeted by name must have been endorsed under
// that name (the hub maps names to nodes, and end-to-end trust doesn't
// take the hub's word: a renamed worker needs a new endorsement, or a
// target by ID); one targeted by ID or ID prefix must have been endorsed
// with an ID that has it. An empty target (a worker the hub placed the
// job on) adds nothing.
func CheckTarget(e *Endorsement, t DialTarget) error {
	if t.Name != "" && e.Name != t.Name {
		return fmt.Errorf("asked for worker %q, the worker answering was endorsed as %q (renamed? target it by id, or endorse it again)", t.Name, e.Name)
	}
	if t.IDPrefix != "" {
		if e.ID == "" {
			return fmt.Errorf("asked for worker id %s, the worker answering has an endorsement without an id (made before node ids): target it by its endorsed name %q, or endorse it again", t.IDPrefix, e.Name)
		}
		if !nodeid.HasPrefix(e.ID, t.IDPrefix) {
			return fmt.Errorf("asked for worker id %s, the worker answering was endorsed as node %s", t.IDPrefix, e.ID)
		}
	}
	return nil
}

// ---------------------------------------------------------------- text forms

// Keys, endorsement requests and endorsements travel by copy and paste
// (chat, terminal): one line, a prefix naming the kind, base64url JSON.

const (
	userKeyPrefix     = "longarm-user-key.v1."
	requestPrefix     = "longarm-endorse-request.v1."
	endorsementPrefix = "longarm-endorsement.v1."
)

// UserKeyLine is a user's public key in text form.
type UserKeyLine struct {
	User string `json:"user"`
	Key  []byte `json:"key"`
}

// EndorseRequest is what a node asks a user to endorse.
type EndorseRequest struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	User    string `json:"user,omitempty"` // devices: the certificate's user
	NodeKey []byte `json:"node_key"`
	ID      string `json:"id,omitempty"` // the node's ID (its certificate's); "" = not known yet
}

func encodeLine(prefix string, v any) string {
	b, _ := json.Marshal(v)
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

func decodeLine(prefix, s string, v any) error {
	rest, ok := strings.CutPrefix(strings.TrimSpace(s), prefix)
	if !ok {
		return fmt.Errorf("not a %s line", prefix[:len(prefix)-1])
	}
	b, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// String is the text form of a user key.
func (k UserKeyLine) String() string { return encodeLine(userKeyPrefix, k) }

// ParseUserKeyLine parses a user key line.
func ParseUserKeyLine(s string) (UserKeyLine, error) {
	var k UserKeyLine
	if err := decodeLine(userKeyPrefix, s, &k); err != nil {
		return k, err
	}
	if !ValidUser(k.User) || len(k.Key) != ed25519.PublicKeySize {
		return k, errors.New("user key: invalid user or key")
	}
	return k, nil
}

// String is the text form of a request.
func (r EndorseRequest) String() string { return encodeLine(requestPrefix, r) }

// ParseEndorseRequest parses a request line.
func ParseEndorseRequest(s string) (EndorseRequest, error) {
	var r EndorseRequest
	if err := decodeLine(requestPrefix, s, &r); err != nil {
		return r, err
	}
	if (r.Kind != KindWorker && r.Kind != KindDevice) || !ValidName(r.Name) || len(r.NodeKey) != ed25519.PublicKeySize {
		return r, errors.New("endorsement request: invalid kind, name or node key")
	}
	if r.ID != "" && !nodeid.Valid(r.ID) {
		return r, errors.New("endorsement request: invalid node id")
	}
	return r, nil
}

// String is the text form of an endorsement.
func (e *Endorsement) String() string { return encodeLine(endorsementPrefix, e) }

// ParseEndorsement parses an endorsement line and checks its signature.
func ParseEndorsement(s string) (*Endorsement, error) {
	var e Endorsement
	if err := decodeLine(endorsementPrefix, s, &e); err != nil {
		return nil, err
	}
	if err := e.Verify(); err != nil {
		return nil, err
	}
	return &e, nil
}

// KeyID is a short, stable label of a key for logs and listings.
func KeyID(k []byte) string {
	h := sha256.Sum256(k)
	return base64.RawURLEncoding.EncodeToString(h[:9])
}

// TrustSource holds the current trust policy; bundles and TLS configs share
// it, so a changed policy (longarmd trust add-user) applies to the next
// handshake. A nil source has no policy.
type TrustSource struct {
	p      atomic.Pointer[TrustPolicy]
	Denied func(error) // optional: told about refused peers (audit)
}

// Load returns the current policy (nil: none).
func (s *TrustSource) Load() *TrustPolicy {
	if s == nil {
		return nil
	}
	return s.p.Load()
}

// Store installs p.
func (s *TrustSource) Store(p *TrustPolicy) { s.p.Store(p) }

func (s *TrustSource) denied(err error) {
	if s != nil && s.Denied != nil {
		s.Denied(err)
	}
}
