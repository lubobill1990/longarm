package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// extRelay is a longarm-relay registered with a hub (--hub): an external
// relay of the hub's relay set.
type extRelay struct {
	t                 *testing.T
	h                 *hubEnv
	udpPort, tcpPort  int
	tlsDir, stateDir  string
	logPath           string
	cmd               *exec.Cmd
	exited            chan struct{}
	udp, tcp, metrics string
}

// startExtRelay issues a relay certificate from the hub's CA (longarm-hub
// issue --role relay, the path deploy/azure uses) and starts longarm-relay
// with UDP and its TLS fallback on different ports (like on Azure Container
// Instances), registered with the hub.
func startExtRelay(t *testing.T, h *hubEnv, name string) *extRelay {
	t.Helper()
	x := &extRelay{t: t, h: h, udpPort: freePort(t), tcpPort: freePort(t),
		tlsDir: filepath.Join(h.root, name+"-tls"), stateDir: filepath.Join(h.root, name+"-state"),
		logPath: filepath.Join(h.root, name+".log")}
	h.hubCmd(t, "issue", "--role", "relay", "--name", name, "--host", "127.0.0.1", "--out", x.tlsDir)
	x.start()
	t.Cleanup(func() {
		x.stop()
		if t.Failed() {
			b, _ := os.ReadFile(x.logPath)
			t.Logf("longarm-relay %s log:\n%s", name, tail(string(b), 8000))
		}
	})
	return x
}

func (x *extRelay) start() {
	t := x.t
	t.Helper()
	logf, err := os.OpenFile(x.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(buildRelay(t), "--udp", fmt.Sprintf("127.0.0.1:%d", x.udpPort), "--tcp", fmt.Sprintf("127.0.0.1:%d", x.tcpPort),
		"--stun2", "", "--tls-dir", x.tlsDir, "--state-dir", x.stateDir, "--metrics", "127.0.0.1:0", "--log-interval", "0", "--rate", "0",
		"--hub", x.h.addr(), "--advertise", "127.0.0.1")
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = logf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	logf.Close()
	x.cmd, x.exited = cmd, make(chan struct{})
	line := make(chan []string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if m := relayLineRE.FindStringSubmatch(sc.Text()); m != nil {
				line <- m
			}
		}
		io.Copy(io.Discard, stdout)
	}()
	go func(c chan struct{}) { cmd.Wait(); close(c) }(x.exited)
	select {
	case m := <-line:
		x.udp, x.tcp, x.metrics = m[1], m[2], m[3]
	case <-x.exited:
		b, _ := os.ReadFile(x.logPath)
		t.Fatalf("longarm-relay exited:\n%s", b)
	case <-time.After(60 * time.Second):
		t.Fatal("longarm-relay did not start")
	}
}

func (x *extRelay) stop() {
	if x.cmd != nil && x.cmd.Process != nil {
		x.cmd.Process.Kill()
		<-x.exited
		x.cmd = nil
	}
}

// health is the relay's /healthz.
func (x *extRelay) health() map[string]any {
	resp, err := http.Get("http://" + x.metrics + "/healthz")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	return m
}

// TestExternalRelay: a longarm-relay registered with the hub (--hub) joins
// the hub's relay set. A loopback-only worker (every sign-in install)
// listens on it and on the hub's embedded relay, advertising both; a worker
// that doesn't know the relay set (an older version, simulated) keeps the
// embedded relay only, and both stay online. Then, with relayed paths only
// (direct paths off) and an end-to-end idle timeout shorter than the hub's
// downtime:
//   - the hub restarts: a log stream over the external relay goes on without
//     a break and new commands work while the hub is down; the same stream
//     over the embedded relay breaks and resumes (reconnect) after the hub is
//     back;
//   - the relay restarts while the hub is down: it starts from its cached
//     policy and relays again;
//   - a revocation reaches the relay through the hub.
func TestExternalRelay(t *testing.T) {
	t.Parallel()
	h := newHubEnv(t, "")
	os.Remove(filepath.Join(h.client, "workers.toml"))
	x := startExtRelay(t, h, "xrelay")
	waitFor(t, "the relay registered with the hub", 30*time.Second, func() bool {
		hs, _ := x.health()["hub"].(map[string]any)
		return isTrue(hs["connected"]) && isTrue(hs["ready"])
	})

	netEnv := []string{"LONGARM_NET_DISABLE_DIRECT=1", "LONGARM_NET_IDLE_TIMEOUT=4s"}
	wa := h.newWorkerDir(t, "wa", "") // loopback tcp only: gets the hub relay legs
	wa.env = netEnv
	h.joinWorker(t, wa, true)
	wa.start(t, true)
	wo := h.newWorkerDir(t, "wo", "")
	wo.env = append([]string{"LONGARM_TEST_LEGACY_RELAY=1"}, netEnv...) // an older longarmd: no relay set
	h.joinWorker(t, wo, true)
	wo.start(t, true)

	extEP := fmt.Sprintf("longnet://%s?name=wa&tcp=%s", x.udp, x.tcp)
	hubEP := func(w string) string { return fmt.Sprintf("longnet://%s?name=%s", h.addr(), w) }
	h.waitNode(t, "wa", "online on both relays", func(n map[string]any) bool {
		eps := fmt.Sprint(n["endpoints"])
		return isTrue(n["online"]) && strings.Contains(eps, extEP) && strings.Contains(eps, hubEP("wa"))
	})
	n := h.waitNode(t, "wo", "online on the embedded relay", func(n map[string]any) bool {
		return isTrue(n["online"]) && strings.Contains(fmt.Sprint(n["endpoints"]), hubEP("wo"))
	})
	if eps := fmt.Sprint(n["endpoints"]); strings.Contains(eps, x.udp) {
		t.Fatalf("the legacy worker listens on the external relay: %s", eps)
	}
	var rel struct {
		Relays []struct {
			Endpoint struct {
				UDP      string `json:"udp"`
				TCP      string `json:"tcp"`
				Embedded bool   `json:"embedded"`
			} `json:"endpoint"`
			Connected bool `json:"connected"`
		} `json:"relays"`
		WorkersRelaySet int            `json:"workers_relay_set"`
		WorkersLegacy   int            `json:"workers_legacy"`
		LegacyVersions  map[string]int `json:"legacy_versions"`
	}
	if err := json.Unmarshal([]byte(h.mustLongarm(h.admin, "admin", "relays", "--json")), &rel); err != nil {
		t.Fatal(err)
	}
	if len(rel.Relays) != 2 || rel.Relays[0].Endpoint.UDP != x.udp || rel.Relays[0].Endpoint.TCP != x.tcp || !rel.Relays[0].Connected ||
		!rel.Relays[1].Endpoint.Embedded || rel.WorkersRelaySet != 1 || rel.WorkersLegacy != 1 {
		t.Fatalf("admin relays: %+v", rel)
	}
	if out := h.mustLongarm(h.admin, "admin", "relays"); !strings.Contains(out, "1 use the relay set, 1 only the embedded relay") {
		t.Fatalf("admin relays:\n%s", out)
	}

	// The client reaches each worker through one relay only: wa over the
	// external relay (its TLS fallback learned from the endpoint is not
	// needed here: UDP), wo over the embedded one.
	workers := fmt.Sprintf("[[worker]]\nname = \"wa-x\"\ncert_name = \"wa\"\ntransport = \"longnet\"\naddress = %q\n"+
		"[[worker]]\nname = \"wo-h\"\ncert_name = \"wo\"\ntransport = \"longnet\"\naddress = %q\n", x.udp, h.addr())
	if err := os.WriteFile(filepath.Join(h.client, "workers.toml"), []byte(workers), 0o644); err != nil {
		t.Fatal(err)
	}
	cli := append([]string{"LONGARM_HUB_TIMEOUT=500ms"}, netEnv...)
	longarm := func(args ...string) result { return h.longarmWith(cli, h.client, args...) }
	for _, w := range []string{"wa-x", "wo-h"} {
		if r := longarm("exec", w, "--", bins.helper, "print", "3", "0", "0"); r.code != 0 || r.stdout != lines(1, 3) {
			t.Fatalf("exec %s with the hub up: %+v", w, r)
		}
	}

	// A log stream through each relay, then the hub goes away for longer
	// than the end-to-end idle timeout.
	follow := func(w string) (*proc, string) {
		id := strings.TrimSpace(longarm("run", w, "--", bins.helper, "print", "120", "100", "0").stdout)
		if id == "" {
			t.Fatalf("run on %s failed", w)
		}
		cmd := exec.Command(bins.longarm, "logs", w, id, "-f")
		cmd.Env = append(append(os.Environ(), "LONGARM_CONFIG_DIR="+h.client), cli...)
		return startProc(t, cmd), id
	}
	pa, _ := follow("wa-x")
	po, _ := follow("wo-h")
	waitFor(t, "both streams running", 30*time.Second, func() bool {
		return strings.Contains(pa.out.String(), "line 5") && strings.Contains(po.out.String(), "line 5")
	})
	h.killHub()
	down := time.Now()
	before := len(pa.out.String())
	time.Sleep(2 * time.Second)
	if after := len(pa.out.String()); after <= before {
		t.Fatalf("the stream over the external relay stalled with the hub down (%d -> %d bytes)", before, after)
	}
	if r := longarm("exec", "wa-x", "--", bins.helper, "print", "2", "0", "0"); r.code != 0 || r.stdout != lines(1, 2) {
		t.Fatalf("exec over the external relay with the hub down: %+v", r)
	}
	time.Sleep(time.Until(down.Add(8 * time.Second))) // > the 4 s idle timeout: embedded-relay paths are gone
	h.startHub(t)
	t.Logf("hub was down %s", time.Since(down).Round(100*time.Millisecond))
	for _, p := range []struct {
		name string
		p    *proc
	}{{"external", pa}, {"embedded", po}} {
		select {
		case <-p.p.done:
		case <-time.After(60 * time.Second):
			t.Fatalf("the %s relay's stream did not finish:\n%s", p.name, p.p.out.String())
		}
		if out := p.p.out.String(); !strings.Contains(out, "line 120") {
			t.Fatalf("the %s relay's stream is incomplete:\n%s", p.name, tail(out, 2000))
		}
	}
	if out := pa.out.String(); strings.Contains(out, "retrying") {
		t.Fatalf("the stream over the external relay broke during the hub restart:\n%s", tail(out, 2000))
	}
	if out := po.out.String(); !strings.Contains(out, "retrying") {
		t.Logf("note: the stream over the embedded relay survived the restart without a reconnect")
	}

	// The relay restarts while the hub is down: its cached policy lets it
	// relay at once (a relay without one would refuse every session).
	h.killHub()
	x.stop()
	x.start()
	if hs, _ := x.health()["hub"].(map[string]any); !isTrue(hs["ready"]) || isTrue(hs["connected"]) {
		t.Fatalf("relay restarted without the hub: %v", x.health())
	}
	waitFor(t, "exec over the restarted relay, hub down", 60*time.Second, func() bool {
		r := longarm("exec", "wa-x", "--", bins.helper, "print", "2", "0", "0")
		return r.code == 0 && r.stdout == lines(1, 2)
	})
	h.startHub(t)
	waitFor(t, "the relay back on the hub", 30*time.Second, func() bool {
		hs, _ := x.health()["hub"].(map[string]any)
		return isTrue(hs["connected"])
	})

	// A revocation reaches the relay (and its sessions of that node end).
	carol := filepath.Join(h.root, "carol")
	h.mustLongarm(h.client, "admin", "issue", "--role", "client", "--name", "carol", "--out", filepath.Join(carol, "tls"))
	os.WriteFile(filepath.Join(carol, "hub.toml"), []byte(fmt.Sprintf("address = %q\n", h.addr())), 0o600)
	os.WriteFile(filepath.Join(carol, "workers.toml"), []byte(workers), 0o644)
	if r := h.longarmWith(cli, carol, "exec", "wa-x", "--", bins.helper, "print", "1", "0", "0"); r.code != 0 {
		t.Fatalf("carol exec: %+v", r)
	}
	h.mustLongarm(carol, "whoami") // the hub registers carol's certificate
	revokedBefore := 0.0
	if hs, _ := x.health()["hub"].(map[string]any); hs != nil {
		revokedBefore, _ = hs["revoked"].(float64)
	}
	h.mustLongarm(h.admin, "admin", "revoke", "carol")
	waitFor(t, "the revocation on the relay", 30*time.Second, func() bool {
		hs, _ := x.health()["hub"].(map[string]any)
		n, _ := hs["revoked"].(float64)
		return n > revokedBefore
	})
	if b, _ := os.ReadFile(x.logPath); !strings.Contains(string(b), "revocation list updated") {
		t.Fatalf("relay log without the revocation:\n%s", tail(string(b), 3000))
	}
}
