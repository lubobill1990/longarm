package relay

import (
	"encoding/json"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/pki"
)

// A different identity that maps to an id already in use (a collision, or a
// ground instance string) must not evict the holder.
func TestRegisterRefusesOtherIdentity(t *testing.T) {
	s := &Server{}
	s.init()
	defer s.Close()
	w := pki.Identity{Role: pki.RoleWorker, Name: "w1"}
	holder := s.newSession(w, "tcp", netip.AddrPort{}, Hello{})
	holder.kick = func(string) {}
	if err := s.register(holder); err != nil {
		t.Fatal(err)
	}
	evil := s.newSession(pki.Identity{Role: pki.RoleClient, Name: "mallory"}, "tcp", netip.AddrPort{}, Hello{})
	evil.id = holder.id // pretend the hash collided
	evil.kick = func(string) {}
	if err := s.register(evil); !errors.Is(err, errIDInUse) {
		t.Fatalf("register: %v", err)
	}
	if s.lookup(holder.id) != holder {
		t.Fatal("holder evicted")
	}
	// The same identity reconnecting replaces it.
	again := s.newSession(w, "udp", netip.AddrPort{}, Hello{})
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
		ss := s.newSession(id, "tcp", netip.MustParseAddrPort(remote), Hello{Candidates: []netip.AddrPort{
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
