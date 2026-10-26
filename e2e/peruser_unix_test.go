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
