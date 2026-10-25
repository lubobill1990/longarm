//go:build linux

package linux

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// cgroup is a cgroup v2 directory holding one container's processes:
// memory.max / cpu.max limits, and a kill that catches processes that left
// the process group (setsid).
type cgroup struct {
	dir string
}

// controllers enabled on the way down so leaves can be limited.
var controllers = []string{"memory", "cpu", "pids"}

// newCgroup creates dir (and missing parents, enabling the controllers in
// each parent's cgroup.subtree_control) and applies the limits.
func newCgroup(dir string, memMax uint64, cpus float64) (*cgroup, error) {
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		return nil, errors.New("cgroup v2 is not mounted at /sys/fs/cgroup")
	}
	dir = filepath.Clean(dir)
	if !strings.HasPrefix(dir, "/sys/fs/cgroup/") {
		return nil, fmt.Errorf("cgroup %s is not under /sys/fs/cgroup", dir)
	}
	// Enable the controllers from the top: a parent may only delegate what
	// it has itself. Errors are tolerated (a controller may be unavailable).
	rel := strings.TrimPrefix(dir, "/sys/fs/cgroup/")
	cur := "/sys/fs/cgroup"
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		enable(cur)
		cur = filepath.Join(cur, p)
		if err := os.Mkdir(cur, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("cgroup: %w", err)
		}
		_ = i
	}
	cg := &cgroup{dir: dir}
	if memMax > 0 {
		if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte(strconv.FormatUint(memMax, 10)), 0); err != nil {
			cg.remove()
			return nil, fmt.Errorf("cgroup memory.max: %w", err)
		}
		os.WriteFile(filepath.Join(dir, "memory.swap.max"), []byte("0"), 0) // best effort
	}
	if cpus > 0 {
		const period = 100000
		quota := int64(cpus * period)
		if quota < 1000 {
			quota = 1000
		}
		if err := os.WriteFile(filepath.Join(dir, "cpu.max"), []byte(fmt.Sprintf("%d %d", quota, period)), 0); err != nil {
			cg.remove()
			return nil, fmt.Errorf("cgroup cpu.max: %w", err)
		}
	}
	return cg, nil
}

// enable turns on the controllers available in dir for its children.
func enable(dir string) {
	b, err := os.ReadFile(filepath.Join(dir, "cgroup.controllers"))
	if err != nil {
		return
	}
	have := strings.Fields(string(b))
	on, _ := os.ReadFile(filepath.Join(dir, "cgroup.subtree_control"))
	for _, c := range controllers {
		if contains(have, c) && !bytes.Contains(on, []byte(c)) {
			os.WriteFile(filepath.Join(dir, "cgroup.subtree_control"), []byte("+"+c), 0)
		}
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// open returns an fd of the directory for CLONE_INTO_CGROUP.
func (c *cgroup) open() (*os.File, error) { return os.Open(c.dir) }

// add moves pid into the cgroup (fallback when clone3 is unavailable).
func (c *cgroup) add(pid int) error {
	return os.WriteFile(filepath.Join(c.dir, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0)
}

// pids lists the processes in the cgroup.
func (c *cgroup) pids() []int {
	b, err := os.ReadFile(filepath.Join(c.dir, "cgroup.procs"))
	if err != nil {
		return nil
	}
	var out []int
	for _, f := range strings.Fields(string(b)) {
		if pid, err := strconv.Atoi(f); err == nil {
			out = append(out, pid)
		}
	}
	return out
}

// kill SIGKILLs every process in the cgroup (cgroup.kill on 5.14+).
func (c *cgroup) kill() {
	if err := os.WriteFile(filepath.Join(c.dir, "cgroup.kill"), []byte("1"), 0); err == nil {
		return
	}
	for round := 0; round < 5; round++ {
		pids := c.pids()
		if len(pids) == 0 {
			return
		}
		for _, pid := range pids {
			syscall.Kill(pid, syscall.SIGKILL)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// remove kills what is left and removes the directory (retrying while the
// kernel reaps the processes).
func (c *cgroup) remove() {
	c.kill()
	for i := 0; i < 50; i++ {
		if err := os.Remove(c.dir); err == nil || errors.Is(err, os.ErrNotExist) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

var clone3Once struct {
	sync.Once
	ok bool
}

// clone3Cgroup reports whether the kernel can start a process inside a
// cgroup (clone3 CLONE_INTO_CGROUP, Linux 5.7+); else the process is moved
// right after its start.
func clone3Cgroup() bool {
	clone3Once.Do(func() {
		var u syscall.Utsname
		if syscall.Uname(&u) != nil {
			return
		}
		var b []byte
		for _, c := range u.Release {
			if c == 0 {
				break
			}
			b = append(b, byte(c))
		}
		var major, minor int
		fmt.Sscanf(string(b), "%d.%d", &major, &minor)
		clone3Once.ok = major > 5 || major == 5 && minor >= 7
	})
	return clone3Once.ok
}
