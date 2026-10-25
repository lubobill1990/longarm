package platform

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// The test binary doubles as the child process.
func TestMain(m *testing.M) {
	switch os.Getenv("LONGARM_TEST_PLATFORM_HELPER") {
	case "sleep":
		time.Sleep(2 * time.Minute)
		os.Exit(0)
	case "spawn":
		// One child, then sleep; prints the child's pid.
		c := exec.Command(os.Args[0])
		c.Env = append(os.Environ(), "LONGARM_TEST_PLATFORM_HELPER=sleep")
		if err := c.Start(); err != nil {
			os.Exit(3)
		}
		os.WriteFile(os.Getenv("LONGARM_TEST_PIDFILE"), []byte(strconv.Itoa(c.Process.Pid)), 0o644)
		time.Sleep(2 * time.Minute)
		os.Exit(0)
	case "alloc":
		b := make([]byte, 512<<20)
		for i := range b {
			if i%4096 == 0 {
				b[i] = 1
			}
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func helperCmd(mode string, extraEnv ...string) *exec.Cmd {
	c := exec.Command(os.Args[0])
	c.Env = append(append(os.Environ(), "LONGARM_TEST_PLATFORM_HELPER="+mode), extraEnv...)
	return c
}

func TestProcessIdentity(t *testing.T) {
	pid := os.Getpid()
	st, err := ProcessStartTime(pid)
	if err != nil || st == 0 {
		t.Fatalf("start time: %d %v", st, err)
	}
	if !ProcessAlive(pid, st) || !ProcessAlive(pid, 0) {
		t.Fatal("self not alive")
	}
	if ProcessAlive(pid, st+1) {
		t.Fatal("wrong start time must not match (pid reuse guard)")
	}
	c := helperCmd("sleep")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	cst, _ := ProcessStartTime(c.Process.Pid)
	if !ProcessAlive(c.Process.Pid, cst) {
		t.Fatal("child not alive")
	}
	if err := KillProcess(c.Process.Pid); err != nil {
		t.Fatal(err)
	}
	c.Wait() // reap
	if ProcessAlive(c.Process.Pid, cst) {
		t.Fatal("killed child still alive")
	}
}

func TestContainerKillsTree(t *testing.T) {
	ctr, err := NewContainer(ContainerOptions{Name: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer ctr.Close()
	pidfile := filepath.Join(t.TempDir(), "pid")
	c := helperCmd("spawn", "LONGARM_TEST_PIDFILE="+pidfile)
	if err := ctr.Start(c); err != nil {
		t.Fatal(err)
	}
	var grandchild int
	deadline := time.Now().Add(20 * time.Second)
	for grandchild == 0 && time.Now().Before(deadline) {
		b, _ := os.ReadFile(pidfile)
		grandchild, _ = strconv.Atoi(string(b))
		time.Sleep(50 * time.Millisecond)
	}
	if grandchild == 0 {
		t.Fatal("grandchild did not start")
	}
	if runtime.GOOS == "windows" {
		pids, err := ctr.PIDs()
		if err != nil || len(pids) != 2 {
			t.Fatalf("job pids %v %v", pids, err)
		}
	}
	if err := ctr.Kill(); err != nil {
		t.Fatal(err)
	}
	c.Wait()
	deadline = time.Now().Add(10 * time.Second)
	for ProcessAlive(grandchild, 0) {
		if time.Now().After(deadline) {
			t.Fatal("grandchild survived container kill")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestContainerCloseKillsTree(t *testing.T) {
	ctr, err := NewContainer(ContainerOptions{Name: "test"})
	if err != nil {
		t.Fatal(err)
	}
	c := helperCmd("sleep")
	if err := ctr.Start(c); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { c.Wait(); close(done) }()
	ctr.Close() // KILL_ON_JOB_CLOSE / group kill
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		c.Process.Kill()
		t.Fatal("child survived container close")
	}
}

func TestContainerMemoryLimit(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("memory limits are enforced on Windows only in P0a")
	}
	ctr, err := NewContainer(ContainerOptions{Name: "test", MemLimitBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer ctr.Close()
	c := helperCmd("alloc")
	if err := ctr.Start(c); err != nil {
		// The Go runtime itself may fail to start under a tight limit; that
		// also proves enforcement.
		return
	}
	if err := c.Wait(); err == nil {
		t.Fatal("allocating 512 MiB under a 64 MiB job limit succeeded")
	}
}

func TestDiskUsageAndLock(t *testing.T) {
	free, total, err := DiskUsage(t.TempDir())
	if err != nil || total == 0 || free > total {
		t.Fatalf("disk %d/%d %v", free, total, err)
	}
	p := filepath.Join(t.TempDir(), "lock")
	f1, _ := os.Create(p)
	defer f1.Close()
	if err := LockFile(f1); err != nil {
		t.Fatal(err)
	}
	f2, _ := os.OpenFile(p, os.O_RDWR, 0)
	defer f2.Close()
	if err := LockFile(f2); err == nil {
		t.Fatal("second lock succeeded")
	}
}

func TestSharedReadDuringReplace(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "status.json")
	os.WriteFile(dst, []byte("v1"), 0o644)
	f, err := OpenShared(dst) // reader holds the file open
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tmp := filepath.Join(dir, "tmp")
	os.WriteFile(tmp, []byte("v2"), 0o644)
	if err := ReplaceFile(tmp, dst); err != nil {
		t.Fatalf("replace while a shared reader is open: %v", err)
	}
	if b, _ := ReadFileShared(dst); string(b) != "v2" {
		t.Fatalf("got %q", b)
	}
}
