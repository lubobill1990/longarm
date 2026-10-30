package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestHubRestartMeasured measures what a client keeps through a hub
// restart, and separately through a restart of the external relay: an
// attached session, a running job's log stream, a port forward and the
// tray's pool metrics, each over three paths to the same worker:
//
//	wa-d  a direct path (tcp)
//	wa-x  the external relay (longarm-relay --hub)
//	wa-h  only the hub's embedded relay
//
// Direct longnet paths are off and the end-to-end idle timeout is 4 s, so
// the hub's downtime (8 s) outlasts every relayed connection through it.
// Expected (and asserted):
//   - hub restart: wa-d and wa-x keep everything without a break; wa-h
//     breaks and is back within reconnectBound of the hub;
//   - relay restart: wa-x is back within reconnectBound;
//   - the tray's pool view shows wa-d and wa-x with fresh direct metrics
//     while the hub is down.
func TestHubRestartMeasured(t *testing.T) {
	t.Parallel()
	const reconnectBound = 30 * time.Second
	h := newHubEnv(t, "")
	x := startExtRelay(t, h, "mrelay")
	waitFor(t, "the relay registered", 30*time.Second, func() bool {
		hs, _ := x.health()["hub"].(map[string]any)
		return isTrue(hs["connected"])
	})
	netEnv := []string{"LONGARM_NET_DISABLE_DIRECT=1", "LONGARM_NET_IDLE_TIMEOUT=4s"}
	wa := h.newWorkerDir(t, "wa", "")
	wa.env = netEnv
	h.joinWorker(t, wa, true)
	wa.start(t, true)
	h.waitNode(t, "wa", "on both relays", func(n map[string]any) bool {
		eps := fmt.Sprint(n["endpoints"])
		return isTrue(n["online"]) && strings.Contains(eps, x.udp) && strings.Contains(eps, "longnet://"+h.addr())
	})
	workers := fmt.Sprintf("[[worker]]\nname = \"wa-d\"\ncert_name = \"wa\"\naddress = \"127.0.0.1:%d\"\n"+
		"[[worker]]\nname = \"wa-x\"\ncert_name = \"wa\"\ntransport = \"longnet\"\naddress = %q\n"+
		"[[worker]]\nname = \"wa-h\"\ncert_name = \"wa\"\ntransport = \"longnet\"\naddress = %q\n", wa.port, x.udp, h.addr())
	if err := os.WriteFile(filepath.Join(h.client, "workers.toml"), []byte(workers), 0o644); err != nil {
		t.Fatal(err)
	}
	cli := append([]string{"LONGARM_CONFIG_DIR=" + h.client, "LONGARM_HUB_TIMEOUT=500ms", "LONGARM_POOL_INTERVAL=500ms", "LONGARM_FORWARD_WAIT=60s"}, netEnv...)
	longarm := func(args ...string) result { return h.longarmWith(cli[1:], h.client, args...) }
	bgCmd := func(args ...string) *bg {
		b := &bg{cmd: exec.Command(bins.longarm, args...), done: make(chan struct{})}
		b.cmd.Env = append(os.Environ(), cli...)
		b.cmd.Stdout, b.cmd.Stderr = &b.stdout, &b.stderr
		var err error
		if b.stdin, err = b.cmd.StdinPipe(); err != nil {
			t.Fatal(err)
		}
		if err := b.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go func() { b.err = b.cmd.Wait(); close(b.done) }()
		t.Cleanup(func() {
			select {
			case <-b.done:
			default:
				b.cmd.Process.Kill()
				<-b.done
			}
		})
		return b
	}
	// the tray's daemon (pool view)
	if r := longarm("daemon", "start"); r.code != 0 {
		t.Fatalf("daemon start: %+v", r)
	}
	t.Cleanup(func() { longarm("daemon", "stop") })

	type path struct {
		name    string
		attach  *bg
		logs    *bg
		fwdPort int
		n       int // attach lines sent
	}
	portRE := regexp.MustCompile(`port (\d+)`)
	fwdRE := regexp.MustCompile(`forwarding 127\.0\.0\.1:(\d+) ->`)
	paths := []*path{{name: "wa-d"}, {name: "wa-x"}, {name: "wa-h"}}
	for _, p := range paths {
		p.attach = bgCmd("attach", p.name, "--session", "s-"+p.name, "--", bins.helper, "repl")
		waitOutput(t, p.attach.stdout.String, "repl ready", 60*time.Second)
		id := strings.TrimSpace(longarm("run", p.name, "--", bins.helper, "print", "400", "100", "0").stdout)
		p.logs = bgCmd("logs", p.name, id, "-f")
		sid := strings.TrimSpace(longarm("run", p.name, "--", bins.helper, "serve", "600").stdout)
		var remote string
		waitFor(t, p.name+": the echo server's port", 30*time.Second, func() bool {
			m := portRE.FindStringSubmatch(longarm("logs", p.name, sid).stdout)
			if m != nil {
				remote = m[1]
			}
			return m != nil
		})
		f := bgCmd("forward", p.name, remote, "0")
		waitFor(t, p.name+": forward listening", 30*time.Second, func() bool {
			m := fwdRE.FindStringSubmatch(f.stderr.String())
			if m != nil {
				fmt.Sscan(m[1], &p.fwdPort)
			}
			return m != nil
		})
	}
	// works: one attach round trip and one forwarded connection, each within d
	attachOK := func(p *path, d time.Duration) bool {
		p.n++
		line := fmt.Sprintf("ping %s %d", p.name, p.n)
		p.attach.stdin.Write([]byte(line + "\n"))
		deadline := time.Now().Add(d)
		for time.Now().Before(deadline) {
			if strings.Contains(plain(p.attach.stdout.String()), "echo: "+line) {
				return true
			}
			time.Sleep(100 * time.Millisecond)
		}
		return false
	}
	forwardOK := func(p *path, d time.Duration) bool {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", p.fwdPort), d)
		if err != nil {
			return false
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(d))
		fmt.Fprintf(c, "hi\n")
		l, err := bufio.NewReader(c).ReadString('\n')
		return err == nil && strings.Contains(l, "echo hi")
	}
	pool := func() map[string]map[string]any {
		r := longarm("pool", "--json")
		var v struct {
			Workers []map[string]any `json:"workers"`
		}
		json.Unmarshal([]byte(r.stdout), &v)
		out := map[string]map[string]any{}
		for _, w := range v.Workers {
			out[fmt.Sprint(w["name"])] = w
		}
		return out
	}
	for _, p := range paths {
		if !attachOK(p, 15*time.Second) || !forwardOK(p, 15*time.Second) {
			t.Fatalf("%s: not working before the restart", p.name)
		}
	}

	// ---- the hub restarts ----
	h.killHub()
	down := time.Now()
	time.Sleep(6 * time.Second) // > the idle timeout: paths through the hub's relay are gone
	for _, p := range paths[:2] {
		if !attachOK(p, 5*time.Second) {
			t.Errorf("hub down: attach over %s stalled", p.name)
		}
		if !forwardOK(p, 5*time.Second) {
			t.Errorf("hub down: a forwarded connection over %s failed", p.name)
		}
	}
	pv := pool()
	for _, n := range []string{"wa-d", "wa-x"} {
		if w := pv[n]; w == nil || w["source"] != "direct" || w["state"] != "online" {
			t.Errorf("hub down: pool view of %s: %v", n, w)
		}
	}
	hubDownH := attachOK(paths[2], 2*time.Second)
	time.Sleep(time.Until(down.Add(8 * time.Second)))
	h.startHub(t)
	back := time.Now()
	t.Logf("hub down %s; embedded-relay attach worked while down: %v", back.Sub(down).Round(100*time.Millisecond), hubDownH)
	ph := paths[2]
	var attachBack, fwdBack time.Duration
	waitFor(t, "wa-h attach back", reconnectBound, func() bool {
		ok := attachOK(ph, time.Second)
		attachBack = time.Since(back)
		return ok
	})
	waitFor(t, "wa-h forward back", reconnectBound, func() bool {
		ok := forwardOK(ph, 2*time.Second)
		fwdBack = time.Since(back)
		return ok
	})
	t.Logf("MEASURE hub restart: direct and external relay: no break; embedded relay: attach back %s, forward back %s after the hub",
		attachBack.Round(100*time.Millisecond), fwdBack.Round(100*time.Millisecond))
	for _, p := range paths[:2] {
		if out := p.attach.stderr.String(); strings.Contains(out, "connection to") && strings.Contains(out, "lost") {
			t.Errorf("attach over %s broke during the hub restart:\n%s", p.name, out)
		}
		if out := p.logs.stderr.String(); strings.Contains(out, "retrying") {
			t.Errorf("logs -f over %s broke during the hub restart:\n%s", p.name, out)
		}
	}

	// ---- the external relay restarts (hub up) ----
	x.stop()
	rdown := time.Now()
	x.start()
	px := paths[1]
	var xBack time.Duration
	waitFor(t, "wa-x attach after the relay restart", reconnectBound, func() bool {
		ok := attachOK(px, time.Second)
		xBack = time.Since(rdown)
		return ok
	})
	waitFor(t, "wa-x forward after the relay restart", reconnectBound, func() bool { return forwardOK(px, 2*time.Second) })
	t.Logf("MEASURE relay restart: wa-x attach back %s after the restart", xBack.Round(100*time.Millisecond))
	if !attachOK(paths[0], 5*time.Second) {
		t.Error("the direct path broke during the relay restart")
	}

	// every log stream ends complete (reconnecting where needed)
	for _, p := range paths {
		select {
		case <-p.logs.done:
		case <-time.After(90 * time.Second):
			t.Fatalf("logs -f over %s did not finish:\n%s", p.name, tail(plain(p.logs.stderr.String()), 2000))
		}
		if !strings.Contains(p.logs.stdout.String(), "line 400") {
			t.Errorf("logs -f over %s incomplete:\n%s", p.name, tail(p.logs.stdout.String(), 500))
		}
	}
}
