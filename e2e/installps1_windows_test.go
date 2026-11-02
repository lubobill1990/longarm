package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestInstallPs1AccountOnlyAndUninstall: the hub's install.ps1 with -PerUser
// installs for this account only and, like the MSI, sets the tray to start
// at every logon (HKCU Run) with a Start menu entry; uninstall.ps1 -Prefix
// removes exactly that install - its files, the Run value, the shortcut -
// and never the MSI. CI only: it writes HKCU Run and the Start menu of the
// account running it.
func TestInstallPs1AccountOnlyAndUninstall(t *testing.T) {
	if os.Getenv("CI") == "" {
		t.Skip("writes HKCU Run and the Start menu of this account: CI only")
	}
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("pwsh not installed")
	}
	h := newHubEnv(t, "")
	rel := filepath.Join(h.root, "release")
	os.MkdirAll(rel, 0o755)
	// the tray: a stand-in (the test helper), started once by install.ps1
	for tool, src := range map[string]string{"longarm": bins.longarm, "longarmd": bins.longarmd, "longarm-tray": bins.helper} {
		copyBin(t, src, filepath.Join(rel, fmt.Sprintf("%s-windows-%s.exe", tool, runtime.GOARCH)))
	}
	if out := h.hubCmd(t, "release", "add", rel, "--version", "1.2.3"); !strings.Contains(out, "1.2.3") {
		t.Fatalf("release add: %s", out)
	}
	prefix := filepath.Join(h.root, "pu-bin")
	env := fmt.Sprintf("$env:LONGARM_PREFIX='%s'; $env:LONGARM_DATA_DIR='%s'; $env:LONGARM_CONFIG_DIR='%s'; ",
		prefix, filepath.Join(h.root, "pu-data"), filepath.Join(h.root, "pu-cfg"))
	run := func(script string) string {
		t.Helper()
		out, err := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-Command", env+script).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", script, err, out)
		}
		return string(out)
	}
	ps := func(q string) string {
		t.Helper()
		out, _ := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-Command", q).CombinedOutput()
		return strings.TrimSpace(string(out))
	}
	runValue := `(Get-ItemProperty 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Run' -Name 'longarm-tray' -ErrorAction SilentlyContinue).'longarm-tray'`
	lnk := `Join-Path ([Environment]::GetFolderPath('Programs')) 'Longarm.lnk'`
	tray := filepath.Join(prefix, "longarm-tray.exe")
	t.Cleanup(func() { // whatever a failed run left
		ps(`Remove-ItemProperty 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Run' -Name 'longarm-tray' -ErrorAction SilentlyContinue; Remove-Item (` + lnk + `) -ErrorAction SilentlyContinue`)
	})

	out := run(fmt.Sprintf("$env:LONGARM_PER_USER='1'; $env:LONGARM_NO_PATH='1'; $env:LONGARM_NO_FIREWALL='1'; irm %s/install.ps1 | iex", h.httpURL()))
	if !strings.Contains(out, "installed longarm, longarmd, longarm-tray into "+prefix) || !strings.Contains(out, "the Longarm tray is starting") {
		t.Fatalf("install.ps1 -PerUser:\n%s", out)
	}
	if got := ps(runValue); got != `"`+tray+`"` {
		t.Fatalf("HKCU Run longarm-tray = %q, want the installed tray\n%s", got, out)
	}
	if got := ps(`(New-Object -ComObject WScript.Shell).CreateShortcut((` + lnk + `)).TargetPath`); !strings.EqualFold(got, tray) {
		t.Fatalf("Start menu entry -> %q, want %s", got, tray)
	}

	out = run(fmt.Sprintf("irm %s/uninstall.ps1 | iex", h.httpURL()))
	if strings.Contains(out, "removing the MSI") || strings.Contains(out, "Windows prompt") {
		t.Fatalf("uninstall.ps1 -Prefix went for the MSI:\n%s", out)
	}
	if !strings.Contains(out, "removed "+prefix) {
		t.Fatalf("uninstall.ps1:\n%s", out)
	}
	if _, err := os.Stat(prefix); !os.IsNotExist(err) {
		t.Fatalf("%s still there (%v)\n%s", prefix, err, out)
	}
	if got := ps(runValue); got != "" {
		t.Fatalf("HKCU Run longarm-tray left: %q", got)
	}
	if got := ps(`Test-Path (` + lnk + `)`); got != "False" {
		t.Fatalf("Start menu entry left (%s)", got)
	}
}
