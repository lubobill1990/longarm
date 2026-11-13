package relay

import (
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/netip"
	"testing"
	"time"

	"longarm/internal/pki"
)

// A different identity that maps to an id already in use (a collision, or a
// ground instance string) must not evict the holder.
func TestRegisterRefusesOtherIdentity(t *testing.T) {
	s := &Server{}
	s.init()
	defer s.Close()
	w := pki.Identity{Role: pki.RoleWorker, Name: "w1"}
	holder := s.newSession(w, nil, "tcp", netip.AddrPort{}, Hello{})
	holder.kick = func(string) {}
	if err := s.register(holder); err != nil {
		t.Fatal(err)
	}
	evil := s.newSession(pki.Identity{Role: pki.RoleClient, Name: "mallory"}, nil, "tcp", netip.AddrPort{}, Hello{})
	evil.id = holder.id // pretend the hash collided
	evil.kick = func(string) {}
	if err := s.register(evil); !errors.Is(err, errIDInUse) {
		t.Fatalf("register: %v", err)
	}
	if s.lookup(holder.id) != holder {
		t.Fatal("holder evicted")
	}
	// The same identity reconnecting replaces it.
	again := s.newSession(w, nil, "udp", netip.AddrPort{}, Hello{})
	kicked := false
	holder.kick = func(string) { kicked = true }
	again.kick = func(string) {}
	if err := s.register(again); err != nil || !kicked || s.lookup(holder.id) != again {
		t.Fatalf("reconnect: %v kicked=%v", err, kicked)
	}
}

// CallMeMaybe forwarding: NAT type and candidates pass through; loopback
// candidates only reach peers on the same IP; NodeInfo updates what
// list-less CallMeMaybes carry.
func TestCallMeMaybeForwarding(t *testing.T) {
	s := &Server{}
	s.init()
	defer s.Close()
	mk := func(id pki.Identity, remote string) *session {
		ss := s.newSession(id, nil, "tcp", netip.MustParseAddrPort(remote), Hello{Candidates: []netip.AddrPort{
			netip.MustParseAddrPort("10.0.0.5:7000"), netip.MustParseAddrPort("127.0.0.1:7000")}})
		ss.kick = func(string) {}
		if err := s.register(ss); err != nil {
			t.Fatal(err)
		}
		return ss
	}
	w := mk(pki.Identity{Role: pki.RoleWorker, Name: "w1"}, "203.0.113.7:5000")
	far := mk(pki.Identity{Role: pki.RoleClient, Name: "far"}, "198.51.100.9:6000")
	near := mk(pki.Identity{Role: pki.RoleClient, Name: "near"}, "203.0.113.7:6001")
	recv := func(ss *session) CallMeMaybe {
		t.Helper()
		select {
		case f := <-ss.ctrl:
			if f[0] != FrameCallMeMaybe {
				t.Fatalf("frame %d", f[0])
			}
			var cm CallMeMaybe
			if err := json.Unmarshal(f[3:], &cm); err != nil {
				t.Fatal(err)
			}
			return cm
		case <-time.After(time.Second):
			t.Fatal("no CallMeMaybe")
		}
		return CallMeMaybe{}
	}
	send := func(from, to *session, cm CallMeMaybe) {
		t.Helper()
		cm.Peer = to.id.String()
		p, _ := json.Marshal(cm)
		if err := s.handleCtrl(from, FrameCallMeMaybe, p); err != nil {
			t.Fatal(err)
		}
	}
	send(w, far, CallMeMaybe{NAT: NATHard, Reply: true})
	if cm := recv(far); cm.NAT != NATHard || !cm.Reply || len(cm.Candidates) != 1 || cm.Candidates[0].String() != "10.0.0.5:7000" {
		t.Fatalf("to a peer elsewhere: %+v", cm)
	}
	send(w, near, CallMeMaybe{})
	if cm := recv(near); len(cm.Candidates) != 2 {
		t.Fatalf("to a peer on the same IP: %+v", cm)
	}
	// NodeInfo replaces the Hello candidates and sets the NAT type; bogus
	// NAT types are dropped.
	ni, _ := json.Marshal(NodeInfo{Candidates: []netip.AddrPort{netip.MustParseAddrPort("203.0.113.7:9999")}, NAT: NATEasy})
	if err := s.handleCtrl(w, FrameNodeInfo, ni); err != nil {
		t.Fatal(err)
	}
	send(w, far, CallMeMaybe{})
	if cm := recv(far); cm.NAT != NATEasy || len(cm.Candidates) != 1 || cm.Candidates[0].Port() != 9999 {
		t.Fatalf("after NodeInfo: %+v", cm)
	}
	send(w, far, CallMeMaybe{NAT: "bogus"})
	if cm := recv(far); cm.NAT != NATUnknown {
		t.Fatalf("bogus NAT forwarded: %+v", cm)
	}
	// Unknown frame types are ignored (forward compatibility).
	if err := s.handleCtrl(w, 200, nil); err != nil {
		t.Fatal(err)
	}
	if st := s.Stats(); len(st.Sessions) != 3 || st.Sessions[2].NAT != NATEasy {
		t.Fatalf("stats %+v", st.Sessions)
	}
}

// A certificate revoked between the TLS handshake and the node's Hello is
// refused at registration (Server.Admit), not registered.
func TestRegisterAdmit(t *testing.T) {
	revoked := false
	s := &Server{Admit: func(*x509.Certificate) error {
		if revoked {
			return errors.New("revoked")
		}
		return nil
	}}
	s.init()
	defer s.Close()
	w := pki.Identity{Role: pki.RoleWorker, Name: "w1"}
	revoked = true
	ss := s.newSession(w, nil, "tcp", netip.AddrPort{}, Hello{})
	ss.kick = func(string) {}
	if err := s.register(ss); err == nil || s.lookup(ss.id) != nil {
		t.Fatalf("a revoked certificate registered: %v", err)
	}
	revoked = false
	if err := s.register(ss); err != nil {
		t.Fatal(err)
	}
}

// PeerGone never tells a worker whether a worker it may not reach is
// online: offline and online look the same (silence) unless the two are
// paired. Clients hear PeerGone for every id they can't reach, worker or
// not, so a dial to a missing worker still fails fast.
func TestPeerGonePolicy(t *testing.T) {
	s := &Server{}
	s.init()
	defer s.Close()
	mk := func(id pki.Identity) *session {
		ss := s.newSession(id, nil, "tcp", netip.MustParseAddrPort("198.51.100.1:1"), Hello{})
		ss.kick = func(string) {}
		if err := s.register(ss); err != nil {
			t.Fatal(err)
		}
		return ss
	}
	w := mk(pki.Identity{Role: pki.RoleWorker, Name: "w1"})
	other := mk(pki.Identity{Role: pki.RoleWorker, Name: "other"}) // online, not paired
	c := mk(pki.Identity{Role: pki.RoleClient, Name: "alice"})
	c2 := mk(pki.Identity{Role: pki.RoleClient, Name: "bob"})
	s.SetWorkerPairs([]WorkerPair{{A: "w1", B: "peer"}})
	gone := func(ss *session) bool {
		t.Helper()
		for {
			select {
			case f := <-ss.ctrl:
				if f[0] == FramePeerGone {
					return true
				}
			default:
				return false
			}
		}
	}
	try := func(name string, from *session, dst ID, want bool) {
		t.Helper()
		from.goneAt = map[ID]time.Time{}
		s.forward(from, dst, []byte("x"))
		if got := gone(from); got != want {
			t.Errorf("%s: packet: PeerGone %v, want %v", name, got, want)
		}
		from.goneAt = map[ID]time.Time{}
		p, _ := json.Marshal(CallMeMaybe{Peer: dst.String()})
		if err := s.handleCtrl(from, FrameCallMeMaybe, p); err != nil {
			t.Fatal(err)
		}
		if got := gone(from); got != want {
			t.Errorf("%s: CallMeMaybe: PeerGone %v, want %v", name, got, want)
		}
	}
	try("worker -> paired offline worker", w, WorkerID("peer"), true)
	try("worker -> unpaired offline worker", w, WorkerID("stranger"), false)
	try("worker -> unpaired online worker", w, other.id, false)
	try("worker -> offline client", w, NodeID(pki.Identity{Role: pki.RoleClient, Name: "x"}, "i"), false)
	try("client -> offline worker", c, WorkerID("nobody"), true)
	try("client -> online client", c, c2.id, true) // like an offline one
	try("paired worker -> worker", mk(pki.Identity{Role: pki.RoleWorker, Name: "peer"}), w.id, false)
	if len(w.data) != 1 {
		t.Fatalf("paired packet not forwarded: %d", len(w.data))
	}
}

// NodeInfo and Ping are rate-limited per session: each NodeInfo rescans
// every session (updateSNAT).
func TestCtrlRateLimit(t *testing.T) {
	s := &Server{}
	s.init()
	defer s.Close()
	ss := s.newSession(pki.Identity{Role: pki.RoleClient, Name: "c"}, nil, "tcp", netip.AddrPort{}, Hello{})
	ss.kick = func(string) {}
	if err := s.register(ss); err != nil {
		t.Fatal(err)
	}
	ni, _ := json.Marshal(NodeInfo{NAT: NATEasy})
	for range 100 {
		if err := s.handleCtrl(ss, FrameNodeInfo, ni); err != nil {
			t.Fatal(err)
		}
		if err := s.handleCtrl(ss, FramePing, make([]byte, 8)); err != nil {
			t.Fatal(err)
		}
	}
	if d := s.dropRate.Load(); d < 150 {
		t.Fatalf("dropped %d of 200", d)
	}
	if n := len(ss.ctrl); n == 0 || n > ctrlBurst+2 {
		t.Fatalf("%d pongs", n)
	}
}
