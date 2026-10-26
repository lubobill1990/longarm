package e2e

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/relay"
)

// A relay behind its hosting platform's source NAT (like a hub in Azure
// Container Instances): its STUN answers and observed session addresses are
// the platform's private address (here faked with the relay's test hook),
// so only an external STUN server — advertised by the relay — tells nodes
// their real address. Everything runs on loopback; LONGARM_NET_TEST_PUBLIC
// makes 127.0.0.0/8 count as "public" (the relay "on the Internet", the
// external STUN server too), while the fake SNAT address 10.0.0.7 stays
// private.
//
// Expected: nodes ignore the relay's private answers (they are no
// candidates and don't make the NAT type "hard"), the relay notices the SNAT
// (stun_snat in /sessions), stops advertising itself as a STUN server and
// stops passing its observed addresses on as candidates, and the two nodes
// still go direct.
func TestLongNetSNATedRelaySTUN(t *testing.T) {
	// The external STUN server (no fake SNAT: it answers with the real
	// source address).
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ext := &relay.Server{}
	go ext.ServeSTUN(pc)
	t.Cleanup(func() { ext.Close() })
	extAddr := pc.LocalAddr().String()

	const fakeSNAT = "10.0.0.7"
	public := relay.EnvTestPublic + "=127.0.0.0/8"
	re := newLongNetEnvWith(t, []string{"--stun-advertise", "self," + extAddr},
		[]string{relay.EnvTestFakeSNAT + "=" + fakeSNAT, public})
	re.startLongarmdLongNet(public)
	e := re.env

	// The worker: only the external server's answer is a mapped address,
	// and the relay concludes that it is SNATed.
	waitFor(t, "worker mapped address from the external STUN server, relay SNAT detection", 20*time.Second, func() bool {
		st := re.relayStats()
		for _, s := range st.Sessions {
			if s.Node != "worker/w1" {
				continue
			}
			for _, m := range s.Mapped {
				if strings.HasPrefix(m, fakeSNAT+":") {
					t.Fatalf("SNATed STUN answer used as mapped address: %+v", s)
				}
			}
			return len(s.Mapped) == 1 && strings.HasPrefix(s.Mapped[0], "127.0.0.1:") && s.NAT != "hard" && st.STUNSNAT
		}
		return false
	})
	if b, _ := os.ReadFile(filepath.Join(e.root, "relay.out")); !strings.Contains(string(b), "look source-NATed") {
		t.Errorf("relay didn't log the SNAT detection:\n%s", b)
	}

	// A client connecting now: the relay's Welcome lists only the external
	// server (no SNATed answers to ignore), and the session goes direct.
	clientEnv := []string{public, "LONGARM_NET_LOG=1"}
	r := e.longarmWith(clientEnv, e.client, "info", "w1-net")
	if r.code != 0 {
		t.Fatalf("info: %+v", r)
	}
	if strings.Contains(r.stderr, "ignoring STUN answers") || !strings.Contains(r.stderr, "longnet: NAT") {
		t.Fatalf("client still queried the SNATed relay (or never STUNed):\n%s", r.stderr)
	}
	for _, line := range strings.Split(r.stderr, "\n") {
		if strings.Contains(line, "longnet: NAT") && (strings.Contains(line, fakeSNAT) || strings.Contains(line, "type=hard")) {
			t.Fatalf("client NAT state uses the SNATed address: %s", line)
		}
	}

	before := re.relayStats().BytesRelayed
	r = e.longarmWith(clientEnv, e.client, "exec", "w1-net", "--", bins.helper, "print", "50000", "0", "0", "1000")
	if r.code != 0 || r.stdout != lines(1, 50000) {
		t.Fatalf("bulk exec: exit %d, %d bytes (%s)", r.code, len(r.stdout), r.stderr)
	}
	relayed := re.relayStats().BytesRelayed - before
	t.Logf("bulk exec: %d bytes of output, %d bytes through the relay", len(r.stdout), relayed)
	if relayed > uint64(len(r.stdout))/2 {
		t.Errorf("direct path expected, but %d of %d bytes were relayed\n%s", relayed, len(r.stdout), r.stderr)
	}
	if !strings.Contains(r.stderr, "direct path up") {
		t.Errorf("no direct path:\n%s", r.stderr)
	}
	// The relay-observed (SNAT) address is never a candidate.
	for _, s := range re.relayStats().Sessions {
		for _, c := range s.Candidates {
			if strings.HasPrefix(c, fakeSNAT+":") {
				t.Errorf("%s advertises the SNAT address: %v", s.Node, s.Candidates)
			}
		}
	}
}
