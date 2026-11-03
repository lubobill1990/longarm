//go:build windows

package toolchain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
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

func TestWindowsDisabledLeavesPathAndToolsAlone(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("APPDATA", dir)
	t.Setenv("LOCALAPPDATA", dir)
	t.Setenv("PATH", os.Getenv("PATH"))
	before := os.Getenv("PATH")
	r := &Reconciler{Backend: &System{}, Disabled: true}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r.Run(ctx)
	r.refreshInventory(context.Background())
	if os.Getenv("PATH") != before || len(r.Status().Versions) != 0 {
		t.Fatal("disabled reconciler ran probes or changed the job PATH")
	}
}

func TestToolchainProbeHelper(t *testing.T) {
	if os.Getenv("LONGARM_TOOLCHAIN_PROBE_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	if err := os.WriteFile(os.Getenv("LONGARM_TOOLCHAIN_PROBE_PID"), []byte(fmt.Sprint(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Minute)
}

func TestWindowsProbeCancellationKillsDescendants(t *testing.T) {
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(dir, "pid")
	t.Setenv("LONGARM_TOOLCHAIN_PROBE_HELPER", "1")
	t.Setenv("LONGARM_TOOLCHAIN_PROBE_PID", pidFile)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	shim := "@echo off\r\n\"" + exe + "\" \"-test.run=^TestToolchainProbeHelper$\"\r\n"
	if err := os.WriteFile(filepath.Join(dir, "copilot.cmd"), []byte(shim), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := (&System{}).Version(ctx, "copilot")
		done <- err
	}()
	deadline := time.Now().Add(20 * time.Second)
	var pid uint32
	for {
		b, err := os.ReadFile(pidFile)
		if err == nil && len(b) > 0 {
			if _, err := fmt.Sscan(string(b), &pid); err != nil {
				t.Fatal(err)
			}
			break
		}
		if (err != nil && !errors.Is(err, os.ErrNotExist)) || time.Now().After(deadline) {
			t.Fatalf("probe child did not start: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	process, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(process)
	defer windows.TerminateProcess(process, 1)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled probe succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("probe did not return after cancellation")
	}
	if state, err := windows.WaitForSingleObject(process, 5000); err != nil || state != windows.WAIT_OBJECT_0 {
		t.Fatalf("orphaned probe child: wait=%d, err=%v", state, err)
	}
}
