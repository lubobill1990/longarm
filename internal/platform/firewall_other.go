//go:build !windows

package platform

// Host firewalls on Linux/macOS are not managed by Longarm (see the
// README: ufw/firewalld only when enabled, macOS application firewall).

// FirewallRuleNames are the rule names AddFirewallRules would create.
func FirewallRuleNames(name string) []string { return []string{name + " (UDP)", name + " (TCP)"} }

// AddFirewallRules is a no-op outside Windows.
func AddFirewallRules(name, program string) error { return nil }

// RemoveFirewallRules is a no-op outside Windows.
func RemoveFirewallRules(name, program string) error { return nil }

// RemoveFirewallBlockRules is a no-op outside Windows.
func RemoveFirewallBlockRules(program string) ([]string, error) { return nil, nil }

// FirewallRules: no rule source outside Windows.
func FirewallRules() ([]FirewallRule, error) { return nil, nil }

// RunningAsSystem is a Windows concept (LocalSystem); false elsewhere.
func RunningAsSystem() bool { return false }

// Elevated reports whether the process runs as root.
func Elevated() bool { return euid() == 0 }
