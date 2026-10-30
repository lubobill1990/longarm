package signin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
)

// Current is an enrollment a sign-in target holds already (a client
// device's or a worker's certificate). Signing in again replaces it:
// at the same hub the pairing proves its key and the hub removes the old
// node at the approval (docs/security.md "Signing in again"); another
// hub's is overwritten here only.
type Current struct {
	Kind          string // "device" or "worker"
	Dir           string // config dir / data dir
	Hub           string // the hub address it recorded ("" if none)
	CAFingerprint string // its hub's CA
	Role, Name    string // its node
	// User: a device's Longarm user, a worker's owner ("" = a fleet
	// worker).
	User     string
	NotAfter time.Time
	// CertPEM, KeyPEM: the node's; they prove it to its hub.
	CertPEM, KeyPEM []byte `json:"-"`
}

// Identity is "role/name".
func (c *Current) Identity() string { return c.Role + "/" + c.Name }

// Expired reports whether the certificate has run out.
func (c *Current) Expired(now time.Time) bool { return !now.Before(c.NotAfter) }

// SameHub reports whether c belongs to the hub whose CA has fingerprint
// fp: a hub is its CA (its address may change).
func (c *Current) SameHub(fp string) bool {
	return c.CAFingerprint != "" && c.CAFingerprint == pki.NormalizeFingerprint(fp)
}

func (c *Current) String() string {
	s := c.Kind + " " + c.Identity()
	if c.User != "" && c.User != c.Name {
		if c.Kind == "worker" {
			s += " (owner " + c.User + ")"
		} else {
			s += " (user " + c.User + ")"
		}
	}
	return s
}

// Holder is a Target that reports the enrollment it holds
// (client.DeviceTarget, longarmd.WorkerTarget).
type Holder interface {
	// Current: the enrollment held now (nil: none).
	Current() (*Current, error)
}

// ReadCurrent reads the enrollment in dir (its tlsDir subdirectory and
// hub.toml): nil when there is no certificate. kind is "device" or
// "worker".
func ReadCurrent(kind, dir, tlsDir string) (*Current, error) {
	tls := filepath.Join(dir, tlsDir)
	certPEM, err := os.ReadFile(filepath.Join(tls, pki.NodeCertFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c, err := pki.ParseCertPEM(certPEM)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(tls, pki.NodeCertFile), err)
	}
	id, err := pki.IdentityFromCert(c)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(tls, pki.NodeCertFile), err)
	}
	cur := &Current{Kind: kind, Dir: dir, Role: string(id.Role), Name: id.Name, NotAfter: c.NotAfter, CertPEM: certPEM}
	cur.KeyPEM, _ = os.ReadFile(filepath.Join(tls, pki.NodeKeyFile))
	if b, err := os.ReadFile(filepath.Join(tls, pki.CACertFile)); err == nil {
		if ca, err := pki.ParseCertPEM(b); err == nil {
			cur.CAFingerprint = pki.Fingerprint(ca)
		}
	}
	hc, _ := hubclient.LoadFile(dir)
	cur.Hub = hc.Address
	if id.Role == pki.RoleWorker {
		cur.User = hc.Owner
	} else {
		cur.User = id.Owner()
	}
	return cur, nil
}

// SignedInError: the machine holds enrollments a sign-in would replace -
// valid ones of the same hub, any of another hub - and Options.Replace
// was not set. A GUI shows Current and asks; a CLI says how to confirm.
type SignedInError struct {
	Hub           string // the hub signing in to (its gRPC address)
	CAFingerprint string // its CA
	// Current: what would be replaced (another hub's: overwritten here,
	// not removed there).
	Current []*Current
}

// OtherHub reports whether any of e.Current is of another hub.
func (e *SignedInError) OtherHub() bool {
	for _, c := range e.Current {
		if !c.SameHub(e.CAFingerprint) {
			return true
		}
	}
	return false
}

func (e *SignedInError) Error() string {
	var parts []string
	for _, c := range e.Current {
		s := c.String()
		if c.Hub != "" && c.Hub != e.Hub {
			s += " at " + c.Hub
		}
		parts = append(parts, s)
	}
	return fmt.Sprintf("this machine is signed in already as %s; signing in to %s replaces that (to go on: --force)",
		strings.Join(parts, ", "), e.Hub)
}
