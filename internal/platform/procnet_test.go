package platform

import (
	"net/netip"
	"testing"
)

func TestProcNetSocketUID(t *testing.T) {
	tcp := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0000000000000000 100 0 0 10 0
   1: 0100007F:D431 0100007F:1F90 01 00000000:00000000 00:00000000 00000000  1001        0 2 1 0000000000000000 20 4 30 10 -1
   2: 0100007F:1F90 0100007F:D431 01 00000000:00000000 00:00000000 00000000     0        0 3 1 0000000000000000 20 4 30 10 -1
`
	client := netip.MustParseAddrPort("127.0.0.1:54321")
	server := netip.MustParseAddrPort("127.0.0.1:8080")
	if uid, ok := procNetSocketUID(tcp, client, server); !ok || uid != 1001 {
		t.Fatalf("client socket: %d %v", uid, ok)
	}
	if uid, ok := procNetSocketUID(tcp, server, client); !ok || uid != 0 {
		t.Fatalf("server socket: %d %v", uid, ok)
	}
	if _, ok := procNetSocketUID(tcp, netip.MustParseAddrPort("127.0.0.1:1"), server); ok {
		t.Fatal("found a socket that is not there")
	}
	// tcp6: ::1 and an IPv4-mapped loopback address
	tcp6 := `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000001000000:D431 00000000000000000000000001000000:1F90 01 00000000:00000000 00:00000000 00000000  1002        0 4 1
   1: 0000000000000000FFFF00000100007F:D432 0000000000000000FFFF00000100007F:1F90 01 00000000:00000000 00:00000000 00000000  1003        0 5 1
`
	if uid, ok := procNetSocketUID(tcp6, netip.MustParseAddrPort("[::1]:54321"), netip.MustParseAddrPort("[::1]:8080")); !ok || uid != 1002 {
		t.Fatalf("::1: %d %v", uid, ok)
	}
	if uid, ok := procNetSocketUID(tcp6, netip.MustParseAddrPort("127.0.0.1:54322"), server); !ok || uid != 1003 {
		t.Fatalf("mapped: %d %v", uid, ok)
	}
}
