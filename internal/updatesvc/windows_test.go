//go:build windows

package updatesvc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	win "github.com/lubobill1990/longarm/internal/platform/windows"
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

// TestPipeServerAndClientChecks: a client tells the service from a
// squatter by the pipe server's PID and owner, never by opening its
// process, and the server identifies the client's user.
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
	checkPipeServerWindows(t, c)
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

// checkPipeServerWindows runs VerifyPipeServer's checks against c, a pipe
// this test process serves.
func checkPipeServerWindows(t *testing.T, c net.Conn) {
	t.Helper()
	me := uint32(os.Getpid())
	const exe = `C:\Program Files\Longarm\longarm-update.exe`
	installed := func() (string, error) { return exe, nil }
	svc := func(sp serviceProcess) func() (serviceProcess, error) {
		return func() (serviceProcess, error) { return sp, nil }
	}
	// The service's PID is this process (the SCM says so): accepted -
	// without opening any process.
	ok := serviceProcess{Running: true, PID: me, Account: "LocalSystem", Command: `"` + exe + `" run`}
	if err := verifyPipeServer(c, pipeServerChecks{service: svc(ok), exe: installed}); err != nil {
		t.Fatalf("the service's own pipe was refused: %v", err)
	}
	// A squatter: the service runs as another process (or not at all).
	for _, sp := range []serviceProcess{
		{Running: true, PID: me + 4, Account: "LocalSystem", Command: ok.Command},
		{Running: false, Account: "LocalSystem", Command: ok.Command},
	} {
		if err := verifyPipeServer(c, pipeServerChecks{service: svc(sp), exe: installed}); !errors.Is(err, ErrImpostor) {
			t.Fatalf("a squatter passed (%+v): %v", sp, err)
		}
	}
	// No such service: refused.
	if err := verifyPipeServer(c, pipeServerChecks{service: func() (serviceProcess, error) {
		sp, err := win.QueryServiceProcess("LongarmNoSuchService")
		return serviceProcess(sp), err
	}, exe: installed}); !errors.Is(err, ErrImpostor) {
		t.Fatalf("no service: %v", err)
	}
	// The pipe's owner: this process's default owner - SYSTEM or
	// Administrators only when elevated (CI); a user's pipe is refused.
	err := pipeOwnerTrusted(windows.Handle(c.(fder).Fd()))
	if want := defaultOwnerTrusted(t); (err == nil) != want {
		t.Fatalf("pipe owner check: %v (default owner trusted: %v)", err, want)
	}
	if err == nil {
		if err := verifyPipeServer(c, pipeServerChecks{owner: pipeOwnerTrusted, service: svc(ok), exe: installed}); err != nil {
			t.Fatal(err)
		}
	} else if err := verifyPipeServer(c, pipeServerChecks{owner: pipeOwnerTrusted, service: svc(ok), exe: installed}); !errors.Is(err, ErrImpostor) {
		t.Fatalf("a user's pipe passed: %v", err)
	}
}

// defaultOwnerTrusted: is this process's default owner for new objects
// SYSTEM or Administrators?
func defaultOwnerTrusted(t *testing.T) bool {
	tok := windows.GetCurrentProcessToken()
	var n uint32
	windows.GetTokenInformation(tok, windows.TokenOwner, nil, 0, &n)
	buf := make([]byte, n)
	if err := windows.GetTokenInformation(tok, windows.TokenOwner, &buf[0], n, &n); err != nil {
		t.Fatal(err)
	}
	return win.SystemOwnerSID((*struct{ Owner *windows.SID })(unsafe.Pointer(&buf[0])).Owner)
}

// TestQueryServiceProcessAsUser: a LocalSystem service's PID, account and
// binary are readable without opening its process (Task Scheduler runs on
// every Windows; a user can't open its svchost).
func TestQueryServiceProcessAsUser(t *testing.T) {
	sp, err := win.QueryServiceProcess("Schedule")
	if err != nil {
		t.Skipf("Schedule: %v", err)
	}
	if !sp.Running || sp.PID == 0 || !localSystemAccount(sp.Account) || !strings.Contains(strings.ToLower(sp.Command), "svchost.exe") {
		t.Fatalf("Schedule: %+v", sp)
	}
	// a squatter claiming to be it is still refused: another PID
	if err := checkPipeServer(uint32(os.Getpid()), serviceProcess(sp), commandExe(sp.Command)); !errors.Is(err, ErrImpostor) {
		t.Fatalf("pid %d passed as %+v: %v", os.Getpid(), sp, err)
	}
	if err := checkPipeServer(sp.PID, serviceProcess(sp), commandExe(sp.Command)); err != nil {
		t.Fatalf("the service's own PID: %v", err)
	}
	if _, err := win.QueryServiceProcess("LongarmNoSuchService"); !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		t.Fatalf("missing service: %v", err)
	}
}
