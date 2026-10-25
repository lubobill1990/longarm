//go:build windows

package e2e

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/platform"
	win "github.com/lubobill1990/longarm/internal/platform/windows"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return string(out)
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	out.Close()
}

// tempServiceUser creates a local account for a service test and removes it
// (its profile, logon right and the account) at cleanup. Register it before
// anything that installs a service as this user: cleanups run in reverse,
// so the service is gone before the profile is deleted.
func tempServiceUser(t *testing.T, prefix string) (user, pass string) {
	t.Helper()
	user = prefix + randHex(3)
	pass = "Rw!" + randHex(4) + "aZ9" // 14 chars: longer makes net.exe prompt
	run(t, "net", "user", user, pass, "/add", "/comment:longarm test (temporary)")
	t.Cleanup(func() {
		ps := fmt.Sprintf(`$p = Get-CimInstance Win32_UserProfile | Where-Object { $_.LocalPath -like '*\%s' }
if (-not $p) { exit 0 }
if ($p.Loaded) { exit 3 }
$p | Remove-CimInstance -ErrorAction Stop`, user)
		removed := false
		for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline); time.Sleep(3 * time.Second) {
			if exec.Command("pwsh", "-NoProfile", "-Command", ps).Run() == nil {
				removed = true
				break
			}
		}
		if !removed {
			t.Logf("profile C:\\Users\\%s is still loaded by Windows; remove it later with: "+
				"Get-CimInstance Win32_UserProfile | ? LocalPath -like '*\\%s' | Remove-CimInstance", user, user)
		}
		if err := win.RevokeServiceLogonRight(user); err != nil {
			t.Logf("revoke logon right: %v", err)
		}
		exec.Command("net", "user", user, "/delete").Run()
	})
	return user, pass
}

// TestWindowsService installs longarmd as an SCM service logging on as a freshly
// created local user, checks jobs run as that user, stops the service
// mid-job (the job must survive), starts it again (the job is re-adopted),
// then uninstalls everything and deletes the user.
//
// It creates a local account, so it only runs with LONGARM_SERVICE_TEST=1 in
// an elevated shell.
func TestWindowsService(t *testing.T) {
	if os.Getenv("LONGARM_SERVICE_TEST") != "1" {
		t.Skip("set LONGARM_SERVICE_TEST=1 (elevated) to run the SCM service test")
	}
	if !platform.Elevated() {
		t.Skip("needs an elevated (administrator) process")
	}
	suffix := randHex(3)
	svcName := "longarmd-test-" + suffix
	base := filepath.Join(`C:\longarm-e2e`, "svctest-"+suffix)
	t.Cleanup(func() { os.RemoveAll(base) }) // registered first => runs last
	if err := os.MkdirAll(filepath.Join(base, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}

	user, pass := tempServiceUser(t, "lasvc")
	t.Cleanup(func() { exec.Command(bins.longarmd, "service", "uninstall", "--name", svcName).Run() })
	// The service account needs to read the binaries and own its data dir.
	longarmdExe, helperExe := filepath.Join(base, "bin", "longarmd.exe"), filepath.Join(base, "bin", "helper.exe")
	copyFile(t, bins.longarmd, longarmdExe)
	copyFile(t, bins.helper, helperExe)
	run(t, "icacls", base, "/grant", user+":(OI)(CI)F", "/q")

	e := newEnvAt(t, base)
	cmd := exec.Command(longarmdExe, "service", "install", "--name", svcName, "--user", `.\`+user,
		"--data-dir", e.dataDir, "--listen", fmt.Sprintf("tcp://127.0.0.1:%d", e.port))
	cmd.Env = append(os.Environ(), "LONGARM_SERVICE_PASSWORD="+pass)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if out := run(t, longarmdExe, "service", "status", "--name", svcName); !strings.Contains(out, "stopped") {
		t.Fatalf("status after install: %s", out)
	}
	sc := run(t, "sc.exe", "qc", svcName)
	if !strings.Contains(sc, `.\`+user) || !strings.Contains(sc, "AUTO_START") {
		t.Fatalf("sc qc:\n%s", sc)
	}

	endpoints := filepath.Join(e.dataDir, "longarmd.endpoints")
	waitListening := func() {
		waitFor(t, "service to listen", 60*time.Second, func() bool { _, err := os.Stat(endpoints); return err == nil })
	}
	run(t, longarmdExe, "service", "start", "--name", svcName)
	waitListening()

	// Commands run as the service account, not as the installing admin.
	who := strings.ToLower(strings.TrimSpace(e.mustLongarm(e.client, "exec", "w1", "--", "whoami")))
	if !strings.HasSuffix(who, `\`+user) {
		t.Fatalf("whoami = %q, want *\\%s", who, user)
	}
	t.Logf("service runs as %s", who)
	// The built-in sshd of a console-less service: exec + pty through longarm
	// proxy, and a ConPTY session through longarm attach's API.
	checkSSHExecPTY(t, bins.longarm, e.client, helperExe)
	a := attachGo(t, e.dial(t), &pb.AttachOpen{Name: "svc", Argv: []string{helperExe, "repl"}})
	a.waitFor("repl ready")
	a.send("exit 3\r")
	a.waitEnd()
	if !a.exited || a.exit != 3 {
		t.Fatalf("service session exit %v %d:\n%s", a.exited, a.exit, plain(a.out.String()))
	}

	id := strings.TrimSpace(e.mustLongarm(e.client, "run", "w1", "--", helperExe, "print", "30", "150", "6"))
	logPath := filepath.Join(e.dataDir, "jobs", id, "out.log")
	size := func() int64 {
		fi, err := os.Stat(logPath)
		if err != nil {
			return 0
		}
		return fi.Size()
	}
	waitFor(t, "job output", 20*time.Second, func() bool { return size() > 0 })

	run(t, longarmdExe, "service", "stop", "--name", svcName)
	if _, err := os.Stat(endpoints); err == nil {
		t.Fatal("endpoints file still present after stop")
	}
	before := size()
	time.Sleep(1200 * time.Millisecond)
	if size() <= before {
		t.Fatalf("job stopped producing output when the service stopped (%d -> %d)", before, size())
	}

	run(t, longarmdExe, "service", "start", "--name", svcName)
	waitListening()
	if r := e.longarm(e.client, "wait", "w1", id); r.code != 6 {
		t.Fatalf("wait after service restart: %+v", r)
	}
	if got := stripErr(e.mustLongarm(e.client, "logs", "w1", id)); got != lines(1, 30) {
		t.Fatalf("log after restart:\n%s", got)
	}
	st, err := jobs.Dir(filepath.Join(e.dataDir, "jobs", id)).ReadStatus()
	if err != nil || st.State != jobs.Failed || *st.ExitCode != 6 {
		t.Fatalf("status %+v %v", st, err)
	}

	run(t, longarmdExe, "service", "uninstall", "--name", svcName)
	if out, _ := exec.Command(longarmdExe, "service", "status", "--name", svcName).CombinedOutput(); !strings.Contains(string(out), "not-installed") {
		t.Fatalf("status after uninstall: %s", out)
	}
}
