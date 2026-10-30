package platform

import (
	"path/filepath"
	"reflect"
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
