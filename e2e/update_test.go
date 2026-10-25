package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/update"
)

// releaseBuild builds longarmd and longarm with an update key and a version stamp
// (extra ldflags appended) into dir, named like release files
// (longarmd-<os>-<arch>[.exe]) when releaseNames is set.
func releaseBuild(t *testing.T, dir, version, pubKey, extraLD string, releaseNames bool) {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	ld := fmt.Sprintf("-X github.com/lubobill1990/longarm/internal/version.Version=%s -X github.com/lubobill1990/longarm/internal/update.PublicKey=%s %s",
		version, pubKey, extraLD)
	goBin := filepath.Join(runtime.GOROOT(), "bin", exeName("go"))
	if _, err := os.Stat(goBin); err != nil {
		goBin = "go"
	}
	var wg sync.WaitGroup
	errs := make(chan string, 2)
	for _, bin := range []string{"longarmd", "longarm"} {
		out := filepath.Join(dir, exeName(bin))
		if releaseNames {
			out = filepath.Join(dir, update.ReleaseName(bin, runtime.GOOS, runtime.GOARCH))
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b, err := exec.Command(goBin, "build", "-ldflags", ld, "-o", out, "../cmd/"+bin).CombinedOutput(); err != nil {
				errs <- fmt.Sprintf("build %s %s: %v\n%s", bin, version, err, b)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
}

func binVersion(t *testing.T, exe string) string {
	t.Helper()
	arg := "version" // longarmd version; longarm --version prints "longarm version X"
	if strings.HasPrefix(filepath.Base(exe), "longarm.") || filepath.Base(exe) == "longarm" {
		arg = "--version"
	}
	b, err := exec.Command(exe, arg).Output()
	if err != nil {
		t.Fatalf("%s %s: %v", exe, arg, err)
	}
	f := strings.Fields(string(b))
	return f[len(f)-1]
}

// TestSelfUpdate: signed releases on the hub; `longarm admin release push` with
// a canary updates a worker (longarmd and longarm swapped, old files kept as .old, a
// job started before the update keeps running in its shim and finishes);
// then a release whose new longarmd fails its health check is rolled back by the
// watchdog (old binary restored and restarted), the rollout halts and the
// version is not retried. longarmd runs in the foreground here ("exec" restart
// mode); TestWindowsServiceUpdate covers the SCM path.
func TestSelfUpdate(t *testing.T) {
	if testing.Short() {
		t.Skip("slow (builds three longarmd versions); skipped with -short")
	}
	t.Parallel()
	pub, priv, err := update.GenerateKey("")
	if err != nil {
		t.Fatal(err)
	}
	h := newHubEnv(t, "")
	keyFile := filepath.Join(h.root, "release.key")
	os.WriteFile(keyFile, priv, 0o600)
	install := filepath.Join(h.root, "install")
	rel2, rel3 := filepath.Join(h.root, "rel2"), filepath.Join(h.root, "rel3")
	var wg sync.WaitGroup
	for _, b := range []struct {
		dir, ver, ld string
		rel          bool
	}{
		{install, "1.0.0", "", false},
		{rel2, "2.0.0", "", true},
		{rel3, "3.0.0", "-X github.com/lubobill1990/longarm/internal/update.failHealthForTest=e2e-broken-build", true},
	} {
		wg.Add(1)
		go func() { defer wg.Done(); releaseBuild(t, b.dir, b.ver, pub, b.ld, b.rel) }()
	}
	wg.Wait()
	if t.Failed() {
		return
	}
	for _, r := range []struct{ dir, ver string }{{rel2, "2.0.0"}, {rel3, "3.0.0"}} {
		if out := h.hubCmd(t, "release", "sign", r.dir, "--version", r.ver, "--key", keyFile); !strings.Contains(out, "signed longarmd-") {
			t.Fatalf("sign: %s", out)
		}
		if out := h.hubCmd(t, "release", "add", r.dir, "--version", r.ver); !strings.Contains(out, " signed") {
			t.Fatalf("release add: %s", out)
		}
	}
	// A tampered binary with a valid-looking signature is refused by the
	// hub when it knows the key.
	os.WriteFile(h.cfgPath, append(must(os.ReadFile(h.cfgPath)), []byte(fmt.Sprintf("[releases]\npublic_key = %q\n", pub))...), 0o600)
	bad := filepath.Join(h.root, "relbad")
	os.MkdirAll(bad, 0o755)
	name := update.ReleaseName("longarmd", runtime.GOOS, runtime.GOARCH)
	os.WriteFile(filepath.Join(bad, name), []byte("not longarmd"), 0o755)
	os.WriteFile(filepath.Join(bad, name+".minisig"), must(os.ReadFile(filepath.Join(rel2, name+".minisig"))), 0o644)
	cmd := exec.Command(h.hubBin, "--config", h.cfgPath, "release", "add", bad, "--version", "2.0.0")
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "invalid signature") {
		t.Fatalf("tampered release accepted: %v %s", err, out)
	}

	wa := h.newWorkerDir(t, "wa", "[update]\nhealth_window = \"4s\"\nstart_grace = \"45s\"\n")
	wa.exe = filepath.Join(install, exeName("longarmd"))
	wa.args = []string{"--log-file", filepath.Join(h.root, "longarmd-wa-file.log")}
	t.Cleanup(func() {
		// Relaunched longarmds are not children of the test.
		h.longarm(h.admin, "admin", "shutdown", "wa")
		if st, _ := update.LoadState(wa.dataDir); st != nil && st.PID != 0 {
			if p, err := os.FindProcess(st.PID); err == nil {
				p.Kill()
			}
		}
		killShims(wa.dataDir)
		if t.Failed() {
			b, _ := os.ReadFile(filepath.Join(h.root, "longarmd-wa-file.log"))
			t.Logf("longarmd log:\n%s", tail(string(b), 6000))
			b, _ = os.ReadFile(filepath.Join(wa.dataDir, "update-watch.log"))
			t.Logf("watchdog log:\n%s", b)
		}
	})
	h.joinWorker(t, wa, true)
	wa.start(t, true)
	h.waitNode(t, "wa", "online 1.0.0", func(n map[string]any) bool { return isTrue(n["online"]) && n["version"] == "1.0.0" })
	if ls := h.mustLongarm(h.admin, "admin", "release", "ls"); !strings.Contains(ls, "2.0.0") || !strings.Contains(ls, "3.0.0") {
		t.Fatalf("release ls:\n%s", ls)
	}
	// A job that outlives both updates.
	job := strings.TrimSpace(h.mustLongarm(h.client, "run", "wa", "--", bins.helper, "print", "400", "100", "9"))

	// 1. Happy path: canary = the only worker, healthy -> done.
	out := h.mustLongarm(h.admin, "admin", "release", "push", "2.0.0", "--canary", "1", "--wait", "2s")
	if !strings.Contains(out, "canary") || !strings.Contains(out, "wa") {
		t.Fatalf("push:\n%s", out)
	}
	h.waitNode(t, "wa", "online 2.0.0", func(n map[string]any) bool { return isTrue(n["online"]) && n["version"] == "2.0.0" })
	waitFor(t, "rollout done", 60*time.Second, func() bool {
		return strings.Contains(h.mustLongarm(h.admin, "admin", "release", "status"), "rollout 2.0.0 (channel stable): done")
	})
	if v := binVersion(t, filepath.Join(install, exeName("longarm"))); v != "2.0.0" {
		t.Fatalf("longarm not updated: %s", v)
	}
	if _, err := os.Stat(filepath.Join(install, exeName("longarmd")) + ".old"); err != nil {
		t.Fatalf("no backup: %v", err)
	}
	if r := h.longarm(h.client, "exec", "wa", "--", bins.helper, "print", "1", "0", "4"); r.code != 4 {
		t.Fatalf("exec on 2.0.0: %+v", r)
	}

	// 2. A broken release, pushed while 2.0.0 may still be in its health
	// window (the worker defers the check until 2.0.0 is healthy): its longarmd
	// fails the health check; the watchdog restores 2.0.0, the rollout
	// halts, 3.0.0 is not retried.
	if st, _ := update.LoadState(wa.dataDir); st != nil {
		t.Logf("pushing 3.0.0 while 2.0.0 is %s", st.Phase)
	}
	h.mustLongarm(h.admin, "admin", "release", "push", "3.0.0", "--canary", "1", "--wait", "2s")
	waitFor(t, "rollout halted", 90*time.Second, func() bool {
		return strings.Contains(h.mustLongarm(h.admin, "admin", "release", "status"), "halted")
	})
	st := h.mustLongarm(h.admin, "admin", "release", "status")
	if !strings.Contains(st, "failed    wa") || !strings.Contains(st, "health check failed") {
		t.Fatalf("status:\n%s", st)
	}
	h.waitNode(t, "wa", "back on 2.0.0", func(n map[string]any) bool { return isTrue(n["online"]) && n["version"] == "2.0.0" })
	if v := binVersion(t, filepath.Join(install, exeName("longarmd"))); v != "2.0.0" {
		t.Fatalf("longarmd after rollback: %s", v)
	}
	if v := binVersion(t, filepath.Join(install, exeName("longarm"))); v != "2.0.0" {
		t.Fatalf("longarm after rollback: %s", v)
	}
	if !update.Skipped(wa.dataDir, "3.0.0") {
		t.Fatal("3.0.0 not on the skip list")
	}
	if r := longarmdCmd(t, "update", "status", "--data-dir", wa.dataDir); !strings.Contains(r.stdout, "2.0.0 -> 3.0.0: rolled_back") {
		t.Fatalf("longarmd update status: %+v", r)
	}
	// The job survived both restarts and the rollback.
	if r := h.longarm(h.client, "wait", "wa", job); r.code != 9 {
		t.Fatalf("job across updates: %+v", r)
	}
	logs := h.mustLongarm(h.client, "logs", "wa", job)
	if !strings.Contains(logs, "line 400") || bytes.Count([]byte(logs), []byte("line ")) != 400 {
		t.Fatalf("job log incomplete (%d lines)", bytes.Count([]byte(logs), []byte("line ")))
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
