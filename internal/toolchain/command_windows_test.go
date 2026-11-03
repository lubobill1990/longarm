//go:build windows

package toolchain

import (
	"context"
	"log/slog"
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

func TestWindowsInstallUsesConfiguredNpmPrefix(t *testing.T) {
	dir, prefix, appdata := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("APPDATA", appdata)
	t.Setenv("PATH", strings.Join([]string{prefix, dir, os.Getenv("PATH")}, string(os.PathListSeparator)))
	argsFile := filepath.Join(dir, "args.txt")
	shim := "@echo off\r\nif \"%~1\"==\"prefix\" (\r\n echo " + prefix + "\r\n exit /b 0\r\n)\r\n" +
		"echo %* > \"" + argsFile + "\"\r\n" +
		"echo @echo 9.8.7 > \"" + filepath.Join(prefix, "copilot.cmd") + "\"\r\n"
	if err := os.WriteFile(filepath.Join(dir, "npm.cmd"), []byte(shim), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &System{}
	if err := s.Install(context.Background(), "copilot", "9.8.7", slog.Default()); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil || !strings.Contains(string(args), `"--prefix" "`+prefix+`"`) {
		t.Fatalf("npm ignored configured prefix: %q, %v", args, err)
	}
	if v, err := s.Version(context.Background(), "copilot"); err != nil || v != "9.8.7" {
		t.Fatalf("installed tool not visible: %q, %v", v, err)
	}
}

func TestWindowsUnparseableVersionIsUnknown(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(dir, "copilot.cmd"), []byte("@echo starting up\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&System{}).Version(context.Background(), "copilot"); err == nil {
		t.Fatal("unparseable probe output reported as missing")
	}
}
