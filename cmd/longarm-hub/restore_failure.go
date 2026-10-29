package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lubobill1990/longarm/internal/diag"
	"github.com/lubobill1990/longarm/internal/hub"
	"github.com/lubobill1990/longarm/internal/version"
)

// restoreFailure is a failed `litestream restore`, described for the log
// (the container's log is all an operator has: ACI drops it on a restart).
type restoreFailure struct {
	msg  string
	code int // the entrypoint's exit status: 128+signal when Litestream was killed
	err  error
}

func (f *restoreFailure) Error() string { return f.msg }
func (f *restoreFailure) Unwrap() error { return f.err }

// ExitCode: main exits with it, so the container's exit code (the only
// thing ACI keeps of a crashed start: previousState.exitCode) says a
// Litestream killed by SIGKILL (137) rather than a generic 1.
func (f *restoreFailure) ExitCode() int { return f.code }

// describeRestoreExit describes the error of the restore's Wait.
// oomBefore: the cgroup's OOM kill count before the restore started.
func describeRestoreExit(err error, oomBefore int64) *restoreFailure {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return &restoreFailure{msg: "litestream restore: " + err.Error(), code: 1, err: err}
	}
	if sig, ok := exitSignal(ee.ProcessState); ok {
		msg := "litestream restore killed by " + signalName(sig)
		if sig == sigKill {
			msg += " (" + memoryNote(oomBefore) + ")"
		}
		return &restoreFailure{msg: msg, code: 128 + sig, err: err}
	}
	return &restoreFailure{msg: fmt.Sprintf("litestream restore exited with status %d (its messages are above)", ee.ExitCode()), code: 1, err: err}
}

// memoryNote: what a SIGKILL during the restore most likely was. The
// restore opens every file of its plan at once, so its memory grows with the
// number of files since the last snapshot (deploy/azure/README.md "Replica
// size and memory").
func memoryNote(oomBefore int64) string {
	limit, oom, ok := cgroupMemory()
	size := ""
	switch {
	case ok && limit > 0:
		size = fmt.Sprintf("the container has %d MB", limit>>20)
	default:
		if total := memTotal(); total > 0 {
			size = fmt.Sprintf("the machine has %d MB", total>>20)
		}
	}
	if ok && oom > oomBefore {
		if size == "" {
			return "out of memory: the kernel's OOM killer ended it"
		}
		return "out of memory: the kernel's OOM killer ended it; " + size
	}
	if size == "" {
		return "out of memory?"
	}
	return "out of memory? " + size
}

// cgroupRoot is this container's cgroup (a variable for the tests).
var cgroupRoot = "/sys/fs/cgroup"

// unlimited: cgroup v1 reports "no limit" as a huge page-aligned number
const unlimited = int64(1) << 60

// cgroupMemory reads the container's memory limit (0 = none) and how many
// processes the OOM killer has ended in it; ok = a cgroup was found.
// cgroup v2 (memory.max, memory.events), else v1 (memory/).
func cgroupMemory() (limit, oomKills int64, ok bool) {
	readInt := func(p string) (int64, bool) {
		b, err := os.ReadFile(p)
		if err != nil {
			return 0, false
		}
		s := strings.TrimSpace(string(b))
		if s == "max" {
			return 0, true
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, false
		}
		if n >= unlimited {
			n = 0
		}
		return n, true
	}
	if l, found := readInt(filepath.Join(cgroupRoot, "memory.max")); found {
		return l, keyValue(filepath.Join(cgroupRoot, "memory.events"), "oom_kill"), true
	}
	v1 := filepath.Join(cgroupRoot, "memory")
	if l, found := readInt(filepath.Join(v1, "memory.limit_in_bytes")); found {
		return l, keyValue(filepath.Join(v1, "memory.oom_control"), "oom_kill"), true
	}
	return 0, 0, false
}

// keyValue: the number after key in a "key value" per line file (-1: none).
func keyValue(path, key string) int64 {
	f, err := os.Open(path)
	if err != nil {
		return -1
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, _ := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		if k == key {
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err == nil {
				return n
			}
		}
	}
	return -1
}

// procMeminfo: /proc/meminfo (a variable for the tests).
var procMeminfo = "/proc/meminfo"

// memTotal: MemTotal of /proc/meminfo in bytes (0: unknown).
func memTotal() int64 {
	f, err := os.Open(procMeminfo)
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "MemTotal:"); ok {
			kb, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(rest), " kB"), 10, 64)
			if err == nil {
				return kb << 10
			}
		}
	}
	return 0
}

// startFailurePrefix: where recordStartFailure writes in the diagnostics
// container (deploy/azure enableDiagnostics), one blob per failed start;
// the container's lifecycle rule deletes them with the bundles.
const startFailurePrefix = "hub-start-failures/"

// recordStartFailure keeps a failed restore beyond the container's log,
// which ACI drops when it restarts the container: a small text blob in the
// diagnostics container ($LONGARM_HUB_DIAG_BLOB_URL, written with the
// managed identity like the bundles), when the hub has one. Only our own
// description (exit status, signal, memory), never Litestream's output.
// Best effort: at most 20 s, a failure is only logged.
func recordStartFailure(db string, err error) {
	u := strings.TrimSpace(os.Getenv(hub.EnvDiagBlobURL))
	if u == "" {
		return
	}
	var rf *restoreFailure
	if !errors.As(err, &rf) {
		return
	}
	st, oerr := diag.OpenStore(u, nil)
	if oerr != nil {
		fmt.Fprintf(os.Stderr, "longarm-hub container: not recording the failure: %v\n", oerr)
		return
	}
	if bs, ok := st.(*diag.BlobStore); ok {
		bs.ContentType = "text/plain; charset=utf-8"
	}
	now := time.Now().UTC()
	host, _ := os.Hostname()
	text := fmt.Sprintf("time: %s\nversion: %s\nhost: %s\nphase: restore of %s\nexit status: %d\nerror: %s\n",
		now.Format(time.RFC3339), version.Version, host, db, rf.code, rf.msg)
	name := startFailurePrefix + now.Format("2006/01/02/150405") + "-" + strconv.Itoa(os.Getpid()) + ".txt"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if perr := st.Put(ctx, name, strings.NewReader(text), int64(len(text)), map[string]string{"kind": "hub-start-failure"}); perr != nil {
		fmt.Fprintf(os.Stderr, "longarm-hub container: recording the failure in %s: %v\n", st.Location(), perr)
		return
	}
	fmt.Fprintf(os.Stderr, "longarm-hub container: failure recorded as %s in %s\n", name, st.Location())
}
