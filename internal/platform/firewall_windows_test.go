//go:build windows

package platform

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFirewallRules(t *testing.T) {
	if !Elevated() {
		t.Skip("needs an elevated process")
	}
	exe := filepath.Join(t.TempDir(), "longarm-fw-test.exe")
	os.WriteFile(exe, []byte("x"), 0o755)
	name := fmt.Sprintf("Longarm test %d", os.Getpid())
	t.Cleanup(func() { RemoveFirewallRules(name, exe) })
	if ok, err := HasFirewallAllowRule(exe); err != nil || ok {
		t.Fatalf("before: %v %v", ok, err)
	}
	if err := AddFirewallRules(name, exe); err != nil {
		t.Fatal(err)
	}
	if err := AddFirewallRules(name, exe); err != nil { // idempotent
		t.Fatal(err)
	}
	if ok, err := HasFirewallAllowRule(exe); err != nil || !ok {
		t.Fatalf("after add: %v %v", ok, err)
	}
	out, _ := netsh("show", "rule", "name="+FirewallRuleNames(name)[0])
	if c := countLines(out, "Rule Name:"); c != 1 {
		t.Fatalf("%d rules named %s:\n%s", c, FirewallRuleNames(name)[0], out)
	}
	if err := RemoveFirewallRules(name, exe); err != nil {
		t.Fatal(err)
	}
	if ok, _ := HasFirewallAllowRule(exe); ok {
		t.Fatal("rule left after remove")
	}
}

func countLines(s, prefix string) int {
	n := 0
	for _, l := range splitLines(s) {
		if len(l) >= len(prefix) && l[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, trimCR(s[start:i]))
			start = i + 1
		}
	}
	return append(out, trimCR(s[start:]))
}

func trimCR(s string) string {
	if len(s) > 0 && s[len(s)-1] == '\r' {
		return s[:len(s)-1]
	}
	return s
}

func TestSecureDir(t *testing.T) {
	if !Elevated() {
		t.Skip("needs an elevated process (sets the owner)")
	}
	dir := filepath.Join(t.TempDir(), "data")
	if err := SecureDir(dir, ""); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("icacls", dir).CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, `BUILTIN\Users`) || strings.Contains(s, "(I)") || !strings.Contains(s, `NT AUTHORITY\SYSTEM`) {
		t.Fatalf("ACL:\n%s", s)
	}
	// A junction in place of the dir is refused.
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if exec.Command("cmd", "/c", "mklink", "/J", link, target).Run() == nil {
		if err := SecureDir(link, ""); err == nil {
			t.Fatal("junction accepted")
		}
	}
}

// TestFirewallRulesFromRegistry: the registry source reads this machine's
// rules (read-only) and agrees with netsh about which rules exist.
func TestFirewallRulesFromRegistry(t *testing.T) {
	rules, err := FirewallRules()
	if err != nil || len(rules) == 0 {
		t.Fatalf("FirewallRules: %d rules, %v", len(rules), err)
	}
	ns, err := netshRules()
	if err != nil {
		t.Skip("netsh:", err)
	}
	// netsh's output is localized; compare only where it is English.
	if len(ns) == 0 {
		t.Skip("netsh output not in English")
	}
	// Built-in rules carry resource names in the registry ("@FirewallAPI.dll,-28502")
	// that netsh resolves; compare the program rules by program and action.
	type key struct {
		prog  string
		allow bool
	}
	have := map[key]bool{}
	for _, r := range rules {
		if r.Inbound && r.Program != "" {
			have[key{strings.ToLower(filepath.Clean(r.Program)), r.Allow}] = true
		}
	}
	n, missing := 0, []string{}
	for _, r := range ns {
		if r.Program == "" || strings.EqualFold(r.Program, "Any") || strings.Contains(r.Program, "%") {
			continue
		}
		n++
		if !have[key{strings.ToLower(filepath.Clean(r.Program)), r.Allow}] {
			missing = append(missing, r.Name+" "+r.Program)
		}
	}
	if len(missing) > n/10 {
		t.Fatalf("%d of %d netsh program rules not found in the registry: %v", len(missing), n, missing)
	}
	t.Logf("%d registry rules; %d netsh program rules, %d not found", len(rules), n, len(missing))
}

// TestFirewallBlockRule: a Block rule for a program (what dismissing the
// Windows Firewall prompt adds) is detected even next to an allow rule, and
// RemoveFirewallBlockRules + AddFirewallRules (longarm firewall allow) fix it.
// Elevated only; the rules are for a temp exe and removed again.
func TestFirewallBlockRule(t *testing.T) {
	if !Elevated() {
		t.Skip("needs an elevated process")
	}
	exe := filepath.Join(t.TempDir(), "longarm-fw-block.exe")
	os.WriteFile(exe, []byte("x"), 0o755)
	name := fmt.Sprintf("Longarm test %d", os.Getpid())
	block := fmt.Sprintf("Longarm test block %d", os.Getpid())
	t.Cleanup(func() {
		RemoveFirewallRules(name, exe)
		deleteRule(block, exe)
	})
	if err := AddFirewallRules(name, exe); err != nil {
		t.Fatal(err)
	}
	if _, err := netsh("add", "rule", "name="+block, "dir=in", "action=block", "program="+exe, "protocol=UDP", "profile=public", "enable=yes"); err != nil {
		t.Fatal(err)
	}
	st, err := CheckFirewall(exe)
	if err != nil || st.OK() || !st.Allowed || len(st.Blocking) != 1 || !strings.HasPrefix(st.Blocking[0], block) {
		t.Fatalf("with a block rule: %+v %v", st, err)
	}
	removed, err := RemoveFirewallBlockRules(exe)
	if err != nil || len(removed) != 1 || removed[0] != block {
		t.Fatalf("RemoveFirewallBlockRules: %v %v", removed, err)
	}
	if st, err := CheckFirewall(exe); err != nil || !st.OK() {
		t.Fatalf("after removing the block rule: %+v %v", st, err)
	}
}
