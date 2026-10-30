//go:build windows

package updatesvc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

func testPipeName(t *testing.T) string {
	return fmt.Sprintf(`\\.\pipe\LongarmUpdateTest-%d-%d`, os.Getpid(), time.Now().UnixNano())
}

// TestPipeSquatting: the service's pipe is created as the first instance
// only - a name somebody else holds makes ListenPipe fail, it never joins
// the other pipe.
func TestPipeSquatting(t *testing.T) {
	name := testPipeName(t)
	squatter, err := winio.ListenPipe(name, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer squatter.Close()
	if ln, err := listenPipe(name, pipeSDDL); err == nil {
		ln.Close()
		t.Fatal("listenPipe joined a pipe somebody else created")
	}
}

// TestPipeServerAndClientChecks: a client sees that the server of a pipe
// is not the installed service (this test process: another image, not
// SYSTEM) and the server identifies the client's user.
func TestPipeServerAndClientChecks(t *testing.T) {
	name := testPipeName(t)
	me, _ := windows.GetCurrentProcessToken().GetTokenUser()
	// the service runs as SYSTEM (full control): this test's user needs
	// the same to add the instances clients connect to
	ln, err := listenPipe(name, pipeSDDL+"(A;;GA;;;"+me.User.Sid.String()+")")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan Caller, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		caller, err := IdentifyPipeClient(c)
		if err != nil {
			t.Error(err)
		}
		got <- caller
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := winio.DialPipeAccessImpLevel(ctx, name, windows.GENERIC_READ|windows.GENERIC_WRITE, winio.PipeImpLevelIdentification)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var pid uint32
	if err := windows.GetNamedPipeServerProcessId(windows.Handle(c.(fder).Fd()), &pid); err != nil || pid != uint32(os.Getpid()) {
		t.Fatalf("server pid %d %v", pid, err)
	}
	if err := verifyServerProcess(pid, `C:\Program Files\Longarm\longarm-update.exe`); !errors.Is(err, ErrImpostor) {
		t.Fatalf("an impostor passed: %v", err)
	}
	self, _ := os.Executable()
	if err := verifyServerProcess(pid, self); !errors.Is(err, ErrImpostor) && !RunningAsSystem() {
		t.Fatalf("a server not running as SYSTEM passed: %v", err)
	}
	select {
	case caller := <-got:
		if caller.SID != me.User.Sid.String() || caller.System != RunningAsSystem() {
			t.Fatalf("caller %+v, want %s", caller, me.User.Sid)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no caller")
	}
}

// TestDirOpsReparseAndLock: a junction is reported as a reparse point and
// removing it leaves its target alone; a locked file can't be changed or
// deleted.
func TestDirOpsReparseAndLock(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	os.Mkdir(target, 0o700)
	os.WriteFile(filepath.Join(target, "keep"), []byte("x"), 0o600)
	link := filepath.Join(dir, "link")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skipf("mklink /J: %v %s", err, out)
	}
	ops := winDirOps{}
	in, err := ops.Inspect(link)
	if err != nil || !in.Exists || !in.Reparse {
		t.Fatalf("junction: %+v %v", in, err)
	}
	if err := ops.Remove(link); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "keep")); err != nil {
		t.Fatalf("removing the junction removed its target's files: %v", err)
	}
	if in, _ := ops.Inspect(target); in.Secured {
		t.Fatal("a temp dir reported SYSTEM-only")
	}

	f := filepath.Join(dir, "pkg.msi")
	os.WriteFile(f, []byte("package"), 0o600)
	l, err := ops.Lock(f)
	if err != nil {
		t.Fatal(err)
	}
	if w, err := os.OpenFile(f, os.O_WRONLY, 0); err == nil {
		w.Close()
		t.Fatal("a locked package was opened for writing")
	}
	if err := os.Remove(f); err == nil {
		t.Fatal("a locked package was deleted")
	}
	if err := os.Rename(f, f+".x"); err == nil {
		t.Fatal("a locked package was renamed")
	}
	l.Close()
	if err := os.Remove(f); err != nil {
		t.Fatal(err)
	}
	// a hard link is refused (another name could change it)
	os.WriteFile(f, []byte("package"), 0o600)
	if err := os.Link(f, f+".hard"); err == nil {
		if _, err := ops.Lock(f); err == nil {
			t.Fatal("a hard-linked package was locked")
		}
	}
}
