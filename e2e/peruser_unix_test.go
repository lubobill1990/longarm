//go:build linux || darwin

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// perUserBase is a fresh dir the accounts can traverse (go test's temp
// dirs are 0700).
func perUserBase(t *testing.T) string {
	t.Helper()
	base, err := os.MkdirTemp("/tmp", "longarm-peruser-")
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(base, 0o755)
	return base
}

// startPerUserWorker runs longarmd as root (the test runs as root).
func startPerUserWorker(t *testing.T, w *puWorker, exe string) func() {
	t.Helper()
	w.logPath = filepath.Join(w.base, "longarmd.log")
	logf, err := os.Create(w.logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "run", "--data-dir", w.dataDir)
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	logf.Close()
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	return func() {
		if t.Failed() {
			// goroutine dump into the log (a hung worker is otherwise opaque)
			cmd.Process.Signal(syscall.SIGQUIT)
			select {
			case <-exited:
			case <-time.After(3 * time.Second):
			}
		}
		cmd.Process.Kill()
		<-exited
		killShims(w.dataDir)
		users, _ := os.ReadDir(filepath.Join(w.dataDir, "users"))
		for _, u := range users {
			killShims(filepath.Join(w.dataDir, "users", u.Name()))
		}
		time.Sleep(300 * time.Millisecond)
		if t.Failed() {
			b, _ := os.ReadFile(w.logPath)
			t.Logf("per-user longarmd log:\n%s", tail(string(b), 80000))
		}
	}
}

// TestPerUserRefusedOnMacOS: macOS workers refuse per-user isolation with a
// clear error instead of silently running everything as one account.
func TestPerUserRefusedOnMacOS(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	e := newEnv(t)
	os.WriteFile(filepath.Join(e.dataDir, "longarmd.toml"), []byte("[isolation]\nmode = \"per-user\"\n"), 0o600)
	out, err := exec.Command(bins.longarmd, "run", "--data-dir", e.dataDir).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "not supported on macOS") {
		t.Fatalf("longarmd with per-user isolation on macOS: %v\n%s", err, out)
	}
}

// TestPerUserRootDataDirDefault: as root on Linux, `longarmd join` without
// --data-dir enrolls into the system data dir (/var/lib/longarmd; here a
// temp dir via LONGARM_TEST_SYSTEM_DATA_DIR), `longarmd service install
// --isolation per-user` accepts it, refuses a data dir below a home the
// accounts cannot traverse, and adopts a legacy enrollment in ~root/.longarm.
// Only --dry-run installs: no service is created.
func TestPerUserRootDataDirDefault(t *testing.T) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("Linux, as root")
	}
	h := newHubEnv(t, "")
	base := perUserBase(t)
	t.Cleanup(func() { os.RemoveAll(base) })
	sys := filepath.Join(base, "longarmd") // base is 0755, like /var/lib
	home := filepath.Join(base, "root")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	os.Chmod(home, 0o700)
	legacy := filepath.Join(home, ".longarm")
	run := func(sysDir string, args ...string) result {
		t.Helper()
		cmd := exec.Command(bins.longarmd, args...)
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, "LONGARM_DATA_DIR=") && !strings.HasPrefix(kv, "HOME=") && !strings.HasPrefix(kv, "LONGARM_TEST_SYSTEM_DATA_DIR=") {
				cmd.Env = append(cmd.Env, kv)
			}
		}
		cmd.Env = append(cmd.Env, "HOME="+home, "LONGARM_TEST_SYSTEM_DATA_DIR="+sysDir)
		var out, errb strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("longarmd %v: %v", args, err)
		}
		return result{out.String(), errb.String(), code}
	}

	code := strings.TrimSpace(h.mustLongarm(h.admin, "admin", "join-code", "--role", "worker"))
	r := run(sys, "join", h.addr(), code, "--ca-fingerprint", h.fp, "--no-wait", "--name", "rootdd")
	if r.code != 0 || !strings.Contains(r.stdout, "data dir "+sys+"\n") || !strings.Contains(r.stdout, "pending approval") {
		t.Fatalf("longarmd join as root: %+v", r)
	}
	for _, f := range []string{"tls/ca.crt", "tls/pending.key", "hub.toml"} {
		if _, err := os.Stat(filepath.Join(sys, f)); err != nil {
			t.Errorf("join as root did not write %s to the system data dir: %v", f, err)
		}
	}
	if _, err := os.Stat(legacy); err == nil {
		t.Errorf("join as root wrote %s", legacy)
	}
	if fi, err := os.Stat(sys); err != nil {
		t.Error(err)
	} else if fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("system data dir mode %v, want root only", fi.Mode().Perm())
	}

	// The per-user service takes it as it is.
	r = run(sys, "service", "install", "--isolation", "per-user", "--dry-run", "--name", "longarmd-e2e-dd")
	if r.code != 0 || !strings.Contains(r.stdout, "data dir "+sys+"\n") || !strings.Contains(r.stdout, "users dir "+filepath.Join(sys, "users")) || strings.Contains(r.stdout, "would copy") {
		t.Fatalf("service install --isolation per-user --dry-run: %+v", r)
	}
	// A data dir below a 0700 home: refused before anything is written.
	r = run(sys, "service", "install", "--isolation", "per-user", "--dry-run", "--name", "longarmd-e2e-dd", "--data-dir", legacy)
	if r.code == 0 || !strings.Contains(r.stderr, home+" is not traversable") || !strings.Contains(r.stderr, "drop --data-dir") {
		t.Fatalf("per-user below a /root-like home: %+v", r)
	}
	if _, err := os.Stat(legacy); err == nil {
		t.Errorf("the refused install created %s", legacy)
	}

	// Legacy: an older longarmd enrolled as root into ~/.longarm and the
	// system dir is fresh. Shared mode keeps the legacy dir; per-user adopts
	// its enrollment.
	if err := os.MkdirAll(filepath.Join(legacy, "tls"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"tls/ca.crt", "tls/pending.key", "hub.toml"} {
		copyExe(t, filepath.Join(sys, f), filepath.Join(legacy, f))
	}
	sys2 := filepath.Join(base, "longarmd2")
	r = run(sys2, "service", "install", "--dry-run", "--name", "longarmd-e2e-dd")
	if r.code != 0 || !strings.Contains(r.stdout, "data dir "+legacy+"\n") {
		t.Fatalf("shared service, legacy enrollment: %+v", r)
	}
	r = run(sys2, "service", "install", "--isolation", "per-user", "--dry-run", "--name", "longarmd-e2e-dd")
	if r.code != 0 || !strings.Contains(r.stdout, "data dir "+sys2+"\n") || !strings.Contains(r.stdout, "would copy the enrollment from "+legacy+" to "+sys2) {
		t.Fatalf("per-user service, legacy enrollment: %+v", r)
	}
	if _, err := os.Stat(sys2); err == nil {
		t.Errorf("--dry-run created %s", sys2)
	}
}
