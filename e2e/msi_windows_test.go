//go:build windows

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"

	"github.com/lubobill1990/longarm/internal/platform"
)

// TestMSI (opt-in: LONGARM_MSI_TEST=1, elevated, WiX v5 installed): build
// the MSI; a silent worker install without a service account is refused
// (no silent LocalSystem); install it silently as a worker running as a
// temporary local user that joins a local hub, check the service account
// (jobs run as that user), enrollment, firewall rules, data dir ACL and
// PATH; a wrong password fails the install (rolled back); uninstall and
// check that nothing is left (service, files, data, rules, registry) and
// that the worker left the hub. Then the hub's install.ps1 with an explicit
// LocalSystem account.
func TestMSI(t *testing.T) {
	if os.Getenv("LONGARM_MSI_TEST") != "1" {
		t.Skip("set LONGARM_MSI_TEST=1 (elevated; installs and removes an MSI)")
	}
	if !platform.Elevated() {
		t.Fatal("needs an elevated process")
	}
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `Software\Longarm`, registry.READ); err == nil {
		k.Close()
		t.Fatal("Longarm is already installed on this machine; not touching it")
	}
	root := t.TempDir()
	// Build the MSI from the e2e binaries.
	bin := filepath.Join(root, "bin")
	os.MkdirAll(bin, 0o755)
	for _, b := range []string{bins.longarm, bins.longarmd} {
		data, _ := os.ReadFile(b)
		os.WriteFile(filepath.Join(bin, filepath.Base(b)), data, 0o755)
	}
	msi := filepath.Join(root, "longarm-test.msi")
	if out, err := exec.Command("pwsh", "-NoProfile", "-File", "../packaging/windows/build-msi.ps1",
		"-BinDir", bin, "-Version", "0.9.1", "-Arch", "x64", "-Out", msi).CombinedOutput(); err != nil {
		t.Fatalf("build-msi: %v\n%s", err, out)
	}

	h := newHubEnv(t, "")
	code := strings.TrimSpace(h.mustLongarm(h.admin, "admin", "join-code", "--role", "worker", "--name", "msiw"))
	svc := "longarmd-msitest"
	// The service account must read the binaries: not under this user's
	// temp dir. No spaces: msiexec parses its own command line.
	base := filepath.Join(`C:\longarm-e2e`, "msitest-"+randHex(3))
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) }) // registered first => runs last
	user, pass := tempServiceUser(t, "lamsi")
	run(t, "icacls", base, "/grant", user+":(OI)(CI)RX", "/q")
	installDir := filepath.Join(base, "pf", "longarm")
	dataDir := filepath.Join(base, "data")
	port := freePort(t)
	msiLog := filepath.Join(root, "install.log")
	uninstalled := false
	uninstall := func() {
		if uninstalled {
			return
		}
		uninstalled = true
		out, err := exec.Command("msiexec", "/x", msi, "/qn", "/norestart", "/l*v", filepath.Join(root, "uninstall.log")).CombinedOutput()
		if err != nil {
			t.Errorf("msiexec /x: %v %s", err, out)
		}
	}
	t.Cleanup(func() {
		uninstall()
		if t.Failed() {
			b, _ := os.ReadFile(filepath.Join(os.TempDir(), "longarm-install.log"))
			t.Logf("install-helper log:\n%s", b)
			b, _ = os.ReadFile(filepath.Join(dataDir, "longarmd.log"))
			t.Logf("longarmd.log:\n%s", tail(string(b), 4000))
		}
		exec.Command("sc.exe", "delete", svc).Run() // in case the MSI left it
	})
	args := []string{"/i", msi, "/qn", "/norestart", "/l*v", msiLog,
		"ROLES=worker", "HUB=" + h.addr(), "JOIN_CODE=" + code, "CA_FINGERPRINT=" + h.fp,
		"SERVICE_NAME=" + svc, "INSTALLFOLDER=" + installDir + `\`, "DATADIR=" + dataDir,
		fmt.Sprintf("LISTEN=tcp://127.0.0.1:%d", port)}
	msiexec := func(extra ...string) error {
		ictx, icancel := context.WithTimeout(context.Background(), 5*time.Minute) // a bad command line opens a dialog
		defer icancel()
		out, err := exec.CommandContext(ictx, "msiexec", append(append([]string(nil), args...), extra...)...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v %s", err, out)
		}
		return nil
	}
	notInstalled := func(what string) {
		t.Helper()
		if st, _ := platform.QueryService(svc); st != platform.ServiceNotInstalled {
			t.Fatalf("%s: service installed (%v)", what, st)
		}
		if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `Software\Longarm`, registry.READ); err == nil {
			k.Close()
			t.Fatalf("%s: Longarm installed", what)
		}
	}

	// 0. A silent worker install must name the service account: no silent
	// fallback to LocalSystem (jobs would run as SYSTEM).
	if err := msiexec(); err == nil {
		t.Fatal("worker install without SERVICE_USER/SERVICE_PASSWORD succeeded")
	}
	if b, _ := os.ReadFile(msiLog); !strings.Contains(utf16ToString(b), "SERVICE_ACCOUNT=LocalSystem") {
		t.Fatalf("no account error in the MSI log:\n%s", tail(utf16ToString(b), 3000))
	}
	notInstalled("without an account")
	// A wrong password: the service can't log on, the install fails and is
	// rolled back, enrollment included.
	args[8] = "JOIN_CODE=" + strings.TrimSpace(h.mustLongarm(h.admin, "admin", "join-code", "--role", "worker", "--name", "msibad"))
	if err := msiexec("SERVICE_USER=.\\"+user, "SERVICE_PASSWORD=wrong-"+pass); err == nil {
		t.Fatal("worker install with a wrong password succeeded")
	}
	notInstalled("with a wrong password")
	for _, f := range []string{"tls", "hub.toml"} {
		if _, err := os.Stat(filepath.Join(dataDir, f)); err == nil {
			t.Fatalf("%s left in the data dir after the rollback", f)
		}
	}

	// 1. The worker, running as the temporary user.
	args[8] = "JOIN_CODE=" + code
	start := time.Now()
	if err := msiexec("SERVICE_USER=.\\"+user, "SERVICE_PASSWORD="+pass); err != nil {
		b, _ := os.ReadFile(msiLog)
		t.Fatalf("msiexec /i: %v\n%s", err, tail(utf16ToString(b), 6000))
	}
	t.Logf("installed in %s", time.Since(start).Round(time.Second))
	// No secret in the verbose MSI log (Hidden properties + HideTarget on the
	// custom actions that carry them).
	noSecretsInLog(t, msiLog, code, pass, "wrong-"+pass)
	if sc := run(t, "sc.exe", "qc", svc); !strings.Contains(strings.ToLower(sc), strings.ToLower(user)) {
		t.Fatalf("service account:\n%s", sc)
	}

	// Service installed and running (it waits for the approval).
	waitFor(t, "service running", 30*time.Second, func() bool {
		st, err := platform.QueryService(svc)
		return err == nil && st == platform.ServiceRunning
	})
	h.waitNode(t, "msiw", "pending", func(n map[string]any) bool { return n["state"] == "pending" })
	h.mustLongarm(h.admin, "admin", "approve", "msiw")
	h.waitNode(t, "msiw", "online", func(n map[string]any) bool { return isTrue(n["online"]) })
	if r := h.longarm(h.client, "exec", "msiw", "--", "cmd", "/c", "exit", "4"); r.code != 4 {
		t.Fatalf("exec on the MSI worker: %+v", r)
	}
	// Jobs run as the service account (DESIGN §3.1), not as SYSTEM.
	if who := strings.ToLower(strings.TrimSpace(h.mustLongarm(h.client, "exec", "msiw", "--", "whoami"))); !strings.HasSuffix(who, `\`+strings.ToLower(user)) {
		t.Fatalf("whoami on the MSI worker = %q, want *\\%s", who, user)
	}
	// Firewall rules for the installed binaries.
	for _, exe := range []string{"longarmd.exe", "longarm.exe"} {
		if ok, err := platform.HasFirewallAllowRule(filepath.Join(installDir, exe)); err != nil || !ok {
			t.Fatalf("no firewall rule for %s: %v", exe, err)
		}
	}
	// Data dir: protected DACL, no access for Users.
	acl, _ := exec.Command("icacls", dataDir).CombinedOutput()
	if strings.Contains(string(acl), "BUILTIN\\Users") || !strings.Contains(string(acl), "NT AUTHORITY\\SYSTEM") ||
		!strings.Contains(strings.ToLower(string(acl)), strings.ToLower(user)) {
		t.Fatalf("data dir ACL:\n%s", acl)
	}
	// PATH (machine) contains the install dir.
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`, registry.READ); err == nil {
		p, _, _ := k.GetStringValue("Path")
		k.Close()
		if !strings.Contains(strings.ToLower(p), strings.ToLower(installDir)) {
			t.Fatalf("PATH lacks %s", installDir)
		}
	}

	// Uninstall: service, files, data, firewall rules, registry gone; the
	// worker left the hub.
	uninstall()
	if st, _ := platform.QueryService(svc); st != platform.ServiceNotInstalled {
		t.Fatalf("service still there: %v", st)
	}
	for _, p := range []string{installDir, dataDir} {
		if _, err := os.Stat(p); err == nil {
			ents, _ := os.ReadDir(p)
			t.Fatalf("%s left behind (%d entries)", p, len(ents))
		}
	}
	if ok, _ := platform.HasFirewallAllowRule(filepath.Join(installDir, "longarmd.exe")); ok {
		t.Fatal("firewall rule left behind")
	}
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `Software\Longarm`, registry.READ); err == nil {
		k.Close()
		t.Fatal(`HKLM\Software\Longarm left behind`)
	}
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`, registry.READ); err == nil {
		p, _, _ := k.GetStringValue("Path")
		_, _, derr := k.GetStringValue("LONGARM_DATA_DIR")
		k.Close()
		if strings.Contains(strings.ToLower(p), strings.ToLower(installDir)) || derr == nil {
			t.Fatal("PATH / LONGARM_DATA_DIR left behind")
		}
	}
	h.waitNode(t, "msiw", "left (revoked)", func(n map[string]any) bool { return n["state"] == "revoked" })

	// 2. The hub's install.ps1, which prefers the MSI when the release has
	// one (elevated): download, sha256, msiexec /qn; here with the explicit
	// LocalSystem account (warned about).
	rel := filepath.Join(root, "rel")
	os.MkdirAll(rel, 0o755)
	copyFile(t, msi, filepath.Join(rel, "longarm-windows-amd64.msi"))
	copyFile(t, bins.longarm, filepath.Join(rel, "longarm-windows-amd64.exe"))
	copyFile(t, bins.longarmd, filepath.Join(rel, "longarmd-windows-amd64.exe"))
	h.hubCmd(t, "release", "add", rel, "--version", "0.9.1")
	code2 := strings.TrimSpace(h.mustLongarm(h.admin, "admin", "join-code", "--role", "worker"))
	uninstalled = false
	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'; $env:LONGARM_SERVICE_ACCOUNT = 'LocalSystem'; $env:LONGARM_MSI_PROPS = 'SERVICE_NAME=%s INSTALLFOLDER=%s DATADIR=%s'; `+
		`& ([scriptblock]::Create((Invoke-RestMethod %s/install.ps1))) -Join %s -Role worker -Name msiw2 -Listen tcp://127.0.0.1:%d`,
		svc, installDir, dataDir, h.httpURL(), code2, freePort(t))
	ictx2, icancel2 := context.WithTimeout(context.Background(), 5*time.Minute)
	defer icancel2()
	if out, err := exec.CommandContext(ictx2, "pwsh", "-NoProfile", "-Command", script).CombinedOutput(); err != nil ||
		!strings.Contains(string(out), "with the MSI") || !strings.Contains(string(out), "runs as LocalSystem") {
		b, _ := os.ReadFile(filepath.Join(os.TempDir(), "longarm-msi.log"))
		t.Fatalf("install.ps1: %v\n%s\n%s", err, out, tail(utf16ToString(b), 4000))
	}
	waitFor(t, "service running (install.ps1)", 30*time.Second, func() bool {
		st, err := platform.QueryService(svc)
		return err == nil && st == platform.ServiceRunning
	})
	if sc := run(t, "sc.exe", "qc", svc); !strings.Contains(sc, "LocalSystem") {
		t.Fatalf("service account (explicit LocalSystem):\n%s", sc)
	}
	if b, _ := os.ReadFile(filepath.Join(os.TempDir(), "longarm-install.log")); !strings.Contains(string(b), "WARNING: the longarmd service runs as LocalSystem") {
		t.Fatalf("install-helper log without the LocalSystem warning:\n%s", tail(string(b), 2000))
	}
	noSecretsInLog(t, filepath.Join(os.TempDir(), "longarm-msi.log"), code2)
	h.mustLongarm(h.admin, "admin", "approve", "msiw2")
	h.waitNode(t, "msiw2", "online", func(n map[string]any) bool { return isTrue(n["online"]) })
	uninstall()
	if st, _ := platform.QueryService(svc); st != platform.ServiceNotInstalled {
		t.Fatalf("service still there after the second uninstall: %v", st)
	}
	if _, err := os.Stat(installDir); err == nil {
		t.Fatal("install dir left behind after the second uninstall")
	}
	h.waitNode(t, "msiw2", "left (revoked)", func(n map[string]any) bool { return n["state"] == "revoked" })
}

// utf16ToString decodes an msiexec log (UTF-16LE) roughly.
func utf16ToString(b []byte) string {
	if len(b) < 2 || b[1] != 0 && !(b[0] == 0xff && b[1] == 0xfe) {
		return string(b)
	}
	out := make([]byte, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		if b[i+1] == 0 {
			out = append(out, b[i])
		}
	}
	return string(out)
}

// noSecretsInLog fails when a verbose msiexec log contains one of secrets.
// The integrator's manual check (PowerShell, after msiexec /l*v install.log):
//
//	Select-String -Path install.log -Encoding unicode -SimpleMatch -Pattern '<JOIN_CODE>','<SERVICE_PASSWORD>'
//
// must print nothing.
func noSecretsInLog(t *testing.T, path string, secrets ...string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("msi log: %v", err)
	}
	log := utf16ToString(b)
	if !strings.Contains(log, "**********") {
		t.Fatalf("%s: no masked value at all; not a verbose log?", path)
	}
	for _, s := range secrets {
		if s == "" {
			continue
		}
		if i := strings.Index(log, s); i >= 0 {
			lo, hi := max(0, i-300), min(len(log), i+len(s)+100)
			t.Fatalf("%s leaks a secret:\n%s", path, strings.ReplaceAll(log[lo:hi], s, "<SECRET>"))
		}
	}
}
