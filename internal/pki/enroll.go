package pki

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lubobill1990/longarm/internal/platform"
)

// Enrollment with longarm-hub (DESIGN §6.3, §11.2): a joining node generates its
// key locally and sends only a CSR; the hub signs it with the identity it
// decided (role and name from the join code and the node registry - never
// from the CSR's subject). Renewal works the same over the authenticated
// channel.

// CSRTrust is a node's end-to-end trust material (trust.go) for its CSRs:
// the node key and, once a user endorsed it, the endorsement.
type CSRTrust struct {
	NodeKey     ed25519.PrivateKey
	Endorsement *Endorsement
}

// NewKeyAndCSR creates a TLS key and a CSR for it; with t the CSR asks for
// the trust extension binding t's node key to the new key.
func NewKeyAndCSR(name string, t *CSRTrust) (keyPEM, csrPEM []byte, err error) {
	key, err := newKey()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: name, Organization: []string{"longarm"}},
	}
	if t != nil && t.NodeKey != nil {
		ext, err := TrustExtension(t.NodeKey, &key.PublicKey, t.Endorsement)
		if err != nil {
			return nil, nil, err
		}
		tmpl.ExtraExtensions = []pkix.Extension{ext}
	}
	der, err := x509.CreateCertificateRequest(nil, tmpl, key)
	if err != nil {
		return nil, nil, err
	}
	if keyPEM, err = encodeKey(key); err != nil {
		return nil, nil, err
	}
	return keyPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}

// ParseCSR decodes and checks the self-signature of a PEM CSR.
func ParseCSR(csrPEM []byte) (*x509.CertificateRequest, error) {
	blk, _ := pem.Decode(csrPEM)
	if blk == nil || blk.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("no PEM certificate request")
	}
	csr, err := x509.ParseCertificateRequest(blk.Bytes)
	if err != nil {
		return nil, err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("CSR signature: %w", err)
	}
	return csr, nil
}

// SignCSR issues a certificate for the CSR's public key with the identity,
// usages and lifetime of o. A trust extension in the CSR (bound to its key)
// is copied as is: the only part of a CSR besides the key that reaches the
// certificate.
func (ca *CA) SignCSR(csrPEM []byte, o IssueOptions) ([]byte, error) {
	csr, err := ParseCSR(csrPEM)
	if err != nil {
		return nil, err
	}
	ext, err := csrTrustExt(csr)
	if err != nil {
		return nil, err
	}
	if ext != nil {
		o.extra = append(o.extra, *ext)
	}
	return ca.sign(csr.PublicKey, o)
}

// ParseCertPEM decodes the first certificate of a PEM block.
func ParseCertPEM(b []byte) (*x509.Certificate, error) {
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil, errors.New("no PEM certificate")
	}
	return x509.ParseCertificate(blk.Bytes)
}

// Serial is a certificate's serial number as lowercase hex (the key of the
// revocation list).
func Serial(c *x509.Certificate) string { return c.SerialNumber.Text(16) }

// Fingerprint is the SHA-256 of a certificate's DER, hex encoded - how users
// pin the hub's CA (`--ca-fingerprint`).
func Fingerprint(c *x509.Certificate) string {
	s := sha256.Sum256(c.Raw)
	return hex.EncodeToString(s[:])
}

// NormalizeFingerprint lowercases a fingerprint and strips ':' and spaces.
func NormalizeFingerprint(s string) string {
	return strings.ToLower(strings.NewReplacer(":", "", " ", "", "sha256/", "", "SHA256:", "").Replace(s))
}

// WriteBundleAtomic writes a bundle so that a concurrent reader (longarmd's
// Reloader) never loads a mismatched certificate and key: each file is
// written to a temp name and renamed, the key and certificate last.
func WriteBundleAtomic(dir string, caPEM, certPEM, keyPEM []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	write := func(name string, b []byte, mode os.FileMode) error {
		tmp := filepath.Join(dir, "."+name+".tmp")
		if err := os.WriteFile(tmp, b, mode); err != nil {
			return err
		}
		return platform.ReplaceFile(tmp, filepath.Join(dir, name))
	}
	if err := write(CACertFile, caPEM, 0o644); err != nil {
		return err
	}
	if err := write(NodeKeyFile, keyPEM, 0o600); err != nil {
		return err
	}
	return write(NodeCertFile, certPEM, 0o644)
}

// ---------------------------------------------------------------- revocation

// RevocationList is the set of revoked certificate serials (DESIGN §6.2:
// pushed by the hub with the heartbeat; longarmd rejects revoked client
// certificates). Entries are kept until the certificate would have expired.
type RevocationList struct {
	path string

	mu      sync.RWMutex
	serials map[string]time.Time // serial -> not_after
	version int64
	changed chan struct{} // closed (and replaced) by Replace
}

type revocationFile struct {
	Version int64            `json:"version"`
	Serials map[string]int64 `json:"serials"` // serial -> not_after (unix)
}

// LoadRevocationList loads path (missing = empty); Replace persists there.
func LoadRevocationList(path string) (*RevocationList, error) {
	r := &RevocationList{path: path, serials: map[string]time.Time{}}
	if path == "" {
		return r, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	var f revocationFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for s, na := range f.Serials {
		r.serials[s] = time.Unix(na, 0)
	}
	r.version = f.Version
	return r, nil
}

// Revoked reports whether the certificate is on the list.
func (r *RevocationList) Revoked(c *x509.Certificate) bool {
	if r == nil || c == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.serials[Serial(c)]
	return ok
}

// Version of the list (the hub's counter; 0 = never received).
func (r *RevocationList) Version() int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.version
}

// Len is the number of entries.
func (r *RevocationList) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.serials)
}

// Replace installs a new full list (expired entries are dropped) and
// persists it.
func (r *RevocationList) Replace(version int64, serials map[string]time.Time) error {
	m := make(map[string]time.Time, len(serials))
	f := revocationFile{Version: version, Serials: map[string]int64{}}
	now := time.Now()
	for s, na := range serials {
		if na.Before(now) {
			continue
		}
		m[s] = na
		f.Serials[s] = na.Unix()
	}
	r.mu.Lock()
	r.serials, r.version = m, version
	if r.changed != nil {
		close(r.changed)
	}
	r.changed = make(chan struct{})
	r.mu.Unlock()
	if r.path == "" {
		return nil
	}
	b, _ := json.Marshal(f)
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return platform.ReplaceFile(tmp, r.path)
}

// VerifyNotRevoked is a tls.Config.VerifyPeerCertificate hook rejecting
// revoked leaf certificates (chains were already verified by crypto/tls).
func (r *RevocationList) VerifyNotRevoked(_ [][]byte, chains [][]*x509.Certificate) error {
	for _, ch := range chains {
		if len(ch) > 0 && r.Revoked(ch[0]) {
			return fmt.Errorf("certificate %s is revoked", Serial(ch[0]))
		}
	}
	return nil
}

// LiveBundle returns a bundle whose TLS configurations follow later
// certificate reloads (same identity only). Listeners and dialers created
// from it present the current certificate on every handshake.
func (r *Reloader) LiveBundle() *Bundle {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.live == nil {
		b := *r.bundle
		b.live = new(atomic.Pointer[tls.Certificate])
		b.live.Store(&r.bundle.Cert)
		r.live = &b
	}
	return r.live
}

// Leaf returns the parsed current certificate.
func (b *Bundle) Leaf() *x509.Certificate { return b.Certificate().Leaf }

// Live returns a copy of b whose certificate Update can swap later (for
// long-lived clients such as longarm daemon / longarm mcp).
func (b *Bundle) Live() *Bundle {
	c := *b
	c.live = new(atomic.Pointer[tls.Certificate])
	c.live.Store(b.Certificate())
	return &c
}

// Update installs nb's certificate into a live bundle (same identity only).
func (b *Bundle) Update(nb *Bundle) error {
	if b.live == nil {
		return errors.New("not a live bundle")
	}
	if nb.Identity != b.Identity {
		return fmt.Errorf("identity changed from %s to %s", b.Identity, nb.Identity)
	}
	b.live.Store(&nb.Cert)
	return nil
}

// Changed returns a channel closed at the next Replace.
func (r *RevocationList) Changed() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.changed == nil {
		r.changed = make(chan struct{})
	}
	return r.changed
}
