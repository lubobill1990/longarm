//go:build windows || linux

package platform

import (
	"net"
	"testing"
)

// testPeerIsAccount: a loopback connection from this process is "me" and
// not "other".
func testPeerIsAccount(t *testing.T, me, other *Account) {
	for _, network := range []string{"tcp4", "tcp6"} {
		addr := map[string]string{"tcp4": "127.0.0.1:0", "tcp6": "[::1]:0"}[network]
		ln, err := net.Listen(network, addr)
		if err != nil {
			t.Logf("%s: %v (skipped)", network, err)
			continue
		}
		c, err := net.Dial(network, ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		s, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := PeerIsAccount(s, me); err != nil || !ok {
			t.Fatalf("%s: peer is me: %v %v", network, ok, err)
		}
		if ok, err := PeerIsAccount(s, other); err != nil || ok {
			t.Fatalf("%s: peer is other: %v %v", network, ok, err)
		}
		c.Close()
		s.Close()
		ln.Close()
	}
}
