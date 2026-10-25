package platform

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// Windows Firewall check (all platforms compile it; only Windows has a rule
// source). Windows Defender Firewall drops unsolicited inbound UDP to a
// program without an allow rule, and an inbound Block rule for the program
// wins over any allow rule. Dismissing the "allow access?" prompt for a new
// longarm.exe path silently adds such a Block rule: hole punching (longnet,
// tailscale) then fails and every connection stays on a relay.

// FirewallRule is one firewall rule, as far as the check needs it.
type FirewallRule struct {
	Name     string
	Program  string // the rule's application path ("" = any program)
	Allow    bool   // Action=Allow (false: Block)
	Enabled  bool
	Inbound  bool
	Protocol string   // "UDP", "TCP", "ANY" or a protocol number
	Profiles []string // Domain, Private, Public; empty = all
	// Policy: the rule comes from group policy (longarm can't delete it).
	Policy bool
}

// FirewallStatus is the verdict for one program.
type FirewallStatus struct {
	Program string
	// Checked: the firewall was inspected (Windows only).
	Checked bool
	// Allowed: an enabled inbound allow rule for the program covers UDP.
	Allowed bool
	// Blocking: enabled inbound Block rules for the program that cover UDP,
	// as "name (profiles)".
	Blocking []string
}

// OK reports whether inbound UDP to the program can pass (always true
// where nothing was checked).
func (s FirewallStatus) OK() bool { return !s.Checked || s.Allowed && len(s.Blocking) == 0 }

// Problem describes what is wrong in one line ("" = nothing).
func (s FirewallStatus) Problem() string {
	switch {
	case s.OK():
		return ""
	case len(s.Blocking) > 0:
		return fmt.Sprintf("Windows Firewall blocks inbound UDP to %s (Block rule %s): hole punching fails, connections stay on relays",
			s.Program, strings.Join(quoteAll(s.Blocking), ", "))
	}
	return fmt.Sprintf("Windows Firewall has no inbound allow rule for %s: hole punching fails, connections stay on relays", s.Program)
}

func quoteAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
}

// FirewallFixCommand is the command that fixes s (run elevated): longarm.exe's
// `firewall allow` for the program, which removes its Block rules and adds
// the allow rules. longarmExe is the longarm.exe to run ("" = "longarm" on PATH).
func FirewallFixCommand(longarmExe, program string) string {
	cmd := "longarm"
	if longarmExe != "" {
		cmd = `"` + longarmExe + `"`
	}
	cmd += " firewall allow"
	if program != "" && !sameFile(program, longarmExe) {
		cmd += ` --program "` + program + `"`
	}
	return cmd
}

func sameFile(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	a, _ = filepath.Abs(a)
	b, _ = filepath.Abs(b)
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// coversUDP reports whether a rule's protocol includes UDP.
func coversUDP(p string) bool {
	switch strings.ToUpper(strings.TrimSpace(p)) {
	case "", "ANY", "UDP", "17", "256":
		return true
	}
	return false
}

// EvalFirewall checks program against rules (Windows semantics: an
// enabled inbound Block rule wins over allow rules).
func EvalFirewall(rules []FirewallRule, program string) FirewallStatus {
	st := FirewallStatus{Program: program, Checked: true}
	for _, r := range rules {
		if !r.Enabled || !r.Inbound || r.Program == "" || !sameFile(r.Program, program) || !coversUDP(r.Protocol) {
			continue
		}
		if r.Allow {
			st.Allowed = true
			continue
		}
		name := r.Name
		if len(r.Profiles) > 0 {
			name += " (" + strings.ToLower(strings.Join(r.Profiles, ", ")) + ")"
		}
		st.Blocking = append(st.Blocking, name)
	}
	return st
}

// ParseFirewallRegistryRule parses a rule as Windows stores it in the
// registry (...\FirewallPolicy\FirewallRules):
// "v2.33|Action=Block|Active=TRUE|Dir=In|Protocol=17|Profile=Public|App=C:\x\longarm.exe|Name=longarm.exe|".
// ok is false for something that isn't a rule.
func ParseFirewallRegistryRule(v string) (FirewallRule, bool) {
	if !strings.HasPrefix(v, "v2.") {
		return FirewallRule{}, false
	}
	r := FirewallRule{Protocol: "ANY"}
	action := ""
	for _, f := range strings.Split(v, "|")[1:] {
		k, val, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		switch k {
		case "Action":
			action = val
		case "Active":
			r.Enabled = strings.EqualFold(val, "TRUE")
		case "Dir":
			r.Inbound = strings.EqualFold(val, "In")
		case "Protocol":
			switch val {
			case "17":
				r.Protocol = "UDP"
			case "6":
				r.Protocol = "TCP"
			default:
				r.Protocol = val
			}
		case "Profile":
			r.Profiles = append(r.Profiles, val)
		case "App":
			r.Program = expandWinEnv(val)
		case "Name":
			r.Name = val
		}
	}
	if action == "" {
		return FirewallRule{}, false
	}
	r.Allow = strings.EqualFold(action, "Allow")
	if !r.Allow && !strings.EqualFold(action, "Block") {
		return FirewallRule{}, false // ByPass etc.
	}
	return r, true
}

var winEnvRE = regexp.MustCompile(`%([^%]+)%`)

// expandWinEnv expands %VAR% (rules may store "%ProgramFiles%\...").
func expandWinEnv(s string) string {
	return winEnvRE.ReplaceAllStringFunc(s, func(m string) string {
		if v, ok := os.LookupEnv(m[1 : len(m)-1]); ok {
			return v
		}
		return m
	})
}

// ParseNetshRules parses `netsh advfirewall firewall show rule name=all
// dir=in verbose` (English output; the fallback when the registry can't be
// read).
func ParseNetshRules(out string) []FirewallRule {
	var rules []FirewallRule
	for _, block := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n\n") {
		f := map[string]string{}
		for _, line := range strings.Split(block, "\n") {
			k, v, ok := strings.Cut(line, ":")
			if ok {
				f[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
		if f["Rule Name"] == "" && f["Program"] == "" {
			continue
		}
		r := FirewallRule{Name: f["Rule Name"], Program: f["Program"], Allow: strings.EqualFold(f["Action"], "Allow"),
			Enabled: strings.EqualFold(f["Enabled"], "Yes"), Inbound: !strings.EqualFold(f["Direction"], "Out"), Protocol: f["Protocol"]}
		if p := f["Profiles"]; p != "" && !strings.EqualFold(p, "Domain,Private,Public") {
			for _, x := range strings.Split(p, ",") {
				r.Profiles = append(r.Profiles, strings.TrimSpace(x))
			}
		}
		if !r.Allow && !strings.EqualFold(f["Action"], "Block") {
			continue
		}
		rules = append(rules, r)
	}
	return rules
}

// TestFirewallRulesEnv names a file of rules in the registry form (one per
// line) that CheckFirewall uses instead of the system's, on any OS: the
// e2e tests' fake rule source.
const TestFirewallRulesEnv = "LONGARM_TEST_FIREWALL_RULES"

// CheckFirewall reports whether inbound UDP to program passes Windows
// Firewall: an allow rule and no Block rule. Outside Windows nothing is
// checked (Checked = false).
func CheckFirewall(program string) (FirewallStatus, error) {
	program, _ = filepath.Abs(program)
	var rules []FirewallRule
	if f := os.Getenv(TestFirewallRulesEnv); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return FirewallStatus{Program: program}, err
		}
		for _, l := range strings.Split(string(b), "\n") {
			if r, ok := ParseFirewallRegistryRule(strings.TrimSpace(l)); ok {
				rules = append(rules, r)
			}
		}
	} else {
		if runtime.GOOS != "windows" {
			return FirewallStatus{Program: program}, nil
		}
		var err error
		if rules, err = FirewallRules(); err != nil {
			return FirewallStatus{Program: program}, err
		}
	}
	return EvalFirewall(rules, program), nil
}

// HasFirewallAllowRule reports whether an enabled inbound allow rule for
// program (UDP or any protocol) exists (true where nothing is checked).
func HasFirewallAllowRule(program string) (bool, error) {
	st, err := CheckFirewall(program)
	return st.Allowed || !st.Checked, err
}
