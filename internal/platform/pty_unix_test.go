//go:build linux || darwin

package platform

import (
	"io"
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// Killing a PTY container also kills background jobs of an interactive
// shell, which job control puts into their own process groups.
func TestPTYKillsBackgroundJobs(t *testing.T) {
	ctr, err := NewContainer(ContainerOptions{Name: "pty-bg"})
	if err != nil {
		t.Fatal(err)
	}
	defer ctr.Close()
	p, err := StartPTY(ctr, PTYOptions{Argv: []string{"/bin/sh", "-i"}, Env: os.Environ()})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release()
	out := readAll(p)
	io.WriteString(p, "set -m; sleep 300 & echo bgpid=$!\r")
	re := regexp.MustCompile(`bgpid=(\d+)`)
	var pid int
	deadline := time.Now().Add(15 * time.Second)
	for pid == 0 {
		if m := re.FindStringSubmatch(out.String()); m != nil {
			pid, _ = strconv.Atoi(m[1])
		}
		if time.Now().After(deadline) {
			t.Fatalf("no bgpid in %q", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := ctr.Kill(); err != nil {
		t.Fatal(err)
	}
	p.Wait()
	deadline = time.Now().Add(5 * time.Second)
	for ProcessAlive(pid, 0) {
		if time.Now().After(deadline) {
			t.Fatalf("background job %d survived the kill", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
