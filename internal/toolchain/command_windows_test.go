//go:build windows

package toolchain

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsVersionUsesJobPathAndBatchShim(t *testing.T) {
	appdata := t.TempDir()
	t.Setenv("APPDATA", appdata)
	t.Setenv("PATH", os.Getenv("PATH"))
	dir := filepath.Join(appdata, "npm")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Use a baseline npm tool so Version exercises the same lookup as a job.
	if err := os.WriteFile(filepath.Join(dir, "copilot.cmd"), []byte("@echo 9.8.7\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Prepend the test directory so a real installed Copilot cannot shadow it.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	v, err := (&System{}).Version(context.Background(), "copilot")
	if err != nil || v != "9.8.7" {
		t.Fatalf("batch version %q: %v", v, err)
	}
	p, err := exec.LookPath("copilot")
	if err != nil || !strings.EqualFold(p, filepath.Join(dir, "copilot.cmd")) {
		t.Fatalf("job lookup %q: %v", p, err)
	}
}

func TestWindowsRefreshAddsNpmForNextJob(t *testing.T) {
	t.Setenv("APPDATA", t.TempDir())
	t.Setenv("PATH", os.Getenv("PATH"))
	if err := refreshPath(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(os.Getenv("PATH"), filepath.Join(os.Getenv("APPDATA"), "npm")) {
		t.Fatal("next job would not find npm-installed tools")
	}
	first := os.Getenv("PATH")
	if err := refreshPath(); err != nil || os.Getenv("PATH") != first {
		t.Fatalf("refresh is not idempotent: %v", err)
	}
}
