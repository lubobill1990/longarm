package e2e

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestHubRestartReconnect measures what a hub redeploy costs the fleet
// after the new hub answers: N workers are online, the hub stops (SIGTERM
// where there is one: it announces the restart; Windows: killed) and stays
// down as long as an Azure Container Instances redeploy takes, then starts
// again. Every worker must be back within the bound after the hub answers.
// Before the reconnect backoff was capped for connection errors
// (hubclient.Reconnect) a worker could sleep 30 s more after a 30 s outage
// (waits 1, 2, 4, 8, 16, 30 s).
//
// LONGARM_E2E_RESTART_WORKERS (default 6) and LONGARM_E2E_RESTART_DOWN
// (default 32s) change the fleet size and the outage.
func TestHubRestartReconnect(t *testing.T) {
	t.Parallel()
	n := 6
	if v, err := strconv.Atoi(os.Getenv("LONGARM_E2E_RESTART_WORKERS")); err == nil && v > 0 {
		n = v
	}
	down := 32 * time.Second
	if v, err := time.ParseDuration(os.Getenv("LONGARM_E2E_RESTART_DOWN")); err == nil && v >= 0 {
		down = v
	}
	// the slowest worker: the fast cap (5 s) + 20% jitter + the dial, plus
	// the admin CLI's polling on a loaded runner
	const bound = 12 * time.Second

	h := newHubEnv(t, "")
	var names []string
	for i := range n {
		w := h.newWorkerDir(t, fmt.Sprintf("rw%d", i), "")
		h.joinWorkerPaced(t, w)
		w.start(t, true)
		names = append(names, w.name)
	}
	online := func() map[string]bool {
		m := map[string]bool{}
		for _, nd := range h.nodes(t) {
			if name, _ := nd["name"].(string); slices.Contains(names, name) && isTrue(nd["online"]) {
				m[name] = true
			}
		}
		return m
	}
	waitFor(t, "all workers online", 60*time.Second, func() bool { return len(online()) == n })

	graceful := h.stopHubGracefully()
	stopped := time.Now()
	t.Logf("hub stopped (graceful=%v); down for %s", graceful, down)
	time.Sleep(down)
	h.startHub(t)
	answered := time.Now()

	back := map[string]time.Duration{}
	deadline := answered.Add(bound + 20*time.Second) // collect late ones too, for the report
	for len(back) < n && time.Now().Before(deadline) {
		for name := range online() {
			if _, ok := back[name]; !ok {
				back[name] = time.Since(answered)
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	var slowest time.Duration
	var lines []string
	for _, name := range names {
		d, ok := back[name]
		if !ok {
			lines = append(lines, name+": not back")
			slowest = time.Hour
			continue
		}
		slowest = max(slowest, d)
		lines = append(lines, fmt.Sprintf("%s: %s", name, d.Round(100*time.Millisecond)))
	}
	t.Logf("hub restart: %d workers, outage %s (stop to answer %s); back after the hub answered: %s",
		n, down, answered.Sub(stopped).Round(100*time.Millisecond), strings.Join(lines, ", "))
	fmt.Printf("RESTART_TIMING workers=%d outage=%s slowest=%s\n", n, down, slowest.Round(100*time.Millisecond))
	if slowest > bound {
		t.Fatalf("workers back %s after the hub answered (bound %s): %s", slowest.Round(100*time.Millisecond), bound, strings.Join(lines, ", "))
	}
	if graceful {
		// the restart notice reached the workers
		for _, w := range h.workers {
			b, _ := os.ReadFile(w.logPath)
			if !strings.Contains(string(b), "hub: restarting") {
				t.Errorf("%s logged no restart notice", w.name)
			}
		}
	}
}

// joinWorkerPaced is joinWorker (approved) for many workers from one
// address: the hub allows 10 join attempts per IP, then one every 5 s.
func (h *hubEnv) joinWorkerPaced(t *testing.T, w *hubWorker) {
	t.Helper()
	code := strings.TrimSpace(h.mustLongarm(h.admin, "admin", "join-code", "--role", "worker", "--labels", "pool=e2e,tier=test"))
	for try := 0; ; try++ {
		r := longarmdCmd(t, "join", h.addr(), code, "--data-dir", w.dataDir, "--name", w.name, "--ca-fingerprint", h.fp, "--no-wait")
		if r.code == 0 && strings.Contains(r.stdout, "pending approval") {
			break
		}
		if !strings.Contains(r.stderr, "too many join attempts") || try >= 10 {
			t.Fatalf("longarmd join %s: %+v", w.name, r)
		}
		time.Sleep(6 * time.Second)
	}
	if out := h.mustLongarm(h.admin, "admin", "approve", w.name); !strings.Contains(out, "approved") {
		t.Fatalf("approve: %s", out)
	}
}

// stopHubGracefully stops the hub with SIGTERM (as ACI / docker stop do)
// and waits for it; where there is no SIGTERM (Windows) it kills it.
// Reports whether the stop was graceful.
func (h *hubEnv) stopHubGracefully() bool {
	h.hmu.Lock()
	defer h.hmu.Unlock()
	if h.hub == nil {
		return false
	}
	graceful := h.hub.Process.Signal(syscall.SIGTERM) == nil
	if !graceful {
		h.hub.Process.Kill()
	}
	h.hub.Process.Wait()
	h.hub = nil
	return graceful
}
