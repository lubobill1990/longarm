package longnet

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"net/netip"

	"github.com/lubobill1990/longarm/internal/relay"
)

// Direct-path packet format (UDP between nodes). Every packet starts with
//
//	"LGN" | type u8 | src id u64 | dst id u64
//
// followed by an end-to-end QUIC packet (typeData) or a disco message.
// Disco messages (ping/pong) are authenticated with HMAC-SHA256 under a key
// exported from the end-to-end QUIC TLS session with that peer, so neither
// the relay nor anyone on the path can forge them, and are bound to the
// (src, dst) pair and direction. Replay protection: keys live only as long
// as their connection; within a key, pings carry a strictly increasing
// sequence number (no wall clocks, so clock skew can't break punching), and
// a pong must match an outstanding ping's txid, once.
const (
	typeData byte = 'D'
	typePing byte = 'P'
	typePong byte = 'O'

	headerLen = 4 + 8 + 8
	macLen    = 16
	txidLen   = 12

	discoExporterLabel = "EXPORTER-longnet-disco"
)

var magic = [3]byte{'L', 'G', 'N'}

func appendHeader(b []byte, typ byte, src, dst relay.ID) []byte {
	b = append(b, magic[0], magic[1], magic[2], typ)
	b = binary.BigEndian.AppendUint64(b, uint64(src))
	return binary.BigEndian.AppendUint64(b, uint64(dst))
}

func parseHeader(b []byte) (typ byte, src, dst relay.ID, rest []byte, ok bool) {
	if len(b) < headerLen || b[0] != magic[0] || b[1] != magic[1] || b[2] != magic[2] {
		return 0, 0, 0, nil, false
	}
	return b[3], relay.ID(binary.BigEndian.Uint64(b[4:])), relay.ID(binary.BigEndian.Uint64(b[12:])), b[headerLen:], true
}

// discoKey authenticates disco messages between two nodes; one per live
// QUIC connection between them.
type discoKey struct {
	id  [8]byte
	key [32]byte
}

func newDiscoKey(material []byte) discoKey {
	var k discoKey
	copy(k.key[:], material)
	h := sha256.Sum256(append([]byte("longnet-disco-key-id\x00"), k.key[:]...))
	copy(k.id[:], h[:8])
	return k
}

type txid [txidLen]byte

// discoMsg is a ping or pong.
type discoMsg struct {
	typ      byte
	src, dst relay.ID
	keyID    [8]byte
	tx       txid
	seq      uint64         // ping: per-key sequence number (0 in pongs)
	observed netip.AddrPort // pong: the ping's source address as seen by the responder
}

func (m discoMsg) seal(k discoKey) []byte {
	b := appendHeader(make([]byte, 0, 96), m.typ, m.src, m.dst)
	b = append(b, k.id[:]...)
	b = append(b, m.tx[:]...)
	b = binary.BigEndian.AppendUint64(b, m.seq)
	if m.typ == typePong {
		ab, _ := m.observed.MarshalBinary()
		b = append(b, byte(len(ab)))
		b = append(b, ab...)
	}
	mac := hmac.New(sha256.New, k.key[:])
	mac.Write(b)
	return mac.Sum(b)[:len(b)+macLen]
}

var errDisco = errors.New("longnet: invalid disco message")

// openDisco verifies and decodes a disco message. keys returns the keys
// known for the sender.
func openDisco(b []byte, keys func(src relay.ID) []discoKey) (discoMsg, error) {
	typ, src, dst, rest, ok := parseHeader(b)
	if !ok || (typ != typePing && typ != typePong) || len(rest) < 8+txidLen+8+macLen {
		return discoMsg{}, errDisco
	}
	m := discoMsg{typ: typ, src: src, dst: dst}
	copy(m.keyID[:], rest)
	var k *discoKey
	for _, c := range keys(src) {
		if c.id == m.keyID {
			k = &c
			break
		}
	}
	if k == nil {
		return discoMsg{}, errDisco
	}
	body, sum := b[:len(b)-macLen], b[len(b)-macLen:]
	mac := hmac.New(sha256.New, k.key[:])
	mac.Write(body)
	if !hmac.Equal(mac.Sum(nil)[:macLen], sum) {
		return discoMsg{}, errDisco
	}
	rest = rest[8 : len(rest)-macLen]
	copy(m.tx[:], rest)
	rest = rest[txidLen:]
	m.seq = binary.BigEndian.Uint64(rest)
	rest = rest[8:]
	if typ == typePong {
		if len(rest) < 1 || len(rest) != 1+int(rest[0]) {
			return discoMsg{}, errDisco
		}
		if err := m.observed.UnmarshalBinary(rest[1:]); err != nil {
			return discoMsg{}, errDisco
		}
	} else if len(rest) != 0 {
		return discoMsg{}, errDisco
	}
	return m, nil
}
