package platform

import (
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// TestEvalFirewall: the detection logic against fake rule sources (the
// registry form Windows stores and netsh's output).
func TestEvalFirewall(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "longarm.exe")
	other := filepath.Join(t.TempDir(), "longarmd.exe")
	reg := func(vs ...string) []FirewallRule {
		var out []FirewallRule
		for _, v := range vs {
			if r, ok := ParseFirewallRegistryRule(strings.ReplaceAll(v, "EXE", exe)); ok {
				out = append(out, r)
			}
		}
		return out
	}
	allowUDP := "v2.33|Action=Allow|Active=TRUE|Dir=In|Protocol=17|App=EXE|Name=Longarm longarm (UDP)|"
	allowTCP := "v2.33|Action=Allow|Active=TRUE|Dir=In|Protocol=6|App=EXE|Name=Longarm longarm (TCP)|"
	// What dismissing the Windows Firewall prompt leaves behind.
	blockUDP := "v2.33|Action=Block|Active=TRUE|Dir=In|Protocol=17|Profile=Public|App=EXE|Name=longarm.exe|Desc=longarm.exe|"
	blockTCP := "v2.33|Action=Block|Active=TRUE|Dir=In|Protocol=6|Profile=Public|App=EXE|Name=longarm.exe|Desc=longarm.exe|"
	cases := []struct {
		name     string
		rules    []FirewallRule
		ok       bool
		allowed  bool
		blocking []string
		problem  string
	}{
		{"MSI rules", reg(allowUDP, allowTCP), true, true, nil, ""},
		{"no rules", nil, false, false, nil, "no inbound allow rule"},
		{"tcp only", reg(allowTCP), false, false, nil, "no inbound allow rule"},
		{"prompt dismissed: block wins over allow", reg(allowUDP, allowTCP, blockUDP, blockTCP), false, true, []string{"longarm.exe (public)"}, `Block rule "longarm.exe (public)"`},
		{"block for any protocol, all profiles", reg("v2.30|Action=Block|Active=TRUE|Dir=In|App=EXE|Name=deny longarm|"), false, false, []string{"deny longarm"}, "blocks inbound UDP"},
		{"disabled block ignored", reg(allowUDP, strings.Replace(blockUDP, "Active=TRUE", "Active=FALSE", 1)), true, true, nil, ""},
		{"outbound block ignored", reg(allowUDP, strings.Replace(blockUDP, "Dir=In", "Dir=Out", 1)), true, true, nil, ""},
		{"tcp-only block ignored", reg(allowUDP, blockTCP), true, true, nil, ""},
		{"another program's rules", reg(strings.ReplaceAll(blockUDP, "EXE", other), strings.ReplaceAll(allowUDP, "EXE", other)), false, false, nil, "no inbound allow rule"},
		{"path case", []FirewallRule{{Name: "x", Program: strings.ToUpper(exe), Allow: true, Enabled: true, Inbound: true, Protocol: "ANY"}}, true, true, nil, ""},
		{"not rules", reg("garbage", "v2.33|Action=ByPass|Active=TRUE|Dir=In|App=EXE|Name=x|"), false, false, nil, "no inbound allow rule"},
	}
	for _, c := range cases {
		st := EvalFirewall(c.rules, exe)
		if st.OK() != c.ok || st.Allowed != c.allowed || !reflect.DeepEqual(st.Blocking, c.blocking) || !st.Checked {
			t.Errorf("%s: %+v (ok %v)", c.name, st, st.OK())
		}
		if p := st.Problem(); (c.problem == "") != (p == "") || !strings.Contains(p, c.problem) {
			t.Errorf("%s: problem %q", c.name, p)
		}
	}
	if (FirewallStatus{}).OK() != true || (FirewallStatus{}).Problem() != "" {
		t.Error("unchecked status must be ok")
	}

	// netsh's form (the fallback source).
	netshOut := "\r\nRule Name:                            longarm.exe\r\n----------------------------------------------------------------------\r\n" +
		"Enabled:                              Yes\r\nDirection:                            In\r\nProfiles:                             Public\r\n" +
		"Protocol:                             UDP\r\nProgram:                              " + exe + "\r\nAction:                               Block\r\n\r\n" +
		"Rule Name:                            Longarm longarm (UDP)\r\n----------------------------------------------------------------------\r\n" +
		"Enabled:                              Yes\r\nDirection:                            In\r\nProfiles:                             Domain,Private,Public\r\n" +
		"Protocol:                             UDP\r\nProgram:                              " + exe + "\r\nAction:                               Allow\r\n\r\nOk.\r\n"
	st := EvalFirewall(ParseNetshRules(netshOut), exe)
	if !st.Allowed || !reflect.DeepEqual(st.Blocking, []string{"longarm.exe (public)"}) {
		t.Errorf("netsh: %+v", st)
	}

	t.Setenv("LONGARM_TEST_FW_DIR", filepath.Dir(exe))
	if r, _ := ParseFirewallRegistryRule("v2.33|Action=Allow|Active=TRUE|Dir=In|App=%LONGARM_TEST_FW_DIR%/longarm.exe|Name=x|"); !strings.EqualFold(filepath.Clean(r.Program), filepath.Clean(filepath.Dir(exe)+"/longarm.exe")) {
		t.Errorf("env var not expanded: %q", r.Program)
	}
}

func TestFirewallFixCommand(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Program Files")
	longarm := filepath.Join(dir, "longarm.exe")
	longarmd := filepath.Join(dir, "longarmd.exe")
	exe, prog := CommandLine(longarm), CommandLine("x", longarmd)[2:] // the quoting is CommandLine's (TestCommandLine)
	if got := FirewallFixCommand(longarm, longarm); got != exe+` firewall allow` {
		t.Errorf("own exe: %s", got)
	}
	if got := FirewallFixCommand(longarm, longarmd); got != exe+` firewall allow --program `+prog {
		t.Errorf("longarmd: %s", got)
	}
	if got := FirewallFixCommand("", longarmd); got != `longarm firewall allow --program `+prog {
		t.Errorf("longarm on PATH: %s", got)
	}
	if runtime.GOOS == "windows" && !strings.HasPrefix(FirewallFixCommand(longarm, longarm), `& "`) {
		t.Errorf("a quoted path needs PowerShell's call operator: %s", FirewallFixCommand(longarm, longarm))
	}
	if got := FirewallAllowArgs(longarm, longarmd); len(got) != 4 || got[3] != longarmd {
		t.Errorf("FirewallAllowArgs: %q", got)
	}
}

// TestCommandLine: the commands people paste (PowerShell on Windows).
func TestCommandLine(t *testing.T) {
	for _, c := range []struct {
		goos, exe string
		args      []string
		want      string
	}{
		// the user's report: a quoted path needs `& ` in PowerShell
		{"windows", `C:\Program Files\Longarm\longarm.exe`, []string{"firewall", "allow"}, `& "C:\Program Files\Longarm\longarm.exe" firewall allow`},
		{"windows", `C:\Users\me\tmia-desktop-live\longarm-1.2.3\longarm.exe`, []string{"firewall", "allow"}, `C:\Users\me\tmia-desktop-live\longarm-1.2.3\longarm.exe firewall allow`},
		{"windows", "longarm", []string{"firewall", "allow", "--program", `C:\Program Files\Longarm\longarmd.exe`}, `longarm firewall allow --program "C:\Program Files\Longarm\longarmd.exe"`},
		{"windows", `C:\a b\$x\longarm.exe`, nil, `& 'C:\a b\$x\longarm.exe'`}, // $ would expand in double quotes
		{"windows", `C:\it's\longarm.exe`, []string{""}, `& "C:\it's\longarm.exe" ""`},
		{"windows", `C:\x(86)\longarm.exe`, nil, `& "C:\x(86)\longarm.exe"`},
		{"linux", "/usr/local/bin/longarm", []string{"firewall", "allow"}, "/usr/local/bin/longarm firewall allow"},
		{"darwin", "/Users/me/my apps/longarm", []string{"--program", "it's"}, `'/Users/me/my apps/longarm' --program 'it'\''s'`},
	} {
		if got := commandLine(c.goos, c.exe, c.args...); got != c.want {
			t.Errorf("%s %q %q:\n got %s\nwant %s", c.goos, c.exe, c.args, got, c.want)
		}
	}
}
