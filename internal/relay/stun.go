package relay

import (
	"encoding/binary"
	"errors"
	"net/netip"
)

// Minimal STUN (RFC 8489) binding requests/responses, used for reflexive
// address discovery and NAT type detection. The relay answers them on its
// QUIC port (STUN packets have the two top bits clear, so they never look
// like QUIC packets) and on an optional second port; nodes may also query
// ordinary public STUN servers.

const (
	stunHeaderLen      = 20
	stunMagicCookie    = 0x2112A442
	stunBindingRequest = 0x0001
	stunBindingSuccess = 0x0101

	stunAttrMappedAddress    = 0x0001
	stunAttrXORMappedAddress = 0x0020
	stunAttrXORMappedOld     = 0x8020 // pre-RFC 5389 servers
)

// STUNTxID is a STUN transaction id.
type STUNTxID [12]byte

// IsSTUN reports whether b looks like a STUN message (top two bits of the
// first byte clear, magic cookie present).
func IsSTUN(b []byte) bool {
	return len(b) >= stunHeaderLen && b[0]&0xC0 == 0 && binary.BigEndian.Uint32(b[4:8]) == stunMagicCookie
}

// STUNRequest builds a binding request without attributes.
func STUNRequest(tx STUNTxID) []byte {
	b := make([]byte, stunHeaderLen)
	binary.BigEndian.PutUint16(b[0:2], stunBindingRequest)
	binary.BigEndian.PutUint32(b[4:8], stunMagicCookie)
	copy(b[8:20], tx[:])
	return b
}

func isSTUNBindingRequest(b []byte) bool {
	return IsSTUN(b) && binary.BigEndian.Uint16(b[0:2]) == stunBindingRequest &&
		int(binary.BigEndian.Uint16(b[2:4])) == len(b)-stunHeaderLen
}

// stunResponse answers a binding request from addr with XOR-MAPPED-ADDRESS.
func stunResponse(req []byte, addr netip.AddrPort) []byte {
	ip := addr.Addr().Unmap()
	family, alen := byte(1), 4
	if ip.Is6() {
		family, alen = 2, 16
	}
	b := make([]byte, stunHeaderLen, stunHeaderLen+8+alen)
	binary.BigEndian.PutUint16(b[0:2], stunBindingSuccess)
	binary.BigEndian.PutUint16(b[2:4], uint16(4+4+alen))
	copy(b[4:20], req[4:20]) // cookie + transaction id
	b = binary.BigEndian.AppendUint16(b, stunAttrXORMappedAddress)
	b = binary.BigEndian.AppendUint16(b, uint16(4+alen))
	b = append(b, 0, family)
	b = binary.BigEndian.AppendUint16(b, addr.Port()^uint16(stunMagicCookie>>16))
	raw := ip.AsSlice()
	for i := range raw {
		raw[i] ^= b[4+i] // cookie, then transaction id
	}
	return append(b, raw...)
}

var errSTUN = errors.New("stun: not a binding success response")

// ParseSTUNResponse decodes a binding success response: its transaction id
// and the mapped address (XOR-MAPPED-ADDRESS preferred).
func ParseSTUNResponse(b []byte) (STUNTxID, netip.AddrPort, error) {
	var tx STUNTxID
	if !IsSTUN(b) || binary.BigEndian.Uint16(b[0:2]) != stunBindingSuccess {
		return tx, netip.AddrPort{}, errSTUN
	}
	n := int(binary.BigEndian.Uint16(b[2:4]))
	if n%4 != 0 || stunHeaderLen+n > len(b) {
		return tx, netip.AddrPort{}, errSTUN
	}
	copy(tx[:], b[8:20])
	var mapped, xored netip.AddrPort
	for a := b[stunHeaderLen : stunHeaderLen+n]; len(a) >= 4; {
		typ, l := binary.BigEndian.Uint16(a[0:2]), int(binary.BigEndian.Uint16(a[2:4]))
		if 4+l > len(a) {
			return tx, netip.AddrPort{}, errSTUN
		}
		v := a[4 : 4+l]
		switch typ {
		case stunAttrMappedAddress:
			mapped = parseSTUNAddr(v, nil)
		case stunAttrXORMappedAddress, stunAttrXORMappedOld:
			xored = parseSTUNAddr(v, b[4:20])
		}
		a = a[min(len(a), 4+(l+3)&^3):]
	}
	if xored.IsValid() {
		return tx, xored, nil
	}
	if mapped.IsValid() {
		return tx, mapped, nil
	}
	return tx, netip.AddrPort{}, errSTUN
}

// parseSTUNAddr decodes a (XOR-)MAPPED-ADDRESS value; xor is the cookie +
// transaction id for XOR attributes.
func parseSTUNAddr(v, xor []byte) netip.AddrPort {
	if len(v) < 4 {
		return netip.AddrPort{}
	}
	alen := map[byte]int{1: 4, 2: 16}[v[1]]
	if alen == 0 || len(v) < 4+alen {
		return netip.AddrPort{}
	}
	port := binary.BigEndian.Uint16(v[2:4])
	raw := append([]byte(nil), v[4:4+alen]...)
	if xor != nil {
		port ^= uint16(stunMagicCookie >> 16)
		for i := range raw {
			raw[i] ^= xor[i]
		}
	}
	ip, _ := netip.AddrFromSlice(raw)
	return netip.AddrPortFrom(ip.Unmap(), port)
}
