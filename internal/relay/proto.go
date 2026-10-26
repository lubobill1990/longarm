// Package relay is the longnet relay + rendezvous server (DESIGN §4.3) and the
// node-side client of it.
//
// Every longnet node (longarmd worker or longarm client process) keeps one authenticated
// control session to a relay: QUIC over UDP (control frames on one stream,
// packets as QUIC datagrams), or, when UDP is blocked, TLS over TCP with
// everything framed on the byte stream (the DERP approach). Both sides
// authenticate with Longarm certificates from the same CA.
//
// The relay
//   - forwards opaque packets between sessions, keyed by destination node id
//     (the payload is end-to-end QUIC; the relay never sees plaintext);
//   - tells each node its reflexive address (STUN-like: the source address it
//     sees for the node's UDP session);
//   - does rendezvous: it forwards CallMeMaybe candidate lists between peers,
//     adding the sender's reflexive address.
//
// The server is a library type (Server) so longarm-hub can embed it in P1;
// cmd/longarm-relay is a thin standalone wrapper.
package relay

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"

	"github.com/lubobill1990/longarm/internal/pki"
)

// ALPN of the relay protocol (QUIC and TLS/TCP).
const ALPN = "longnet-relay/1"

// ID is a node's routing id. Workers have a fixed id derived from their name
// (so clients can address them without a lookup); clients get one per
// process, derived from their identity and a random instance string. Ids are
// computed by the relay from the authenticated certificate, so a node cannot
// claim another node's id.
type ID uint64

func (id ID) String() string {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(id))
	return hex.EncodeToString(b[:])
}

// ParseID parses the hex form produced by String.
func ParseID(s string) (ID, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 8 {
		return 0, fmt.Errorf("bad node id %q", s)
	}
	return ID(binary.BigEndian.Uint64(b)), nil
}

// WorkerID is the id of the worker with the given certificate name.
func WorkerID(name string) ID { return deriveID(string(pki.RoleWorker)+"/"+name, "") }

// NodeID is the id of a session with the given identity and instance
// (instance is ignored for workers).
func NodeID(id pki.Identity, instance string) ID {
	if id.Role == pki.RoleWorker {
		return WorkerID(id.Name)
	}
	return deriveID(id.String(), instance)
}

func deriveID(ident, instance string) ID {
	h := sha256.Sum256([]byte("longnet-id\x00" + ident + "\x00" + instance))
	return ID(binary.BigEndian.Uint64(h[:8]))
}

// Frame types. A frame on a stream is: type u8 | length u16 BE | payload.
const (
	FrameHello       byte = 1 // node → relay, JSON Hello (first frame)
	FrameWelcome     byte = 2 // relay → node, JSON Welcome
	FrameData        byte = 3 // both, binary: id u64 | packet (dst on send, src on receive); TCP only
	FrameCallMeMaybe byte = 4 // both, JSON CallMeMaybe
	FramePing        byte = 5 // both, 8-byte nonce (TCP keepalive / relay RTT)
	FramePong        byte = 6 // both, echoes the ping nonce
	FramePeerGone    byte = 7 // relay → node, id u64: destination not connected
	FrameClose       byte = 8 // relay → node, UTF-8 reason, before closing (e.g. "replaced")
	// FrameNodeInfo (node → relay, JSON NodeInfo) updates the node's
	// candidates and NAT type. Nodes only send it to relays that advertise
	// STUN in Welcome (older relays treat unknown frames as errors).
	FrameNodeInfo byte = 9
)

// NAT types reported by nodes (see longnet's NAT detection).
const (
	NATUnknown = ""     // not enough observations
	NATOpen    = "open" // no NAT: the reflexive address is a local one
	NATEasy    = "easy" // endpoint-independent mapping (same public addr for every destination)
	NATHard    = "hard" // mapping varies by destination (symmetric NAT)
)

// MaxFramePayload bounds any frame; MaxPacket bounds relayed packets (the
// end-to-end QUIC uses 1200-byte packets, with headroom).
const (
	MaxFramePayload = 1<<16 - 1
	MaxPacket       = 1452
)

// Hello is the first message of a session.
type Hello struct {
	Version    int              `json:"v"`
	Instance   string           `json:"instance,omitempty"`   // clients: random per process
	Candidates []netip.AddrPort `json:"candidates,omitempty"` // LAN addresses of the node's UDP socket
}

// Welcome answers Hello.
type Welcome struct {
	ID        string         `json:"id"`                  // the node's id
	Reflexive netip.AddrPort `json:"reflexive,omitzero"`  // source address seen by the relay (UDP sessions)
	Transport string         `json:"transport,omitempty"` // "udp" | "tcp"
	// STUN lists the relay's STUN endpoints: ":port" means the relay's own
	// IP (the address the node dialed), "host:port" an explicit server.
	// Non-empty also means the relay understands FrameNodeInfo.
	STUN []string `json:"stun,omitempty"`
	// NodeInfo: the relay understands FrameNodeInfo even with an empty STUN
	// list (a relay that stopped advertising its own SNATed STUN ports and
	// has no external servers configured).
	NodeInfo bool `json:"node_info,omitempty"`
}

// CallMeMaybe carries candidate addresses for hole punching. Sent by a node
// with Peer = destination; delivered with Peer = source. The relay adds the
// sender's reflexive address (also in Reflexive) and drops loopback
// candidates unless both nodes connect from the same IP.
type CallMeMaybe struct {
	Peer       string           `json:"peer"`
	Candidates []netip.AddrPort `json:"candidates,omitempty"`
	Reply      bool             `json:"reply,omitempty"` // receiver should answer with its own candidates
	NAT        string           `json:"nat,omitempty"`   // sender's NAT type (NAT* constants)
	Reflexive  netip.AddrPort   `json:"reflexive,omitzero"`
}

// NodeInfo is what a node tells the relay about itself after STUN (shown
// in /sessions; candidates are used for CallMeMaybes without a list).
type NodeInfo struct {
	Candidates []netip.AddrPort `json:"candidates,omitempty"`
	NAT        string           `json:"nat,omitempty"`
	Mapped     []netip.AddrPort `json:"mapped,omitempty"` // distinct STUN-observed addresses
}

// ErrFrameTooLarge is returned for frames exceeding MaxFramePayload.
var ErrFrameTooLarge = errors.New("relay: frame too large")

// AppendFrame appends an encoded frame to b.
func AppendFrame(b []byte, typ byte, payload []byte) ([]byte, error) {
	if len(payload) > MaxFramePayload {
		return b, ErrFrameTooLarge
	}
	b = append(b, typ, byte(len(payload)>>8), byte(len(payload)))
	return append(b, payload...), nil
}

// ReadFrame reads one frame. buf is reused for the payload when big enough;
// the returned payload aliases it.
func ReadFrame(r io.Reader, buf []byte) (typ byte, payload []byte, err error) {
	var h [3]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := int(h[1])<<8 | int(h[2])
	if cap(buf) < n {
		buf = make([]byte, n)
	}
	buf = buf[:n]
	if _, err := io.ReadFull(r, buf); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	return h[0], buf, nil
}

// AppendPacket appends the id-prefixed packet encoding used by FrameData and
// QUIC datagrams.
func AppendPacket(b []byte, id ID, pkt []byte) []byte {
	b = binary.BigEndian.AppendUint64(b, uint64(id))
	return append(b, pkt...)
}

// ParsePacket splits an id-prefixed packet.
func ParsePacket(b []byte) (ID, []byte, error) {
	if len(b) < 8 {
		return 0, nil, errors.New("relay: short packet")
	}
	if len(b)-8 > MaxPacket {
		return 0, nil, errors.New("relay: packet too large")
	}
	return ID(binary.BigEndian.Uint64(b)), b[8:], nil
}

func encodeID(id ID) []byte { return binary.BigEndian.AppendUint64(nil, uint64(id)) }

func decodeID(b []byte) (ID, error) {
	if len(b) != 8 {
		return 0, errors.New("relay: bad id payload")
	}
	return ID(binary.BigEndian.Uint64(b)), nil
}

func jsonFrame(typ byte, v any) ([]byte, error) {
	p, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return AppendFrame(nil, typ, p)
}

// maxCandidates bounds candidate lists (the relay drops the excess).
const maxCandidates = 16

// ValidNAT reports whether s is a known NAT type.
func ValidNAT(s string) bool {
	return s == NATUnknown || s == NATOpen || s == NATEasy || s == NATHard
}

func cleanCandidates(in []netip.AddrPort, extra ...netip.AddrPort) []netip.AddrPort {
	seen := map[netip.AddrPort]bool{}
	var out []netip.AddrPort
	for _, a := range append(append([]netip.AddrPort{}, extra...), in...) {
		a = netip.AddrPortFrom(a.Addr().Unmap(), a.Port())
		if !a.IsValid() || a.Port() == 0 || a.Addr().IsUnspecified() || a.Addr().IsMulticast() ||
			a.Addr() == netip.AddrFrom4([4]byte{255, 255, 255, 255}) || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
		if len(out) == maxCandidates {
			break
		}
	}
	return out
}
