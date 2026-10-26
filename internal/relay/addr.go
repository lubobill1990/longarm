package relay

import (
	"net/netip"
	"os"
	"slices"
	"strings"
)

// Public vs. private addresses, for telling usable STUN observations from
// server-side SNAT. Some hosting platforms (e.g. Azure Container Instances)
// rewrite the source address of every inbound UDP packet to an internal
// one: a STUN server (or relay) running there answers every client with a
// private "mapped" address that means nothing to anybody else. See
// longnet's NAT discovery (internal/transport/longnet/nat.go) and the
// relay's SNAT detection (snatVerdict).

// ExpandSTUN returns the STUN endpoint list to advertise (Server.STUN):
// the entries of list, where "self" stands for own (the relay's ":port"
// endpoints) and "off"/"none" for nothing. An empty list means own.
// Blank entries are dropped, duplicates removed. The result is never nil
// (an empty list advertises no STUN server).
func ExpandSTUN(list, own []string) []string {
	if len(list) == 0 {
		list = []string{"self"}
	}
	out := []string{}
	add := func(s string) {
		if s = strings.TrimSpace(s); s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	for _, s := range list {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "self":
			for _, o := range own {
				add(o)
			}
		case "off", "none":
		default:
			add(s)
		}
	}
	return out
}

// STUNKeyword reports whether s is one of ExpandSTUN's keywords.
func STUNKeyword(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "self", "off", "none":
		return true
	}
	return false
}

// nonPublic lists IPv4/IPv6 ranges that are never an Internet-reachable
// host address, besides what netip classifies (loopback, link-local,
// multicast, unspecified, RFC 1918 and fc00::/7 via IsPrivate).
var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),      // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),  // shared address space (carrier-grade NAT, tailnets)
	netip.MustParsePrefix("192.0.0.0/24"),   // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"),  // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),    // reserved, broadcast
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use NAT64
	netip.MustParsePrefix("fec0::/10"),      // deprecated site-local
	netip.MustParsePrefix("100::/64"),       // discard-only
	netip.MustParsePrefix("2001:10::/28"),   // ORCHID
}

// documentation ranges count as public: tests and examples use them as
// stand-ins for real public addresses (RFC 5737, RFC 3849).
var documentation = []netip.Prefix{
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("2001:db8::/32"),
}

// EnvTestPublic is a test hook: a comma-separated list of prefixes (e.g.
// "127.0.0.0/8") that PublicAddr treats as public. The end-to-end tests run
// every process on loopback; this lets them play "relay on the Internet,
// SNAT inside the hosting platform" without real public addresses. It only
// ever widens what counts as public, so a stray setting can at worst keep
// a useless STUN answer (the behaviour before SNAT filtering existed).
const EnvTestPublic = "LONGARM_NET_TEST_PUBLIC"

// PublicAddr reports whether ip is a globally routable unicast address: not
// loopback, link-local, multicast, unspecified, RFC 1918, carrier-grade NAT
// (100.64.0.0/10), ULA (fc00::/7) or another special-purpose range.
// Documentation ranges (192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24,
// 2001:db8::/32) count as public, and so do the prefixes in
// $LONGARM_NET_TEST_PUBLIC (a test hook).
func PublicAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() {
		return false
	}
	if v := os.Getenv(EnvTestPublic); v != "" {
		for _, s := range strings.Split(v, ",") {
			if p, err := netip.ParsePrefix(strings.TrimSpace(s)); err == nil && p.Contains(ip) {
				return true
			}
		}
	}
	for _, p := range documentation {
		if p.Contains(ip) {
			return true
		}
	}
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}
