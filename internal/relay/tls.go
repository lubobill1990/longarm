package relay

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"

	"github.com/lubobill1990/longarm/internal/pki"
)

// ServerTLS is the relay's TLS config: relay certificate, client certificate
// required and verified against the Longarm CA.
func ServerTLS(b *pki.Bundle) (*tls.Config, error) {
	if b.Identity.Role != pki.RoleRelay {
		return nil, fmt.Errorf("the relay needs a relay certificate, got %s (longarm admin issue --role relay)", b.Identity)
	}
	return &tls.Config{
		MinVersion:     tls.VersionTLS13,
		GetCertificate: b.GetCertificate,
		ClientAuth:     tls.RequireAndVerifyClientCert,
		ClientCAs:      b.Pool,
		NextProtos:     []string{ALPN},
	}, nil
}

// ClientTLS is a node's TLS config towards the relay. The relay must present
// a certificate of the Longarm CA with the relay role; when serverName is
// non-empty it must also be valid for that name. (Relays are reached by
// address, often an IP, and every relay certificate of the private CA is
// equally trusted, so the name check is optional.)
func ClientTLS(b *pki.Bundle, serverName string) *tls.Config {
	return &tls.Config{
		MinVersion:           tls.VersionTLS13,
		GetClientCertificate: b.GetClientCertificate, // follows a renewed certificate (live bundle)
		NextProtos:           []string{ALPN},
		InsecureSkipVerify:   true, // replaced by VerifyConnection below (chain + role [+ name])
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("relay presented no certificate")
			}
			leaf := cs.PeerCertificates[0]
			inter := x509.NewCertPool()
			for _, c := range cs.PeerCertificates[1:] {
				inter.AddCert(c)
			}
			if _, err := leaf.Verify(x509.VerifyOptions{Roots: b.Pool, Intermediates: inter,
				KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
				return fmt.Errorf("relay certificate: %w", err)
			}
			id, err := pki.IdentityFromCert(leaf)
			if err != nil {
				return err
			}
			if id.Role != pki.RoleRelay {
				return fmt.Errorf("relay server presented %s, not a relay certificate", id)
			}
			if serverName != "" {
				if err := leaf.VerifyHostname(serverName); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// peerIdentity extracts and checks a node identity from a verified TLS state.
func peerIdentity(cs tls.ConnectionState) (pki.Identity, error) {
	if len(cs.PeerCertificates) == 0 {
		return pki.Identity{}, errors.New("no client certificate")
	}
	id, err := pki.IdentityFromCert(cs.PeerCertificates[0])
	if err != nil {
		return pki.Identity{}, err
	}
	switch id.Role {
	case pki.RoleRelay:
		return pki.Identity{}, errors.New("relay certificates cannot be nodes")
	case pki.RoleRelease: // CI: talks to the hub's API only, never to workers
		return pki.Identity{}, errors.New("release certificates cannot use the relay")
	}
	return id, nil
}

// rolesAllowed is the base forwarding policy: packets and rendezvous between
// a worker and a non-worker (client/admin). Clients don't talk to clients;
// workers talk to workers only as a port link allows (Server.SetWorkerPairs).
func rolesAllowed(a, b pki.Role) bool {
	return (a == pki.RoleWorker) != (b == pki.RoleWorker)
}

// pairKey is an unordered pair of worker names.
type pairKey struct{ a, b string }

func newPairKey(a, b string) pairKey {
	if b < a {
		a, b = b, a
	}
	return pairKey{a, b}
}

// SetWorkerPairs replaces the worker pairs that may talk to each other
// through the relay (both directions): the hub passes the pairs of its
// active port links and updates them on every change. Default: none - a
// standalone longarm-relay never relays between workers.
func (s *Server) SetWorkerPairs(pairs [][2]string) {
	m := make(map[pairKey]struct{}, len(pairs))
	for _, p := range pairs {
		m[newPairKey(p[0], p[1])] = struct{}{}
	}
	s.workerPairs.Store(&m)
}

// allowed is the forwarding policy between two authenticated nodes.
func (s *Server) allowed(a, b pki.Identity) bool {
	if rolesAllowed(a.Role, b.Role) {
		return true
	}
	if a.Role != pki.RoleWorker || b.Role != pki.RoleWorker || a.Name == b.Name {
		return false
	}
	m := s.workerPairs.Load()
	if m == nil {
		return false
	}
	_, ok := (*m)[newPairKey(a.Name, b.Name)]
	return ok
}
