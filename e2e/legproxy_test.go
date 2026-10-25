package e2e

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"tailscale.com/derp/derpserver"
	"tailscale.com/net/stun/stuntest"
	"tailscale.com/tailcfg"
	"tailscale.com/tstest/integration/testcontrol"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
	"tailscale.com/types/nettype"
)

// startTestTailnet runs an in-process tailnet: Tailscale's test control
// server (any node may join, no auth) with one DERP + STUN server on
// 127.0.0.1 (as tailscale.com/tstest/integration.RunDERPAndSTUN, whose
// package would pull dozens of modules into go.mod). It returns the
// control URL.
func startTestTailnet(t *testing.T) string {
	t.Helper()
	d := derpserver.New(key.NewNode(), logger.Discard)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	derpSrv := httptest.NewUnstartedServer(derpserver.Handler(d))
	derpSrv.Listener.Close()
	derpSrv.Listener = ln
	derpSrv.Config.ErrorLog = logger.StdLogger(logger.Discard)
	derpSrv.Config.TLSNextProto = make(map[string]func(*http.Server, *tls.Conn, http.Handler))
	derpSrv.StartTLS()
	stunAddr, stunCleanup := stuntest.ServeWithPacketListener(t, nettype.Std{})
	derpMap := &tailcfg.DERPMap{Regions: map[tailcfg.DERPRegionID]*tailcfg.DERPRegion{
		1: {RegionID: 1, RegionCode: "test", Nodes: []*tailcfg.DERPNode{{
			Name: "t1", RegionID: 1, HostName: "127.0.0.1", IPv4: "127.0.0.1", IPv6: "none",
			STUNPort: stunAddr.Port, DERPPort: ln.Addr().(*net.TCPAddr).Port, InsecureForTests: true, STUNTestIP: "127.0.0.1",
		}}},
	}}
	control := &testcontrol.Server{DERPMap: derpMap, DNSConfig: &tailcfg.DNSConfig{Proxied: true}, MagicDNSDomain: "tail-scale.ts.net"}
	control.HTTPTestServer = httptest.NewUnstartedServer(control)
	control.HTTPTestServer.Start()
	t.Cleanup(func() {
		control.HTTPTestServer.Close()
		derpSrv.CloseClientConnections()
		derpSrv.Close()
		d.Close()
		stunCleanup()
	})
	return control.HTTPTestServer.URL
}

// TestDaemonLegProxy: one-shot longarm dials the tailscale and longnet legs through
// a running longarm daemon, whose nodes are already up, instead of starting its
// own tsnet node for every command. A real tailnet runs in-process (test
// control + DERP); the worker listens on tcp, tailscale and longnet.
//   - Without a daemon, `longarm --transport tailscale info` doesn't wait for the
//     cold tsnet node: tcp wins at once and longarm exits without waiting for the
//     node's shutdown.
//   - With the daemon, the tailscale leg wins through it, under 1 s per
//     command (budget); longnet goes through it too.
//   - A stale daemon address (daemon gone) falls back to the own nodes.
func TestDaemonLegProxy(t *testing.T) {
	if testing.Short() {
		t.Skip("slow; skipped with -short")
	}
	t.Parallel()
	controlURL := startTestTailnet(t)
	h := newHubEnv(t, "")
	tsEnv := []string{"TS_AUTHKEY=tskey-auth-e2e-unused"} // the test control lets anyone in; the key only marks tsnet as configured

	wa := h.newWorkerDir(t, "wa", "")
	os.WriteFile(filepath.Join(wa.dataDir, "longarmd.toml"), []byte(fmt.Sprintf(
		"listen = [\"tcp://127.0.0.1:%d\", \"tailscale://longarm-e2e-wa:7701?control=%s\", \"longnet://%s?name=wa\"]\n",
		wa.port, controlURL, h.addr())), 0o600)
	wa.env = tsEnv
	h.joinWorker(t, wa, true)
	wa.start(t, true)
	h.waitNode(t, "wa", "online with a tailscale endpoint", func(n map[string]any) bool {
		return isTrue(n["online"]) && strings.Contains(fmt.Sprint(n["endpoints"]), "tailscale://100.")
	})
	// The client's embedded node joins the same tailnet.
	os.WriteFile(filepath.Join(h.client, "workers.toml"), []byte(fmt.Sprintf(
		"[tailscale]\ncontrol_url = %q\nhostname = \"longarm-e2e-bob\"\n", controlURL)), 0o600)
	longarm := func(args ...string) (result, time.Duration) {
		t.Helper()
		start := time.Now()
		r := h.longarmWith(tsEnv, h.client, args...)
		took := time.Since(start)
		if r.code != 0 {
			t.Fatalf("longarm %v: %+v", args, r)
		}
		return r, took
	}
	path := func(r result) string {
		m := regexp.MustCompile(`(?m)^path: (.*)$`).FindStringSubmatch(r.stdout)
		if m == nil {
			t.Fatalf("no path in:\n%s", r.stdout)
		}
		return m[1]
	}
	legs := func(r result) string {
		m := regexp.MustCompile(`(?m)^legs: (.*)$`).FindStringSubmatch(r.stdout)
		if m == nil {
			return ""
		}
		return m[1]
	}

	// 1. No daemon: the cold tsnet node of this one-shot longarm gets no head
	// start (another leg wins right away: tcp, or longnet when it happens to
	// be quicker), and its shutdown doesn't hold up the exit.
	var cold []time.Duration
	for range 2 {
		r, took := longarm("--transport", "tailscale", "info", "wa")
		if strings.HasPrefix(path(r), "tailscale/") {
			t.Fatalf("without a daemon, path %s (legs %s)", path(r), legs(r))
		}
		cold = append(cold, took)
		t.Logf("no daemon: longarm --transport tailscale info: %s, legs: %s", took.Round(time.Millisecond), legs(r))
	}
	if slices.Min(cold) > 3*time.Second {
		t.Fatalf("one-shot longarm without a daemon waits for its tsnet node: %v", cold)
	}

	// 2. longarm daemon: its nodes come up once; one-shot longarm dials through them.
	if r := h.longarmWith(tsEnv, h.client, "daemon", "start"); r.code != 0 {
		t.Fatalf("longarm daemon start: %+v", r)
	}
	t.Cleanup(func() { h.longarmWith(nil, h.client, "daemon", "stop") })
	waitFor(t, "tailscale leg through the daemon", 90*time.Second, func() bool {
		r := h.longarmWith(tsEnv, h.client, "--transport", "tailscale", "info", "wa")
		return r.code == 0 && strings.HasPrefix(path(r), "tailscale/") && strings.Contains(path(r), "via longarm daemon")
	})
	var warm []time.Duration
	for range 5 {
		r, took := longarm("--transport", "tailscale", "info", "wa")
		p := path(r)
		if !strings.HasPrefix(p, "tailscale/") || !strings.Contains(p, "via longarm daemon") {
			t.Fatalf("with the daemon, path %s (legs %s)", p, legs(r))
		}
		// The leg's own dial time (a cold node of this process takes
		// seconds; the daemon's is up): the whole longarm process' wall time
		// mostly measured process start-up on a loaded CI VM (1.5-6 s with
		// a 35-470 ms leg).
		m := legWonRE.FindStringSubmatch(legs(r))
		if m == nil {
			t.Fatalf("no tailscale time in legs %q", legs(r))
		}
		d, err := time.ParseDuration(m[1])
		if err != nil {
			t.Fatalf("legs %q: %v", legs(r), err)
		}
		warm = append(warm, d)
		t.Logf("daemon: longarm --transport tailscale info: %s, path: %s, legs: %s", took.Round(time.Millisecond), p, legs(r))
	}
	slices.Sort(warm)
	if med := warm[len(warm)/2]; med > time.Second {
		t.Fatalf("the tailscale leg through the daemon takes %s (median of %v), budget 1s", med, warm)
	}
	// Other commands use it as well (exec over the proxied tailscale leg).
	if r := h.longarmWith(tsEnv, h.client, "--transport", "tailscale", "exec", "wa", "--", bins.helper, "print", "1", "0", "7"); r.code != 7 {
		t.Fatalf("exec over the proxied tailscale leg: %+v", r)
	}
	// longnet through the daemon's node.
	waitFor(t, "longnet leg through the daemon", 60*time.Second, func() bool {
		r := h.longarmWith(tsEnv, h.client, "--transport", "longnet", "info", "wa")
		return r.code == 0 && strings.HasPrefix(path(r), "longnet/") && strings.Contains(path(r), "via longarm daemon")
	})

	// 3. The daemon is gone but its address file stays (a crash): longarm falls
	// back to its own nodes / the other legs.
	addrFile := filepath.Join(h.client, "daemon.addr")
	addr, _ := os.ReadFile(addrFile)
	h.longarmWith(nil, h.client, "daemon", "stop")
	waitFor(t, "daemon stopped", 20*time.Second, func() bool { _, err := os.Stat(addrFile); return err != nil })
	os.WriteFile(addrFile, addr, 0o600)
	r, took := longarm("--transport", "longnet", "info", "wa")
	if p := path(r); strings.Contains(p, "via longarm daemon") {
		t.Fatalf("path %s without a daemon", p)
	}
	t.Logf("stale daemon address: longarm --transport longnet info: %s, path %s", took.Round(time.Millisecond), path(r))
	os.Remove(addrFile)
}

// legWonRE finds the winning tailscale leg's time in longarm info's legs line.
var legWonRE = regexp.MustCompile(`tailscale (\S+) won`)
