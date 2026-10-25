//go:build windows

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/update"
)

// TestWindowsServiceUpdate (opt-in: LONGARM_SERVICE_TEST=1, elevated): longarmd
// runs as an SCM service (LocalSystem) and updates itself through the hub:
//  1. 2.0.0 healthy: swapped, the service exits, the SCM's recovery starts
//     the new binary, the rollout completes;
//  2. 3.0.0 fails its health check: the watchdog restores 2.0.0 and kills
//     3.0.0, the SCM restarts 2.0.0, the rollout halts;
//  3. 4.0.0 is not even longarmd (exits at once): the service crash-loops until
//     the watchdog's start grace ends and it restores 2.0.0.
//
// A job started under 1.0.0 survives all of it.
func TestWindowsServiceUpdate(t *testing.T) {
	if os.Getenv("LONGARM_SERVICE_TEST") != "1" {
		t.Skip("set LONGARM_SERVICE_TEST=1 (elevated) to run the SCM service tests")
	}
	if !platform.Elevated() {
		t.Skip("needs an elevated (administrator) process")
	}
	pub, priv, err := update.GenerateKey("")
	if err != nil {
		t.Fatal(err)
	}
	h := newHubEnv(t, "")
	keyFile := filepath.Join(h.root, "release.key")
	os.WriteFile(keyFile, priv, 0o600)
	install := filepath.Join(h.root, "install")
	rels := map[string]string{"2.0.0": filepath.Join(h.root, "rel2"), "3.0.0": filepath.Join(h.root, "rel3"), "4.0.0": filepath.Join(h.root, "rel4")}
	var wg sync.WaitGroup
	for _, b := range []struct{ dir, ver, ld string }{
		{install, "1.0.0", ""},
		{rels["2.0.0"], "2.0.0", ""},
		{rels["3.0.0"], "3.0.0", "-X github.com/lubobill1990/longarm/internal/update.failHealthForTest=svc-broken-build"},
	} {
		wg.Add(1)
		go func() { defer wg.Done(); releaseBuild(t, b.dir, b.ver, pub, b.ld, b.dir != install) }()
	}
	wg.Wait()
	if t.Failed() {
		return
	}
	// 4.0.0: "longarmd" is the e2e helper (prints usage, exits 2), plus a real longarm.
	os.MkdirAll(rels["4.0.0"], 0o755)
	copyFile(t, bins.helper, filepath.Join(rels["4.0.0"], update.ReleaseName("longarmd", runtime.GOOS, runtime.GOARCH)))
	copyFile(t, filepath.Join(rels["2.0.0"], update.ReleaseName("longarm", runtime.GOOS, runtime.GOARCH)),
		filepath.Join(rels["4.0.0"], update.ReleaseName("longarm", runtime.GOOS, runtime.GOARCH)))
	for _, v := range []string{"2.0.0", "3.0.0", "4.0.0"} {
		h.hubCmd(t, "release", "sign", rels[v], "--version", v, "--key", keyFile)
		h.hubCmd(t, "release", "add", rels[v], "--version", v)
	}

	svc := "longarmd-upd-" + randHex(3)
	wa := h.newWorkerDir(t, "wa", "[update]\nhealth_window = \"4s\"\nstart_grace = \"75s\"\n")
	longarmdExe := filepath.Join(install, "longarmd.exe")
	h.joinWorker(t, wa, true)
	t.Cleanup(func() {
		exec.Command(longarmdExe, "service", "uninstall", "--name", svc).Run()
		if st, _ := update.LoadState(wa.dataDir); st != nil && st.WatchdogPID != 0 {
			platform.KillProcess(st.WatchdogPID)
		}
		killShims(wa.dataDir)
		if t.Failed() {
			b, _ := os.ReadFile(filepath.Join(wa.dataDir, "longarmd.log"))
			t.Logf("longarmd.log:\n%s", tail(string(b), 8000))
			b, _ = os.ReadFile(filepath.Join(wa.dataDir, "update-watch.log"))
			t.Logf("watchdog log:\n%s", b)
		}
	})
	if out, err := exec.Command(longarmdExe, "service", "install", "--name", svc, "--data-dir", wa.dataDir).CombinedOutput(); err != nil {
		t.Fatalf("service install: %v\n%s", err, out)
	}
	if out, err := exec.Command(longarmdExe, "service", "start", "--name", svc).CombinedOutput(); err != nil {
		t.Fatalf("service start: %v\n%s", err, out)
	}
	online := func(v string) {
		t.Helper()
		h.waitNode(t, "wa", "online "+v, func(n map[string]any) bool { return isTrue(n["online"]) && n["version"] == v })
	}
	waitRollout := func(v, state string, d time.Duration) string {
		t.Helper()
		var out string
		waitFor(t, "rollout "+v+" "+state, d, func() bool {
			out = h.mustLongarm(h.admin, "admin", "release", "status")
			return strings.Contains(out, "rollout "+v+" (channel stable): "+state)
		})
		return out
	}
	online("1.0.0")
	job := strings.TrimSpace(h.mustLongarm(h.client, "run", "wa", "--", bins.helper, "sleep", "200"))

	// 1. healthy update through the SCM.
	start := time.Now()
	h.mustLongarm(h.admin, "admin", "release", "push", "2.0.0", "--canary", "1", "--wait", "2s")
	online("2.0.0")
	waitRollout("2.0.0", "done", 90*time.Second)
	t.Logf("2.0.0 rolled out in %s", time.Since(start).Round(time.Second))
	if st, _ := platform.QueryService(svc); st != platform.ServiceRunning {
		t.Fatalf("service %v after the update", st)
	}

	// 2. health check fails: rolled back to 2.0.0.
	start = time.Now()
	h.mustLongarm(h.admin, "admin", "release", "push", "3.0.0", "--canary", "1", "--wait", "2s")
	out := waitRollout("3.0.0", "halted", 3*time.Minute)
	if !strings.Contains(out, "health check failed") {
		t.Fatalf("status:\n%s", out)
	}
	online("2.0.0")
	t.Logf("3.0.0 rolled back in %s", time.Since(start).Round(time.Second))

	// 3. the new binary doesn't even start: rolled back after the start grace.
	start = time.Now()
	h.mustLongarm(h.admin, "admin", "release", "push", "4.0.0", "--canary", "1", "--wait", "2s")
	out = waitRollout("4.0.0", "halted", 5*time.Minute)
	if !strings.Contains(out, "did not start") && !strings.Contains(out, "exited") {
		t.Fatalf("status:\n%s", out)
	}
	online("2.0.0")
	t.Logf("4.0.0 rolled back in %s", time.Since(start).Round(time.Second))
	if v := binVersion(t, longarmdExe); v != "2.0.0" {
		t.Fatalf("installed longarmd %s", v)
	}

	// The job started under 1.0.0 still runs (its shim is independent).
	r := h.longarm(h.client, "jobs", "wa", "--json")
	if !strings.Contains(r.stdout, job) || !strings.Contains(r.stdout, "RUNNING") {
		t.Fatalf("job %s after three updates: %+v", job, r)
	}
	fmt.Fprintln(os.Stderr, "service update test done")
}
