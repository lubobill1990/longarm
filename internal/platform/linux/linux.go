//go:build linux

// Package linux implements internal/platform on Linux: process-group
// containment (+ parent-death signal), /proc identity and metrics, systemd.
package linux

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"syscall"

	"github.com/lubobill1990/longarm/internal/platform/ptypes"
	"github.com/lubobill1990/longarm/internal/platform/unix"
)

// NewContainer returns a process-group container. With KillOnOwnerDeath the
// leader also gets SIGKILL when the shim dies (PR_SET_PDEATHSIG),
// approximating Windows' KILL_ON_JOB_CLOSE for the direct child.
//
// Pdeathsig is tied to the OS thread that forked the child, so Start locks
// the calling goroutine to its thread for good; the shim calls Start from its
// main goroutine, which lives as long as the shim.
func NewContainer(opts ptypes.ContainerOptions) (*unix.Container, error) {
	c := unix.NewContainer(opts)
	c.ListPIDs = listPIDs
	if opts.KillOnOwnerDeath {
		c.PreStart = func(cmd *exec.Cmd) {
			runtime.LockOSThread()
			cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
		}
	}
	return c, nil
}

// procStat returns the fields of /proc/<pid>/stat after the "(comm)" part,
// plus comm. Field numbering follows proc(5): rest[0] is field 3 (state).
func procStat(pid int) (comm string, rest [][]byte, err error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", nil, err
	}
	l, r := bytes.IndexByte(b, '('), bytes.LastIndexByte(b, ')')
	if l < 0 || r < l {
		return "", nil, fmt.Errorf("malformed stat for %d", pid)
	}
	return string(b[l+1 : r]), bytes.Fields(b[r+1:]), nil
}

// ProcessStartTime returns field 22 (starttime, clock ticks since boot).
func ProcessStartTime(pid int) (uint64, error) {
	_, f, err := procStat(pid)
	if err != nil {
		return 0, err
	}
	if len(f) < 20 {
		return 0, fmt.Errorf("short stat for %d", pid)
	}
	return strconv.ParseUint(string(f[19]), 10, 64)
}

// ProcessAlive reports whether pid runs (not a zombie) with the given start time.
func ProcessAlive(pid int, start uint64) bool {
	if !unix.Alive(pid) {
		return false
	}
	_, f, err := procStat(pid)
	if err != nil || len(f) < 20 {
		return false
	}
	if string(f[0]) == "Z" || string(f[0]) == "X" {
		return false
	}
	if start == 0 {
		return true
	}
	st, err := strconv.ParseUint(string(f[19]), 10, 64)
	return err == nil && st == start
}

// listPIDs returns every pid in /proc.
func listPIDs() []int {
	ents, _ := os.ReadDir("/proc")
	var pids []int
	for _, e := range ents {
		if pid, err := strconv.Atoi(e.Name()); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}
