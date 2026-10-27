package platform

import (
	"encoding/binary"
	"encoding/hex"
	"net/netip"
	"strconv"
	"strings"
)

// procNetSocketUID finds, in the text of Linux's /proc/net/tcp or tcp6, the
// socket whose local address is local and remote address is remote, and
// returns its uid. Addresses there are hex: IPv4 as one 32-bit word, IPv6
// as four, each word in host byte order (little-endian on the platforms
// longarmd supports); ports big-endian.
func procNetSocketUID(table string, local, remote netip.AddrPort) (int, bool) {
	for i, line := range strings.Split(table, "\n") {
		fs := strings.Fields(line)
		if i == 0 || len(fs) < 8 {
			continue
		}
		l, ok1 := parseProcAddr(fs[1])
		r, ok2 := parseProcAddr(fs[2])
		if !ok1 || !ok2 || l != local || r != remote {
			continue
		}
		uid, err := strconv.Atoi(fs[7])
		if err != nil {
			return 0, false
		}
		return uid, true
	}
	return 0, false
}

// parseProcAddr parses "0100007F:1F90" / 32 hex digits + ":PORT" (IPv4
// mapped addresses unmapped).
func parseProcAddr(s string) (netip.AddrPort, bool) {
	ipHex, portHex, ok := strings.Cut(s, ":")
	if !ok {
		return netip.AddrPort{}, false
	}
	b, err := hex.DecodeString(ipHex)
	if err != nil || (len(b) != 4 && len(b) != 16) {
		return netip.AddrPort{}, false
	}
	for w := 0; w+4 <= len(b); w += 4 { // each 32-bit word: host (little-endian) order
		binary.BigEndian.PutUint32(b[w:], binary.LittleEndian.Uint32(b[w:]))
	}
	p, err := strconv.ParseUint(portHex, 16, 16)
	if err != nil {
		return netip.AddrPort{}, false
	}
	addr, _ := netip.AddrFromSlice(b)
	return netip.AddrPortFrom(addr.Unmap(), uint16(p)), true
}

// tcpEnds returns the two ends of a TCP connection (unmapped).
func tcpEnds(local, remote string) (netip.AddrPort, netip.AddrPort, bool) {
	l, err1 := netip.ParseAddrPort(local)
	r, err2 := netip.ParseAddrPort(remote)
	if err1 != nil || err2 != nil {
		return netip.AddrPort{}, netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(l.Addr().Unmap(), l.Port()), netip.AddrPortFrom(r.Addr().Unmap(), r.Port()), true
}
