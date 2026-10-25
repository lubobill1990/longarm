package relay

import "github.com/lubobill1990/longarm/internal/pki"

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
