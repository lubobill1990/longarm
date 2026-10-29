package relay

import (
	"encoding/json"
	"net/netip"
	"slices"
	"testing"

	"github.com/lubobill1990/longarm/internal/pki"
)

func TestPublicAddr(t *testing.T) {
	for s, want := range map[string]bool{
		"198.51.100.7": true, "203.0.113.1": true, "192.0.2.1": true, "2001:db8::1": true, // documentation stand-ins
		"8.8.8.8": true, "2606:4700::1": true, "::ffff:8.8.8.8": true,
		"10.0.0.7": false, "172.16.1.1": false, "192.168.1.1": false, "100.64.0.1": false, "100.127.255.254": false,
		"169.254.1.1": false, "127.0.0.1": false, "0.0.0.0": false, "::": false, "::1": false, "fe80::1": false,
		"fd00::1": false, "fc00::1": false, "224.0.0.1": false, "255.255.255.255": false, "198.18.0.1": false,
	} {
		if got := PublicAddr(netip.MustParseAddr(s)); got != want {
			t.Errorf("PublicAddr(%s) = %v", s, got)
		}
	}
	if PublicAddr(netip.Addr{}) {
		t.Error("invalid address public")
	}
	t.Setenv(EnvTestPublic, "127.0.0.0/8, bogus")
	if !PublicAddr(netip.MustParseAddr("127.0.0.1")) || PublicAddr(netip.MustParseAddr("10.0.0.1")) {
		t.Error("test hook")
	}
}

func TestExpandSTUN(t *testing.T) {
	own := []string{":7443", ":7444"}
	for _, tc := range []struct {
		in, want []string
	}{
		{nil, own},
		{[]string{"stun.example.net:3478", " stun2.example.net:3478 "}, []string{"stun.example.net:3478", "stun2.example.net:3478"}},
		{[]string{"self", "stun.example.net:3478", ":7443"}, []string{":7443", ":7444", "stun.example.net:3478"}},
		{[]string{"off"}, []string{}},
		{[]string{"", "NONE"}, []string{}},
	} {
		got := ExpandSTUN(tc.in, own)
		if got == nil || !slices.Equal(got, tc.want) {
			t.Errorf("ExpandSTUN(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSNATVerdict(t *testing.T) {
	ip, ap := netip.MustParseAddr, netip.MustParseAddrPort
	pub := []netip.AddrPort{ap("198.51.100.1:4000")}
	for _, tc := range []struct {
		name        string
		views       []snatView
		snat, known bool
	}{
		{"no sessions", nil, false, false},
		{"private remote, no public mapping (no external STUN)", []snatView{{ip("10.0.0.7"), []netip.AddrPort{ap("10.0.0.7:4000")}}}, false, false},
		{"private remote, public mapping", []snatView{{ip("10.0.0.7"), pub}, {ip("10.0.0.8"), nil}}, true, true},
		{"one node seen at a public address rules it out", []snatView{{ip("10.0.0.7"), pub}, {ip("203.0.113.5"), nil}}, false, true},
		{"public remote", []snatView{{ip("198.51.100.1"), pub}}, false, true},
		{"other family doesn't count", []snatView{{ip("10.0.0.7"), []netip.AddrPort{ap("[2001:db8::1]:4000")}}}, false, false},
		{"loopback test world", []snatView{{ip("127.0.0.1"), []netip.AddrPort{ap("127.0.0.1:4000")}}}, false, false},
	} {
		snat, known := snatVerdict(tc.views)
		if snat != tc.snat || known != tc.known {
			t.Errorf("%s: snat=%v known=%v, want %v %v", tc.name, snat, known, tc.snat, tc.known)
		}
	}
}

// Once SNAT is detected the relay stops advertising its own STUN ports and
// its observed addresses (Welcome.Reflexive, CallMeMaybe candidates).
func TestSNATDetectedEffects(t *testing.T) {
	s := &Server{STUN: []string{":7443", ":7444", "stun.example.net:3478"}}
	s.init()
	defer s.Close()
	ss := s.newSession(pki.Identity{Role: pki.RoleWorker, Name: "w1"}, nil, "udp", netip.AddrPort{}, Hello{})
	var w Welcome
	if json.Unmarshal(s.welcome(ss)[3:], &w); len(w.STUN) != 3 || !w.NodeInfo {
		t.Fatalf("welcome %+v", w)
	}
	s.snat.Store(true)
	w = Welcome{}
	if json.Unmarshal(s.welcome(ss)[3:], &w); !slices.Equal(w.STUN, []string{"stun.example.net:3478"}) || !w.NodeInfo || w.Reflexive.IsValid() {
		t.Fatalf("SNAT welcome %+v", w)
	}
	s.STUN = []string{":7443"}
	if eps := s.stunEndpoints(); len(eps) != 0 {
		t.Fatalf("endpoints %q", eps)
	}
}
