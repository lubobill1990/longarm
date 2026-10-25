// Package relaytest has test helpers: a throwaway CA with bundles, and an
// in-process relay server on loopback.
package relaytest

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/relay"
)

// PKI is a test CA.
type PKI struct {
	Dir string
	CA  *pki.CA
}

// NewPKI creates a CA in a temp dir.
func NewPKI(t testing.TB) *PKI {
	t.Helper()
	dir := t.TempDir()
	ca, err := pki.InitCA(filepath.Join(dir, "ca"), "test CA", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return &PKI{Dir: dir, CA: ca}
}

// Bundle issues and loads a bundle for role/name.
func (p *PKI) Bundle(t testing.TB, role pki.Role, name string) *pki.Bundle {
	t.Helper()
	c, k, err := p.CA.Issue(pki.IssueOptions{Role: role, Name: name, Hosts: []string{"127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(p.Dir, string(role)+"-"+name)
	if err := pki.WriteBundle(dir, p.CA.CertPEM, c, k); err != nil {
		t.Fatal(err)
	}
	b, err := pki.LoadBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Relay is a running in-process relay.
type Relay struct {
	Server *relay.Server
	UDP    net.PacketConn
	TCP    net.Listener
	STUN2  net.PacketConn // second STUN port (NAT type detection)
	Addr   string         // UDP address
	TCPAdr string
}

// Logger returns a logger writing to stderr when LONGARM_NET_TEST_LOG=1.
func Logger() *slog.Logger {
	if os.Getenv("LONGARM_NET_TEST_LOG") == "1" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.DiscardHandler)
}

// StartRelay starts a relay on 127.0.0.1 (addr "" = random ports; UDP and
// TCP use the same port number when possible).
func StartRelay(t testing.TB, b *pki.Bundle, addr string) *Relay {
	t.Helper()
	tlsConf, err := relay.ServerTLS(b)
	if err != nil {
		t.Fatal(err)
	}
	return StartRelayWith(t, &relay.Server{TLS: tlsConf, Logger: Logger()}, addr)
}

// StartRelayWith serves s (configured by the caller) like StartRelay.
func StartRelayWith(t testing.TB, s *relay.Server, addr string) *Relay {
	t.Helper()
	var err error
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	var pc net.PacketConn
	var ln net.Listener
	for i := 0; ; i++ {
		pc, err = net.ListenPacket("udp", addr)
		if err != nil {
			t.Fatal(err)
		}
		ln, err = net.Listen("tcp", pc.LocalAddr().String())
		if err == nil {
			break
		}
		pc.Close()
		if addr != "127.0.0.1:0" || i > 20 {
			t.Fatal(err)
		}
	}
	stun2, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &Relay{Server: s, UDP: pc, TCP: ln, STUN2: stun2, Addr: pc.LocalAddr().String(), TCPAdr: ln.Addr().String()}
	if s.STUN == nil {
		s.STUN = []string{fmt.Sprintf(":%d", pc.LocalAddr().(*net.UDPAddr).Port), fmt.Sprintf(":%d", stun2.LocalAddr().(*net.UDPAddr).Port)}
	}
	go s.ServeQUIC(pc)
	go s.ServeTCP(ln)
	go s.ServeSTUN(stun2)
	t.Cleanup(func() { s.Close(); pc.Close(); stun2.Close() })
	return r
}

// Close stops the relay and frees its ports.
func (r *Relay) Close() {
	r.Server.Close()
	r.UDP.Close()
	r.TCP.Close()
	r.STUN2.Close()
}
