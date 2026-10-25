//go:build windows

package platform

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// Windows Defender Firewall drops unsolicited inbound UDP to programs
// without an allow rule, which turns any NAT into a port-restricted one for
// hole punching (longnet) and Tailscale's direct paths. Longarm adds
// program-scoped inbound allow rules (UDP and TCP, all profiles) for its
// binaries: the MSI for longarm, longarmd and longarm-tray, `longarmd service install` for longarmd.

// FirewallRuleNames are the two rules AddFirewallRules creates for name.
func FirewallRuleNames(name string) []string { return []string{name + " (UDP)", name + " (TCP)"} }

func netsh(args ...string) (string, error) {
	cmd := exec.Command("netsh", append([]string{"advfirewall", "firewall"}, args...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("netsh %s: %v: %s", strings.Join(args[:min(len(args), 3)], " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// AddFirewallRules (re)creates inbound allow rules named "<name> (UDP)" and
// "<name> (TCP)" for program. Needs an elevated process.
func AddFirewallRules(name, program string) error {
	program = longPath(program)
	RemoveFirewallRules(name, program)
	for i, proto := range []string{"UDP", "TCP"} {
		if _, err := netsh("add", "rule", "name="+FirewallRuleNames(name)[i], "dir=in", "action=allow",
			"program="+program, "protocol="+proto, "profile=any", "enable=yes",
			"description=Longarm: direct connections (hole punching, tailscale) to "+filepath.Base(program)); err != nil {
			return err
		}
	}
	return nil
}

// RemoveFirewallRules deletes the rules of AddFirewallRules (missing = ok);
// with a program only the rules for that program (the MSI's rules share
// the names "Longarm longarm|longarmd|longarm-tray (UDP|TCP)" but not the program path).
func RemoveFirewallRules(name, program string) error {
	var last error
	for _, n := range FirewallRuleNames(name) {
		if err := deleteRule(n, program); err != nil {
			last = err
		}
	}
	return last
}

func deleteRule(name, program string) error {
	args := []string{"delete", "rule", "name=" + name, "dir=in"}
	if program != "" {
		args = append(args, "program="+longPath(program))
	}
	if out, err := netsh(args...); err != nil && !strings.Contains(out, "No rules match") {
		return err
	}
	return nil
}

// RemoveFirewallBlockRules deletes the enabled inbound Block rules for
// program (CheckFirewall's Blocking) and returns their names. Rules of the
// same name for the program are deleted with them. Block rules set by
// group policy can't be deleted here: an error names them. Needs an
// elevated process.
func RemoveFirewallBlockRules(program string) ([]string, error) {
	rules, err := FirewallRules()
	if err != nil {
		return nil, err
	}
	var names, policy []string
	seen := map[string]bool{}
	for _, r := range rules {
		if r.Allow || !r.Enabled || !r.Inbound || !sameFile(r.Program, program) || seen[r.Name] {
			continue
		}
		if r.Policy {
			policy = append(policy, r.Name)
			continue
		}
		seen[r.Name] = true
		if err := deleteRule(r.Name, program); err != nil {
			return names, err
		}
		names = append(names, r.Name)
	}
	if len(policy) > 0 {
		return names, fmt.Errorf("inbound Block rules %q for %s come from group policy; ask your administrator to remove them", policy, program)
	}
	return names, nil
}

// Where Windows keeps the firewall rules: the local store and the group
// policy store (missing unless a GPO sets rules).
var firewallRuleKeys = []string{
	`SYSTEM\CurrentControlSet\Services\SharedAccess\Parameters\FirewallPolicy\FirewallRules`,
	`SOFTWARE\Policies\Microsoft\WindowsFirewall\FirewallRules`,
}

// FirewallRules reads the firewall rules from the registry (fast, any
// user, independent of the UI language); netsh (English output) is the
// fallback.
func FirewallRules() ([]FirewallRule, error) {
	var rules []FirewallRule
	for i, path := range firewallRuleKeys {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.QUERY_VALUE)
		if err != nil {
			if i == 0 {
				return netshRules()
			}
			continue
		}
		names, err := k.ReadValueNames(-1)
		if err != nil && i == 0 {
			k.Close()
			return netshRules()
		}
		for _, n := range names {
			v, _, err := k.GetStringValue(n)
			if err != nil {
				continue
			}
			if r, ok := ParseFirewallRegistryRule(v); ok {
				r.Policy = i > 0
				rules = append(rules, r)
			}
		}
		k.Close()
	}
	return rules, nil
}

func netshRules() ([]FirewallRule, error) {
	out, err := netsh("show", "rule", "name=all", "dir=in", "verbose")
	if err != nil {
		return nil, err
	}
	return ParseNetshRules(out), nil
}

// RunningAsSystem reports whether the process runs as LocalSystem.
func RunningAsSystem() bool {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	return err == nil && u.User.Sid.IsWellKnown(windows.WinLocalSystemSid)
}

// Elevated reports whether the process runs with an elevated token that
// holds the Administrators group (firewall rules and taking ownership need
// it). A service account such as LocalService has an "elevated" (unfiltered)
// token without being an administrator - e.g. a GitHub Actions runner
// service - so TokenElevation alone is not enough.
func Elevated() bool {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return false
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return false
	}
	ok, err := windows.Token(0).IsMember(admins) // 0: the calling thread's/process token
	return err == nil && ok
}
