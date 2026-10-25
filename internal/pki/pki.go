// Package pki is Longarm's small CA and app-level mTLS (DESIGN §6).
//
// Every node holds a bundle: ca.crt (trust root), node.crt, node.key. The
// node identity is carried in the certificate as a URI SAN
// "longarm://<role>/<name>" (also mirrored as OU=<role>, CN=<name> for
// humans). Roles: worker (longarmd), client (longarm), admin (longarm with admin rights),
// relay (longarm-relay, the longnet relay/rendezvous server), release (CI: may only
// publish releases and start/inspect/abort rollouts on the hub).
// Worker certificates also carry DNS SAN <name> so clients can verify the
// server with plain ServerName checking, independent of the address dialed.
//
// P0a: certificates are issued manually with `longarm admin issue`; hub-issued,
// auto-renewed certs and revocation arrive in P1.
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// Role of a certificate holder.
type Role string

const (
	RoleWorker Role = "worker"
	RoleClient Role = "client"
	RoleAdmin  Role = "admin"
	RoleRelay  Role = "relay"
	RoleHub    Role = "hub" // longarm-hub's control-plane API server
	// RoleRelease is the CI's credential: publish releases and run rollouts
	// on the hub, nothing else (no workers, jobs, nodes or exec).
	RoleRelease Role = "release"
)

// ParseRole validates a role string.
func ParseRole(s string) (Role, error) {
	switch r := Role(s); r {
	case RoleWorker, RoleClient, RoleAdmin, RoleRelay, RoleHub, RoleRelease:
		return r, nil
	}
	return "", fmt.Errorf("unknown role %q (want worker|client|admin|relay|hub|release)", s)
}

// URIScheme is the scheme of the identity URI SAN.
const URIScheme = "longarm"

// Bundle file names.
const (
	CACertFile   = "ca.crt"
	CAKeyFile    = "ca.key"
	NodeCertFile = "node.crt"
	NodeKeyFile  = "node.key"
)

// DefaultValidity per role (DESIGN §6: worker 90 days, client 30 days; relay
// servers like workers). The release (CI) certificate lives in a CI secret
// that nothing renews: 180 days, then the owner re-issues it.
func DefaultValidity(r Role) time.Duration {
	if r == RoleWorker || r == RoleRelay || r == RoleHub {
		return 90 * 24 * time.Hour
	}
	if r == RoleRelease {
		return 180 * 24 * time.Hour
	}
	return 30 * 24 * time.Hour
}

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// ValidName reports whether name is usable as a node name (and DNS SAN).
func ValidName(name string) bool { return nameRE.MatchString(name) }

// Identity is who a certificate belongs to.
type Identity struct {
	Role Role
	Name string
}

func (i Identity) String() string { return string(i.Role) + "/" + i.Name }

// IdentityFromCert extracts the identity from the longarm:// URI SAN.
func IdentityFromCert(c *x509.Certificate) (Identity, error) {
	for _, u := range c.URIs {
		if u.Scheme != URIScheme {
			continue
		}
		role, err := ParseRole(u.Host)
		if err != nil {
			return Identity{}, err
		}
		name := strings.TrimPrefix(u.Path, "/")
		if !ValidName(name) {
			return Identity{}, fmt.Errorf("bad name %q in identity URI", name)
		}
		return Identity{Role: role, Name: name}, nil
	}
	return Identity{}, errors.New("certificate has no longarm identity")
}

// CA is a loaded certificate authority.
type CA struct {
	Cert    *x509.Certificate
	CertPEM []byte
	key     *ecdsa.PrivateKey
}

func newKey() (*ecdsa.PrivateKey, error) { return ecdsa.GenerateKey(elliptic.P256(), rand.Reader) }

func serial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

func encodeKey(k *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func encodeCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// InitCA creates a new CA in dir (ca.crt, ca.key). It refuses to overwrite.
func InitCA(dir, commonName string, validity time.Duration) (*CA, error) {
	if _, err := os.Stat(filepath.Join(dir, CAKeyFile)); err == nil {
		return nil, fmt.Errorf("CA already exists in %s", dir)
	}
	key, err := newKey()
	if err != nil {
		return nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          sn,
		Subject:               pkix.Name{CommonName: commonName, Organization: []string{"longarm"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	keyPEM, err := encodeKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	ca := &CA{Cert: cert, CertPEM: encodeCert(der), key: key}
	if err := os.WriteFile(filepath.Join(dir, CACertFile), ca.CertPEM, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, CAKeyFile), keyPEM, 0o600); err != nil {
		return nil, err
	}
	return ca, nil
}

// LoadCA loads ca.crt + ca.key from dir.
func LoadCA(dir string) (*CA, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, CACertFile))
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, CAKeyFile))
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	key, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("CA key is not ECDSA")
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, CertPEM: certPEM, key: key}, nil
}

// IssueOptions for Issue.
type IssueOptions struct {
	Role     Role
	Name     string
	Hosts    []string // extra DNS names / IPs (workers, relays)
	Validity time.Duration
}

// Issue creates a key pair and a certificate signed by the CA.
func (ca *CA) Issue(o IssueOptions) (certPEM, keyPEM []byte, err error) {
	key, err := newKey()
	if err != nil {
		return nil, nil, err
	}
	certPEM, err = ca.sign(&key.PublicKey, o)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err = encodeKey(key)
	if err != nil {
		return nil, nil, err
	}
	return certPEM, keyPEM, nil
}

// sign issues a certificate for pub. The identity, usages and lifetime come
// from o only - never from a CSR's subject.
func (ca *CA) sign(pub any, o IssueOptions) ([]byte, error) {
	if _, err := ParseRole(string(o.Role)); err != nil {
		return nil, err
	}
	if !ValidName(o.Name) {
		return nil, fmt.Errorf("invalid name %q (letters, digits, . _ -)", o.Name)
	}
	if o.Validity == 0 {
		o.Validity = DefaultValidity(o.Role)
	}
	sn, err := serial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: sn,
		Subject: pkix.Name{
			CommonName:         o.Name,
			OrganizationalUnit: []string{string(o.Role)},
			Organization:       []string{"longarm"},
		},
		NotBefore:   now.Add(-time.Hour),
		NotAfter:    now.Add(o.Validity),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:        []*url.URL{{Scheme: URIScheme, Host: string(o.Role), Path: "/" + o.Name}},
	}
	if o.Role == RoleWorker || o.Role == RoleRelay || o.Role == RoleHub {
		tmpl.ExtKeyUsage = append(tmpl.ExtKeyUsage, x509.ExtKeyUsageServerAuth)
		tmpl.DNSNames = []string{o.Name}
		for _, h := range o.Hosts {
			if ip := net.ParseIP(h); ip != nil {
				tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
			} else if h != "" && h != o.Name {
				tmpl.DNSNames = append(tmpl.DNSNames, h)
			}
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, pub, ca.key)
	if err != nil {
		return nil, err
	}
	return encodeCert(der), nil
}

// WriteBundle writes ca.crt, node.crt, node.key into dir.
func WriteBundle(dir string, caPEM, certPEM, keyPEM []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, CACertFile), caPEM, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, NodeCertFile), certPEM, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, NodeKeyFile), keyPEM, 0o600)
}

// Bundle is a loaded node bundle.
type Bundle struct {
	Pool     *x509.CertPool
	Cert     tls.Certificate
	Identity Identity
	NotAfter time.Time

	// live, when set (Reloader.LiveBundle), holds the current certificate:
	// TLS configs built from this bundle present it on every handshake, so
	// long-lived listeners and dialers follow a renewed certificate.
	live *atomic.Pointer[tls.Certificate]
}

// Certificate returns the certificate to present now.
func (b *Bundle) Certificate() *tls.Certificate {
	if b.live != nil {
		if c := b.live.Load(); c != nil {
			return c
		}
	}
	return &b.Cert
}

// GetCertificate is a tls.Config.GetCertificate returning Certificate().
func (b *Bundle) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return b.Certificate(), nil
}

// GetClientCertificate is a tls.Config.GetClientCertificate returning
// Certificate().
func (b *Bundle) GetClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return b.Certificate(), nil
}

// LoadBundle loads ca.crt, node.crt, node.key from dir.
func LoadBundle(dir string) (*Bundle, error) {
	caPEM, err := os.ReadFile(filepath.Join(dir, CACertFile))
	if err != nil {
		return nil, fmt.Errorf("load TLS bundle: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no CA certificate in %s", filepath.Join(dir, CACertFile))
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, NodeCertFile), filepath.Join(dir, NodeKeyFile))
	if err != nil {
		return nil, fmt.Errorf("load TLS bundle: %w", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, err
	}
	id, err := IdentityFromCert(leaf)
	if err != nil {
		return nil, err
	}
	cert.Leaf = leaf
	return &Bundle{Pool: pool, Cert: cert, Identity: id, NotAfter: leaf.NotAfter}, nil
}

// ServerConfig is the longarmd side: TLS 1.3, client certificate required and
// verified against the CA. Authorization by role happens per RPC.
func (b *Bundle) ServerConfig() (*tls.Config, error) {
	if b.Identity.Role != RoleWorker {
		return nil, fmt.Errorf("longarmd needs a worker certificate, got %s", b.Identity)
	}
	return &tls.Config{
		MinVersion:     tls.VersionTLS13,
		GetCertificate: b.GetCertificate,
		ClientAuth:     tls.RequireAndVerifyClientCert,
		ClientCAs:      b.Pool,
		NextProtos:     []string{"h2"},
	}, nil
}

// ClientConfig is the longarm side: verifies the worker's certificate chain, that
// it was issued for serverName, and that it carries the worker role.
func (b *Bundle) ClientConfig(serverName string) *tls.Config {
	return &tls.Config{
		MinVersion:           tls.VersionTLS13,
		GetClientCertificate: b.GetClientCertificate,
		RootCAs:              b.Pool,
		ServerName:           serverName,
		NextProtos:           []string{"h2"},
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("no server certificate")
			}
			id, err := IdentityFromCert(cs.PeerCertificates[0])
			if err != nil {
				return err
			}
			if id.Role != RoleWorker {
				return fmt.Errorf("server %s is not a worker", id)
			}
			return nil
		},
	}
}
