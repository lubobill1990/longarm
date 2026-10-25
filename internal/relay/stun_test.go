package relay_test

import (
	"bytes"
	"encoding/hex"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/relay"
	"github.com/lubobill1990/longarm/internal/relay/relaytest"
)

func TestSTUNCodec(t *testing.T) {
	tx := relay.STUNTxID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	req := relay.STUNRequest(tx)
	if !relay.IsSTUN(req) || len(req) != 20 || req[0]&0xC0 != 0 {
		t.Fatalf("request %x", req)
	}
	// RFC 5769 §2.2 sample IPv4 response (XOR-MAPPED-ADDRESS
	// 192.0.2.1:32853, plus SOFTWARE, MESSAGE-INTEGRITY, FINGERPRINT).
	vec, _ := hex.DecodeString("0101003c2112a442b7e7a701bc34d686fa87dfae" +
		"8022000b7465737420766563746f7220" + "002000080001a147e112a643" +
		"00080014" + "2b91f599fd9e90c38c7489f92af9ba53f06be7d7" + "80280004c07d4c96")
	gotTx, addr, err := relay.ParseSTUNResponse(vec)
	if err != nil || addr != netip.MustParseAddrPort("192.0.2.1:32853") || hex.EncodeToString(gotTx[:]) != "b7e7a701bc34d686fa87dfae" {
		t.Fatalf("RFC 5769 vector: %v %v %x", addr, err, gotTx)
	}
	// RFC 5769 §2.3 sample IPv6 response.
	vec6, _ := hex.DecodeString("010100482112a442b7e7a701bc34d686fa87dfae" +
		"8022000b7465737420766563746f7220" + "002000140002a1470113a9faa5d3f179bc25f4b5bed2b9d9" +
		"00080014" + "a382954e4be67bf11784c97c8292c275bfe3ed41" + "80280004c8fb0b4c")
	if _, addr, err := relay.ParseSTUNResponse(vec6); err != nil || addr != netip.MustParseAddrPort("[2001:db8:1234:5678:11:2233:4455:6677]:32853") {
		t.Fatalf("RFC 5769 IPv6 vector: %v %v", addr, err)
	}
	// Garbage and truncation are rejected, never panic.
	for i := 0; i < len(vec); i++ {
		relay.ParseSTUNResponse(vec[:i])
	}
	if _, _, err := relay.ParseSTUNResponse(req); err == nil {
		t.Fatal("request parsed as response")
	}
	// QUIC / longnet packets are never STUN (fixed bit set).
	if relay.IsSTUN(append([]byte{0x40}, bytes.Repeat([]byte{0}, 30)...)) || relay.IsSTUN([]byte("LGNPxxxxxxxxxxxxxxxxxxxxxxxxx")) {
		t.Fatal("QUIC/LGN classified as STUN")
	}
}

// The relay answers STUN on its QUIC port and on the second port, and
// advertises both in Welcome.
func TestRelaySTUN(t *testing.T) {
	p := relaytest.NewPKI(t)
	r := relaytest.StartRelay(t, p.Bundle(t, pki.RoleRelay, "relay1"), "")
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	me := pc.LocalAddr().(*net.UDPAddr).AddrPort()
	for _, srv := range []net.Addr{r.UDP.LocalAddr(), r.STUN2.LocalAddr()} {
		tx := relay.STUNTxID{byte(len(srv.String()))}
		var got netip.AddrPort
		for try := 0; try < 5 && !got.IsValid(); try++ {
			pc.WriteTo(relay.STUNRequest(tx), srv)
			pc.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			buf := make([]byte, 1500)
			n, _, err := pc.ReadFrom(buf)
			if err != nil {
				continue
			}
			gotTx, addr, err := relay.ParseSTUNResponse(buf[:n])
			if err != nil || gotTx != tx {
				t.Fatalf("response from %v: %v %x", srv, err, gotTx)
			}
			got = addr
		}
		if got != me {
			t.Fatalf("%v saw us as %v, want %v", srv, got, me)
		}
	}
	if st := r.Server.Stats(); st.STUNRequests < 2 {
		t.Fatalf("stats %+v", st)
	}
	e := &env{p: p, relay: r}
	c := e.connect(t, p.Bundle(t, pki.RoleClient, "alice"), relay.ModeUDP, newRecorder())
	if len(c.STUN()) != 2 {
		t.Fatalf("welcome STUN %v", c.STUN())
	}
}
