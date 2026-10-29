package relay

import (
	"crypto/tls"
	"crypto/x509"

	"github.com/lubobill1990/longarm/internal/pki"
)

// Kick closes the sessions of the nodes match selects (e.g. a revoked
// node; its reconnects then fail in the TLS handshake) and returns how many
// were closed.
func (s *Server) Kick(match func(pki.Identity) bool, reason string) int {
	s.init()
	s.mu.Lock()
	var victims []*session
	for _, ss := range s.sessions {
		if match(ss.ident) {
			victims = append(victims, ss)
		}
	}
	s.mu.Unlock()
	for _, ss := range victims {
		ss.close(reason)
	}
	return len(victims)
}

// KickCerts closes the sessions whose certificate match selects (e.g. one
// on a new revocation list) and returns how many were closed.
func (s *Server) KickCerts(match func(*x509.Certificate) bool, reason string) int {
	s.init()
	s.mu.Lock()
	var victims []*session
	for _, ss := range s.sessions {
		if ss.cert != nil && match(ss.cert) {
			victims = append(victims, ss)
		}
	}
	s.mu.Unlock()
	for _, ss := range victims {
		ss.close(reason)
	}
	return len(victims)
}

// leafOf is the peer's leaf certificate of a verified TLS state.
func leafOf(cs tls.ConnectionState) *x509.Certificate {
	if len(cs.PeerCertificates) == 0 {
		return nil
	}
	return cs.PeerCertificates[0]
}
