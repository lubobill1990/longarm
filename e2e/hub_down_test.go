package e2e

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHubDownFast: with the hub unreachable in the slow way (TCP accepted
// but never answered, UDP swallowed - like a VM whose packets are dropped),
// longarm ls asks the workers at once and is done after the short hub connect
// timeout instead of hub timeout + relay dial timeouts; the failure is
// remembered (hub-state.json), so the next invocations skip the hub and its
// relay leg entirely until the retry window passed; a hub that is back is
// used again and the remembered failure is cleared.
func TestHubDownFast(t *testing.T) {
	t.Parallel()
	h := newHubEnv(t, "")
	os.Remove(filepath.Join(h.client, "workers.toml")) // workers come from the hub (and its cache)
	wa := h.newWorkerDir(t, "wa", "")
	os.WriteFile(filepath.Join(wa.dataDir, "longarmd.toml"), []byte(fmt.Sprintf(`listen = ["tcp://127.0.0.1:%d", "longnet://%s?name=wa"]
`, wa.port, h.addr())), 0o600)
	h.joinWorker(t, wa, true)
	wa.start(t, true)
	h.waitNode(t, "wa", "online with both legs", func(n map[string]any) bool {
		eps := fmt.Sprint(n["endpoints"])
		return isTrue(n["online"]) && strings.Contains(eps, "longnet://") && strings.Contains(eps, "tcp://")
	})
	longarm := func(env []string, args ...string) (result, time.Duration) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bins.longarm, args...)
		cmd.Env = append(append(os.Environ(), "LONGARM_CONFIG_DIR="+h.client), env...)
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		start := time.Now()
		err := cmd.Run()
		took := time.Since(start)
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("longarm %v: %v", args, err)
		}
		return result{out.String(), errb.String(), code}, took
	}
	if r, _ := longarm(nil, "ls", "-o", "wide"); r.code != 0 || !strings.Contains(r.stdout, "wa ") || !strings.Contains(r.stdout, "hub") {
		t.Fatalf("longarm ls with the hub: %+v", r)
	}
	// Prefer the longnet leg so a hanging relay would hold every dial up.
	if r, _ := longarm(nil, "transport", "longnet"); r.code != 0 {
		t.Fatalf("longarm transport longnet: %+v", r)
	}

	// Baselines with the hub up (the machine may be loaded by other tests):
	// longarm ls, and exec over the tcp leg alone.
	_, lsBase := longarm(nil, "ls")
	_, execBase := longarm([]string{"LONGARM_TRANSPORT=tcp"}, "exec", "wa", "--", bins.helper, "print", "1", "0", "7")
	t.Logf("baselines: longarm ls %s, exec over tcp %s", lsBase, execBase)
	// A loaded runner stretches every step, the baselines included (a CI
	// VM measured 3 s for longarm ls): the bounds below are twice the baseline
	// plus the expected extra - still far below the multi-second connect
	// timeouts they guard against on an idle machine.
	within := func(base, extra time.Duration) time.Duration { return 2*base + extra }

	// The hub "disappears": its address now swallows everything.
	h.killHub()
	tcpBH, err := net.Listen("tcp", h.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer tcpBH.Close()
	go func() {
		var held []net.Conn
		defer func() {
			for _, c := range held {
				c.Close()
			}
		}()
		for {
			c, err := tcpBH.Accept()
			if err != nil {
				return
			}
			held = append(held, c) // accepted, never answered
		}
	}()
	udpBH, err := net.ListenPacket("udp", h.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer udpBH.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			if _, _, err := udpBH.ReadFrom(buf); err != nil {
				return
			}
		}
	}()
	os.Remove(filepath.Join(h.client, "hub-state.json"))

	// A single run can still hit a load spike: up to 3 attempts, the best
	// one counts.
	attempts := func(bound time.Duration, run func() time.Duration) time.Duration {
		best := time.Duration(1<<63 - 1)
		for range 3 {
			best = min(best, run())
			if best <= bound {
				break
			}
		}
		return best
	}

	// 1. First longarm ls: one hub connect timeout, workers probed meanwhile.
	var r result
	took := attempts(within(lsBase, 3*time.Second), func() time.Duration {
		os.Remove(filepath.Join(h.client, "hub-state.json")) // a first contact each time
		var d time.Duration
		r, d = longarm(nil, "ls", "-o", "wide")
		t.Logf("longarm ls, hub black-holed: %s", d)
		if r.code != 0 || !strings.Contains(r.stdout, "wa ") || !strings.Contains(r.stdout, "tcp/") || !strings.Contains(r.stderr, "unavailable") {
			t.Fatalf("longarm ls with the hub down: %+v", r)
		}
		return d
	})
	if took > within(lsBase, 3*time.Second) {
		t.Fatalf("longarm ls took %s with the hub down (want about the 1.5 s hub connect timeout over %s)", took, lsBase)
	}
	if b, err := os.ReadFile(filepath.Join(h.client, "hub-state.json")); err != nil || !strings.Contains(string(b), h.addr()) {
		t.Fatalf("hub-state.json: %s %v", b, err)
	}
	// 2. The next invocations skip the hub (and the longnet leg through its
	// relay) at once.
	took = attempts(within(lsBase, 1500*time.Millisecond), func() time.Duration {
		var d time.Duration
		r, d = longarm(nil, "ls")
		t.Logf("longarm ls, hub down remembered: %s", d)
		if r.code != 0 || !strings.Contains(r.stdout, "wa ") || !strings.Contains(r.stderr, "was unavailable") {
			t.Fatalf("longarm ls with the remembered failure: %+v", r)
		}
		return d
	})
	if took > within(lsBase, 1500*time.Millisecond) {
		t.Fatalf("longarm ls took %s with the hub failure remembered (baseline %s)", took, lsBase)
	}
	// The preferred longnet leg is skipped at once (no 2 s preference grace).
	took = attempts(within(execBase, 1500*time.Millisecond), func() time.Duration {
		r, d := longarm(nil, "exec", "wa", "--", bins.helper, "print", "1", "0", "7")
		if r.code != 7 {
			t.Fatalf("exec with the hub failure remembered: %+v", r)
		}
		return d
	})
	if took > within(execBase, 1500*time.Millisecond) {
		t.Fatalf("exec with the hub failure remembered took %s (baseline %s)", took, execBase)
	}
	if r, _ := longarm(nil, "info", "wa"); !strings.Contains(r.stdout, "the hub's relay is down") {
		t.Fatalf("longarm info: the longnet leg should be skipped:\n%s", r.stdout)
	}

	// 3. Hub back; after the retry window longarm uses it again and forgets the
	// failure.
	tcpBH.Close()
	udpBH.Close()
	state := must(os.ReadFile(filepath.Join(h.client, "hub-state.json")))
	h.startHub(t) // (clears hub-state.json for the other tests: put it back)
	os.WriteFile(filepath.Join(h.client, "hub-state.json"), state, 0o600)
	h.waitNode(t, "wa", "online again", func(n map[string]any) bool { return isTrue(n["online"]) })
	r, _ = longarm([]string{"LONGARM_HUB_RETRY_AFTER=0s"}, "ls", "-o", "wide")
	if r.code != 0 || !strings.Contains(r.stdout, "hub") || strings.Contains(r.stderr, "unavailable") {
		t.Fatalf("longarm ls with the hub back: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(h.client, "hub-state.json")); !os.IsNotExist(err) {
		t.Fatalf("hub-state.json kept after the hub answered: %v", err)
	}
}
