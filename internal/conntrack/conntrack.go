// Package conntrack closes established mTLS connections whose client
// certificate was revoked. A revocation list stops new handshakes and new
// RPCs, but streams already running (attach, forwards, log follows, hub
// event streams) live on their HTTP/2 connection; closing the connection
// ends them all.
package conntrack

import (
	"context"
	"crypto/x509"
	"net"
	"sync"

	"google.golang.org/grpc/credentials"
)

// Tracker records server-side TLS connections by peer certificate.
type Tracker struct {
	mu    sync.Mutex
	conns map[*conn]*x509.Certificate
}

// New returns an empty tracker.
func New() *Tracker { return &Tracker{conns: map[*conn]*x509.Certificate{}} }

// Wrap returns transport credentials that register every server handshake.
func (t *Tracker) Wrap(c credentials.TransportCredentials) credentials.TransportCredentials {
	return &creds{TransportCredentials: c, t: t}
}

// CloseIf closes the connections whose peer certificate matches; it
// returns how many were closed.
func (t *Tracker) CloseIf(match func(*x509.Certificate) bool) int {
	t.mu.Lock()
	var victims []*conn
	for c, cert := range t.conns {
		if match(cert) {
			victims = append(victims, c)
		}
	}
	t.mu.Unlock()
	for _, c := range victims {
		c.Close()
	}
	return len(victims)
}

// Len is the number of tracked connections.
func (t *Tracker) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.conns)
}

type creds struct {
	credentials.TransportCredentials
	t *Tracker
}

func (c *creds) ServerHandshake(raw net.Conn) (net.Conn, credentials.AuthInfo, error) {
	nc, ai, err := c.TransportCredentials.ServerHandshake(raw)
	if err != nil {
		return nc, ai, err
	}
	ti, ok := ai.(credentials.TLSInfo)
	if !ok || len(ti.State.PeerCertificates) == 0 {
		return nc, ai, nil
	}
	tc := &conn{Conn: nc, t: c.t}
	c.t.mu.Lock()
	c.t.conns[tc] = ti.State.PeerCertificates[0]
	c.t.mu.Unlock()
	return tc, ai, nil
}

func (c *creds) Clone() credentials.TransportCredentials {
	return &creds{TransportCredentials: c.TransportCredentials.Clone(), t: c.t}
}

type conn struct {
	net.Conn
	t    *Tracker
	once sync.Once
}

func (c *conn) Close() error {
	c.once.Do(func() {
		c.t.mu.Lock()
		delete(c.t.conns, c)
		c.t.mu.Unlock()
	})
	return c.Conn.Close()
}

// Watch closes revoked connections whenever changed fires, until ctx ends.
// changed must return a fresh channel each time (pki.RevocationList.Changed).
func (t *Tracker) Watch(ctx context.Context, changed func() <-chan struct{}, revoked func(*x509.Certificate) bool, onClose func(int)) {
	for {
		ch := changed()
		select {
		case <-ctx.Done():
			return
		case <-ch:
		}
		if n := t.CloseIf(revoked); n > 0 && onClose != nil {
			onClose(n)
		}
	}
}
