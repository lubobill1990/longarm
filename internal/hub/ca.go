package hub

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/platform"
)

// ImportCA copies an existing CA (ca.crt + ca.key, e.g. the
// ~/.config/longarm/ca made by `longarm admin ca init`) into the hub's data dir,
// so certificates issued so far stay valid.
func ImportCA(dataDir, src string, force bool) (*pki.CA, error) {
	ca, err := pki.LoadCA(src)
	if err != nil {
		return nil, fmt.Errorf("load CA from %s: %w", src, err)
	}
	dst := filepath.Join(dataDir, CADir)
	if _, err := os.Stat(filepath.Join(dst, pki.CAKeyFile)); err == nil && !force {
		return nil, fmt.Errorf("the hub already has a CA in %s (use --force to replace it)", dst)
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return nil, err
	}
	for _, f := range []string{pki.CACertFile, pki.CAKeyFile} {
		if err := copyFile(filepath.Join(src, f), filepath.Join(dst, f), 0o600); err != nil {
			return nil, err
		}
	}
	// Hub/relay certificates of an older CA are re-issued on the next start.
	return ca, nil
}

// InitCA creates a new CA in the hub's data dir.
func InitCA(dataDir string) (*pki.CA, error) {
	return pki.InitCA(filepath.Join(dataDir, CADir), "Longarm CA", 10*365*24*time.Hour)
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return platform.ReplaceFile(tmp, dst)
}

// serverHosts are the SANs of the hub's own certificates.
func (c *Config) serverHosts() []string {
	hosts := []string{"localhost", "127.0.0.1", "::1"}
	if h, _, err := net.SplitHostPort(c.PublicAddr); err == nil && h != "" {
		hosts = append(hosts, h)
	}
	hosts = append(hosts, c.Hosts...)
	slices.Sort(hosts)
	return slices.Compact(hosts)
}

// ensureServerCert loads the hub's certificate for role (hub or relay) from
// dir, issuing a new one when it is missing, from another CA, expiring
// within 30 days, or lacks a configured host.
func ensureServerCert(ca *pki.CA, dir string, role pki.Role, name string, hosts []string) (*pki.Bundle, error) {
	if b, err := pki.LoadBundle(dir); err == nil && serverCertOK(ca, b, role, name, hosts) {
		return b, nil
	}
	cert, key, err := ca.Issue(pki.IssueOptions{Role: role, Name: name, Hosts: hosts, Validity: 365 * 24 * time.Hour})
	if err != nil {
		return nil, err
	}
	if err := pki.WriteBundleAtomic(dir, ca.CertPEM, cert, key); err != nil {
		return nil, err
	}
	return pki.LoadBundle(dir)
}

func serverCertOK(ca *pki.CA, b *pki.Bundle, role pki.Role, name string, hosts []string) bool {
	leaf := b.Cert.Leaf
	if b.Identity != (pki.Identity{Role: role, Name: name}) || time.Until(leaf.NotAfter) < 30*24*time.Hour {
		return false
	}
	if err := leaf.CheckSignatureFrom(ca.Cert); err != nil {
		return false
	}
	for _, h := range hosts {
		if leaf.VerifyHostname(h) != nil {
			return false
		}
	}
	return true
}

// withChain appends the CA certificate to a server certificate, so joining
// nodes (no CA yet) can verify the hub against a pinned CA fingerprint.
func withChain(cert *tls.Certificate, ca *pki.CA) *tls.Certificate {
	c := *cert
	c.Certificate = append(slices.Clone(c.Certificate), ca.Cert.Raw)
	return &c
}

// apiTLS is the gRPC API's TLS config: client certificates are verified if
// presented (only Enroll works without one), revoked ones are refused.
func (h *Hub) apiTLS() *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(h.ca.Cert)
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return withChain(h.hubCert.Certificate(), h.ca), nil // live: renewed in maintain
		},
		ClientAuth:            tls.VerifyClientCertIfGiven,
		ClientCAs:             pool,
		NextProtos:            []string{"h2"},
		VerifyPeerCertificate: h.revoked.VerifyNotRevoked,
	}
}

var errNoCA = errors.New("the hub has no CA: run `longarm-hub ca import ~/.config/longarm/ca` or `longarm-hub ca init`")

// renewServerCerts re-issues the hub's own certificates when they get
// close to expiry (checked by maintain); the live bundles switch at once.
func (h *Hub) renewServerCerts() {
	hosts := h.cfg.serverHosts()
	for _, c := range []struct {
		b    *pki.Bundle
		dir  string
		role pki.Role
	}{{h.hubCert, HubTLSDir, pki.RoleHub}, {h.relayCert, RelayTLSDir, pki.RoleRelay}} {
		nb, err := ensureServerCert(h.ca, filepath.Join(h.cfg.DataDir, c.dir), c.role, h.cfg.Name, hosts)
		if err != nil {
			h.log.Warn("renew server certificate", "role", c.role, "err", err)
			continue
		}
		if pki.Serial(nb.Cert.Leaf) != pki.Serial(c.b.Leaf()) {
			if err := c.b.Update(nb); err == nil {
				h.log.Info("server certificate renewed", "role", c.role, "not_after", nb.NotAfter)
			}
		}
	}
}
