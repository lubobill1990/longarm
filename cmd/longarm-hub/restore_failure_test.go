package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeCgroup points cgroupRoot / procMeminfo at files written by the test.
func fakeCgroup(t *testing.T, files map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oldRoot, oldMem := cgroupRoot, procMeminfo
	cgroupRoot, procMeminfo = filepath.Join(dir, "cgroup"), filepath.Join(dir, "meminfo")
	t.Cleanup(func() { cgroupRoot, procMeminfo = oldRoot, oldMem })
}

// TestMemoryNote: what a SIGKILLed restore is reported as - the container's
// memory from its cgroup (v2, v1, none: the machine's), and certainty when
// the cgroup counted an OOM kill during the restore.
func TestMemoryNote(t *testing.T) {
	fakeCgroup(t, map[string]string{
		"cgroup/memory.max":    "1610612736\n",
		"cgroup/memory.events": "low 0\nhigh 0\nmax 12\noom 1\noom_kill 1\n",
	})
	if limit, oom, ok := cgroupMemory(); !ok || limit != 1536<<20 || oom != 1 {
		t.Fatalf("cgroup v2: %d %d %v", limit, oom, ok)
	}
	if got, want := memoryNote(0), "out of memory: the kernel's OOM killer ended it; the container has 1536 MB"; got != want {
		t.Errorf("an OOM kill during the restore: %q, want %q", got, want)
	}
	if got, want := memoryNote(1), "out of memory? the container has 1536 MB"; got != want {
		t.Errorf("no OOM kill counted: %q, want %q", got, want)
	}

	// cgroup v1 without a limit: the machine's memory
	fakeCgroup(t, map[string]string{
		"cgroup/memory/memory.limit_in_bytes": "9223372036854771712\n",
		"cgroup/memory/memory.oom_control":    "oom_kill_disable 0\nunder_oom 0\noom_kill 0\n",
		"meminfo":                             "MemTotal:        8000000 kB\nMemFree:         1000 kB\n",
	})
	if limit, oom, ok := cgroupMemory(); !ok || limit != 0 || oom != 0 {
		t.Fatalf("cgroup v1: %d %d %v", limit, oom, ok)
	}
	if got, want := memoryNote(0), "out of memory? the machine has 7812 MB"; got != want {
		t.Errorf("cgroup v1, no limit: %q, want %q", got, want)
	}

	// nothing known
	fakeCgroup(t, nil)
	if got := memoryNote(-1); got != "out of memory?" {
		t.Errorf("no cgroup: %q", got)
	}
}

// fakeLitestream writes a shell script standing in for litestream.
func fakeLitestream(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("a shell script as litestream; the container entrypoint runs on Linux")
	}
	p := filepath.Join(t.TempDir(), "litestream")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestRunRestoreKilled: a restore the OOM killer ends (SIGKILL; the
// 2026-10-29 incident: "exit 1" and nothing else) is reported as such, with
// the container's memory, and the entrypoint exits 137 like a shell would.
func TestRunRestoreKilled(t *testing.T) {
	ls := fakeLitestream(t, `kill -KILL $$`)
	fakeCgroup(t, map[string]string{"cgroup/memory.max": "1610612736\n"})
	err := runRestore(context.Background(), nil, "restore", 0, ls, "litestream.yml", filepath.Join(t.TempDir(), "hub.db"))
	var rf *restoreFailure
	if !errors.As(err, &rf) {
		t.Fatalf("not a restoreFailure: %T %v", err, err)
	}
	if want := "litestream restore killed by SIGKILL (out of memory? the container has 1536 MB)"; rf.Error() != want || rf.ExitCode() != 137 {
		t.Fatalf("%q (exit %d), want %q (exit 137)", rf.Error(), rf.ExitCode(), want)
	}

	// another signal: no memory note
	ls = fakeLitestream(t, `kill -TERM $$`)
	err = runRestore(context.Background(), nil, "restore", 0, ls, "litestream.yml", filepath.Join(t.TempDir(), "hub.db"))
	if !errors.As(err, &rf) || rf.Error() != "litestream restore killed by SIGTERM" || rf.ExitCode() != 143 {
		t.Fatalf("SIGTERM: %v", err)
	}

	// an error of Litestream's own: its status, exit 1
	ls = fakeLitestream(t, `echo "cannot calc restore plan" >&2; exit 3`)
	err = runRestore(context.Background(), nil, "restore", 0, ls, "litestream.yml", filepath.Join(t.TempDir(), "hub.db"))
	if !errors.As(err, &rf) || !strings.Contains(rf.Error(), "litestream restore exited with status 3") || rf.ExitCode() != 1 {
		t.Fatalf("exit 3: %v", err)
	}
}

// TestRecordStartFailure: with a diagnostics container the failure outlives
// the container's log, as our own description only.
func TestRecordStartFailure(t *testing.T) {
	dir := t.TempDir()
	u := filepath.ToSlash(dir)
	if !strings.HasPrefix(u, "/") {
		u = "/" + u // file:///C:/...
	}
	t.Setenv("LONGARM_HUB_DIAG_BLOB_URL", "file://"+u)
	recordStartFailure("/var/lib/longarm-hub/hub.db", &restoreFailure{msg: "litestream restore killed by SIGKILL (out of memory? the container has 1536 MB)", code: 137})
	var found []string
	filepath.Walk(filepath.Join(dir, "hub-start-failures"), func(p string, fi os.FileInfo, err error) error {
		if err == nil && strings.HasSuffix(p, ".txt") {
			found = append(found, p)
		}
		return nil
	})
	if len(found) != 1 {
		t.Fatalf("records: %v", found)
	}
	b, err := os.ReadFile(found[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"phase: restore of /var/lib/longarm-hub/hub.db", "exit status: 137", "error: litestream restore killed by SIGKILL"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("record lacks %q:\n%s", want, b)
		}
	}
	// other errors are not recorded (their text is not ours)
	recordStartFailure("x", errors.New("hub.toml: secret = ..."))
	n := 0
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && strings.HasSuffix(p, ".txt") {
			n++
		}
		return nil
	})
	if n != 1 {
		t.Fatalf("%d records after a non-restore error", n)
	}
}
