package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lubobill1990/longarm/internal/platform"
)

// TestLongarmFirewall: longarm firewall status/allow/remove manage the inbound rule
// hole punching needs (installs without the MSI). Elevated only; the rule
// is for a copy of longarm.exe in a temp dir and removed again.
func TestLongarmFirewall(t *testing.T) {
	if !platform.Elevated() {
		t.Skip("needs an elevated process (firewall rules)")
	}
	t.Parallel()
	e := newEnv(t)
	exe := filepath.Join(t.TempDir(), "longarm.exe")
	b, err := os.ReadFile(bins.longarm)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, b, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) result {
		t.Helper()
		cmd := exec.Command(exe, args...)
		cmd.Env = append(os.Environ(), "LONGARM_CONFIG_DIR="+e.client)
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return result{out.String(), errb.String(), code}
	}
	t.Cleanup(func() { platform.RemoveFirewallRules("Longarm longarm", exe) }) // only this exe's: the MSI's rules share the name
	if r := run("firewall"); r.code == 0 || !strings.Contains(r.stdout, "missing") {
		t.Fatalf("status without a rule: %+v", r)
	}
	if r := run("firewall", "allow"); r.code != 0 || !strings.Contains(r.stdout, "inbound UDP+TCP allowed") {
		t.Fatalf("allow: %+v", r)
	}
	if r := run("firewall", "status"); r.code != 0 || !strings.Contains(r.stdout, "ok:") {
		t.Fatalf("status with the rule: %+v", r)
	}
	// A dismissed firewall prompt leaves a Block rule: it wins over the
	// allow rule; longarm firewall allow removes it.
	block := fmt.Sprintf("Longarm e2e block %d", os.Getpid())
	netsh := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("netsh", append([]string{"advfirewall", "firewall"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("netsh %v: %v %s", args, err, out)
		}
	}
	netsh("add", "rule", "name="+block, "dir=in", "action=block", "program="+exe, "protocol=UDP", "profile=public", "enable=yes")
	t.Cleanup(func() {
		exec.Command("netsh", "advfirewall", "firewall", "delete", "rule", "name="+block, "dir=in", "program="+exe).Run()
	})
	if r := run("firewall", "status"); r.code == 0 || !strings.Contains(r.stdout, "blocked:") || !strings.Contains(r.stdout, block) ||
		!strings.Contains(r.stdout, "firewall allow") {
		t.Fatalf("status with a block rule: %+v", r)
	}
	if r := run("firewall", "allow"); r.code != 0 || !strings.Contains(r.stdout, "removed inbound Block rules") || !strings.Contains(r.stdout, block) {
		t.Fatalf("allow with a block rule: %+v", r)
	}
	if r := run("firewall", "status"); r.code != 0 || !strings.Contains(r.stdout, "ok:") {
		t.Fatalf("status after allow: %+v", r)
	}
	// The MSI's rules have the same names ("Longarm longarm (UDP)") for the
	// installed longarm.exe: longarm firewall remove (and allow) must leave rules for
	// other programs alone.
	decoy := filepath.Join(t.TempDir(), "longarm.exe")
	os.WriteFile(decoy, b, 0o755)
	if err := platform.AddFirewallRules("Longarm longarm", decoy); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { platform.RemoveFirewallRules("Longarm longarm", decoy) })
	if r := run("firewall", "remove"); r.code != 0 {
		t.Fatalf("remove: %+v", r)
	}
	if ok, _ := platform.HasFirewallAllowRule(exe); ok {
		t.Fatal("rule left behind")
	}
	if ok, _ := platform.HasFirewallAllowRule(decoy); !ok {
		t.Fatal("longarm firewall remove deleted another program's rule of the same name (the MSI's)")
	}
}
