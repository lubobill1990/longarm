package e2e

import (
	"encoding/xml"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"unicode"
)

// TestMSIServiceLogonOnUpgrade (static, every platform): the conditions of
// longarm.wxs, evaluated for the installs that create, keep or change the
// worker service. An upgrade of a service worker with no SERVICE_PASSWORD -
// the update service's msiexec /i /qn (longarm worker update, --auto, the
// tray's banner), or a double-clicked MSI - keeps the service's logon (no
// ServiceInstall, no account check: every such update failed with 1603); an
// upgrade given another account, a password or LocalSystem re-creates it as
// before; a fresh install still needs an account.
func TestMSIServiceLogonOnUpgrade(t *testing.T) {
	w := loadWxsConditions(t)
	const user = `.\alice`
	// what AppSearch / FindRelatedProducts find on a machine with a service worker
	upgrade := func(saved string, extra map[string]string) map[string]string {
		p := map[string]string{"WIX_UPGRADE_DETECTED": "{OLD}", "PREV_LONGARMD_SERVICE": `C:\Program Files\Longarm\longarmd.exe`,
			"ROLES": "both", "SAVED_RUN": "service", "SAVED_SERVICE_ACCOUNT": saved, "SERVICE_NAME": "longarmd"}
		for k, v := range extra {
			p[k] = v
		}
		return p
	}
	for _, tc := range []struct {
		name  string
		props map[string]string
		// want: KeepServiceLogon, the service (re)created, CheckServiceAccount refuses, the account recorded
		keep, create, refuse bool
		account              string
	}{
		{"update service: no properties", upgrade(user, nil), true, false, false, user},
		{"another spelling of the same account", upgrade(user, map[string]string{"SERVICE_USER": `.\ALICE`}), true, false, false, `.\ALICE`},
		{"a pre-RUN service install", upgrade(user, map[string]string{"SAVED_RUN": ""}), true, false, false, user},
		{"LocalSystem service", upgrade("LocalSystem", nil), true, false, false, "LocalSystem"},
		{"LocalSystem service, per-user isolation", upgrade("LocalSystem", map[string]string{"ISOLATION": "per-user"}), true, false, false, "LocalSystem"},
		{"the password again", upgrade(user, map[string]string{"SERVICE_PASSWORD": "pw"}), false, true, false, user},
		{"another account with its password", upgrade(user, map[string]string{"SERVICE_USER": `.\bob`, "SERVICE_PASSWORD": "pw"}), false, true, false, `.\bob`},
		{"another account without a password", upgrade(user, map[string]string{"SERVICE_USER": `.\bob`}), false, true, true, `.\bob`},
		{"to LocalSystem", upgrade(user, map[string]string{"SERVICE_ACCOUNT": "LocalSystem"}), false, true, false, "LocalSystem"},
		{"to per-user isolation", upgrade(user, map[string]string{"ISOLATION": "per-user"}), false, true, false, "LocalSystem"},
		{"no account recorded", upgrade("", map[string]string{"RUN": "service"}), false, true, true, ""},
		{"fresh install without an account", map[string]string{"ROLES": "worker", "RUN": "service"}, false, true, true, ""},
		{"fresh install with an account", map[string]string{"ROLES": "worker", "RUN": "service", "SERVICE_USER": user, "SERVICE_PASSWORD": "pw"}, false, true, false, user},
		{"fresh LocalSystem install", map[string]string{"ROLES": "worker", "RUN": "service", "SERVICE_ACCOUNT": "LocalSystem"}, false, true, false, "LocalSystem"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := w.execute(t, tc.props)
			if got := p["KeepServiceLogon"] != ""; got != tc.keep {
				t.Errorf("KeepServiceLogon = %v, want %v", got, tc.keep)
			}
			if got := w.component(t, "LongarmdServiceInstall", p); got != tc.create {
				t.Errorf("LongarmdServiceInstall (the ServiceInstall) = %v, want %v", got, tc.create)
			}
			if !w.component(t, "LongarmdService", p) {
				t.Error("LongarmdService (longarmd.exe, the service's stop and removal) not installed")
			}
			if got := w.custom(t, "CheckServiceAccount", p); got != tc.refuse {
				t.Errorf("CheckServiceAccount refuses = %v, want %v", got, tc.refuse)
			}
			if w.custom(t, "CheckLongarmdSwitch", p) {
				t.Error("CheckLongarmdSwitch refuses")
			}
			if p["SERVICE_ACCOUNT"] != tc.account {
				t.Errorf("SERVICE_ACCOUNT (recorded, LongarmdSetup's user) = %q, want %q", p["SERVICE_ACCOUNT"], tc.account)
			}
		})
	}

	// the wizard's service page: on an upgrade the recorded account goes on without a password
	for _, tc := range []struct {
		name  string
		props map[string]string
		next  string
	}{
		{"upgrade, the recorded account", map[string]string{"ServiceToKeep": "1", "SAVED_SERVICE_ACCOUNT": user, "SERVICE_USER": user}, "UpdatesDlg"},
		{"upgrade, another account", map[string]string{"ServiceToKeep": "1", "SAVED_SERVICE_ACCOUNT": user, "SERVICE_USER": `.\bob`}, "NeedPasswordDlg"},
		{"upgrade, another account and its password", map[string]string{"ServiceToKeep": "1", "SAVED_SERVICE_ACCOUNT": user, "SERVICE_USER": `.\bob`, "SERVICE_PASSWORD": "pw"}, "UpdatesDlg"},
		{"fresh install", map[string]string{"SERVICE_USER": user}, "NeedPasswordDlg"},
		{"fresh install with a password", map[string]string{"SERVICE_USER": user, "SERVICE_PASSWORD": "pw"}, "UpdatesDlg"},
		{"LocalSystem", map[string]string{"SVC_LOCALSYSTEM": "1"}, "UpdatesDlg"},
	} {
		var got []string
		for _, pub := range w.ui.Publish {
			if pub.Dialog == "ServiceAccountDlg" && pub.Control == "Next" && pub.Event != "" && evalCond(t, pub.Condition, tc.props) {
				got = append(got, pub.Value)
			}
		}
		if len(got) != 1 || got[0] != tc.next {
			t.Errorf("ServiceAccountDlg Next, %s: %v, want %s", tc.name, got, tc.next)
		}
	}
}

type wxsConditions struct {
	sets       []wxsSet
	components map[string]string
	customs    map[string]string
	ui         struct {
		Publish []struct {
			Dialog    string `xml:"Dialog,attr"`
			Control   string `xml:"Control,attr"`
			Event     string `xml:"Event,attr"`
			Value     string `xml:"Value,attr"`
			Condition string `xml:"Condition,attr"`
		} `xml:"Publish"`
	}
}

type wxsSet struct {
	ID        string `xml:"Id,attr"`
	Action    string `xml:"Action,attr"`
	Value     string `xml:"Value,attr"`
	Before    string `xml:"Before,attr"`
	Condition string `xml:"Condition,attr"`
	Sequence  string `xml:"Sequence,attr"`
}

func loadWxsConditions(t *testing.T) *wxsConditions {
	t.Helper()
	b, err := os.ReadFile("../packaging/windows/longarm.wxs")
	if err != nil {
		t.Fatal(err)
	}
	src := regexp.MustCompile(`<\?[^>]*\?>`).ReplaceAll(b, nil)
	src = append([]byte(`<?xml version="1.0" encoding="utf-8"?>`), src...)
	type cond struct {
		ID        string `xml:"Id,attr"`
		Action    string `xml:"Action,attr"`
		Condition string `xml:"Condition,attr"`
	}
	w := &wxsConditions{components: map[string]string{}, customs: map[string]string{}}
	var doc struct {
		Package struct {
			Sets       []wxsSet `xml:"SetProperty"`
			Components []cond   `xml:"ComponentGroup>Component"`
			Sequence   []cond   `xml:"InstallExecuteSequence>Custom"`
		} `xml:"Package"`
	}
	if err := xml.Unmarshal(src, &doc); err != nil {
		t.Fatal(err)
	}
	var ui struct {
		Fragment []struct {
			UI []struct {
				Inner []byte `xml:",innerxml"`
			} `xml:"UI"`
		} `xml:"Fragment"`
	}
	if err := xml.Unmarshal(src, &ui); err != nil {
		t.Fatal(err)
	}
	for _, f := range ui.Fragment {
		for _, u := range f.UI {
			if err := xml.Unmarshal(append(append([]byte("<UI>"), u.Inner...), "</UI>"...), &w.ui); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(w.ui.Publish) == 0 {
		t.Fatal("no Publish in the UI")
	}
	w.sets = doc.Package.Sets
	for _, c := range doc.Package.Components {
		w.components[c.ID] = c.Condition
	}
	for _, c := range doc.Package.Sequence {
		w.customs[c.Action] = c.Condition
	}
	return w
}

func (w *wxsConditions) set(t *testing.T, action string) wxsSet {
	t.Helper()
	for _, s := range w.sets {
		id := s.Action
		if id == "" {
			id = "Set" + s.ID
		}
		if id == action {
			return s
		}
	}
	t.Fatalf("no SetProperty %s", action)
	return wxsSet{}
}

// execute applies, to a silent install's properties, the SetProperty
// actions of the execute sequence that decide the worker service, in
// their order (each Before chain as in longarm.wxs, checked here).
func (w *wxsConditions) execute(t *testing.T, props map[string]string) map[string]string {
	t.Helper()
	p := map[string]string{}
	for k, v := range props {
		p[k] = v
	}
	// (before) each Before the next, the last of each chain before CostFinalize or CheckServiceAccount
	chains := [][]string{
		{"SetRunSaved", "SetRunLegacy", "SetRunDefault", "SetRunAsServiceUI"},
		{"SetIsWorker", "CostFinalize"},
		{"SetServiceToKeep", "SetKeepServiceLogon", "CostFinalize"},
		{"SetServiceAccountPerUser", "SetServiceAccountUser", "SetServiceAccountSaved", "CheckServiceAccount"},
	}
	for _, chain := range chains {
		for i, a := range chain[:len(chain)-1] {
			s := w.set(t, a)
			if s.Before != chain[i+1] {
				t.Fatalf("%s: Before=%q, want %s", a, s.Before, chain[i+1])
			}
			if s.Sequence == "ui" {
				t.Fatalf("%s is not in the execute sequence", a)
			}
			if evalCond(t, s.Condition, p) {
				p[s.ID] = formatProps(s.Value, p)
			}
		}
	}
	return p
}

func (w *wxsConditions) component(t *testing.T, id string, p map[string]string) bool {
	t.Helper()
	c, ok := w.components[id]
	if !ok {
		t.Fatalf("no component %s", id)
	}
	return c == "" || evalCond(t, c, p)
}

func (w *wxsConditions) custom(t *testing.T, action string, p map[string]string) bool {
	t.Helper()
	c, ok := w.customs[action]
	if !ok {
		t.Fatalf("%s is not scheduled", action)
	}
	return evalCond(t, c, p)
}

func formatProps(v string, p map[string]string) string {
	return regexp.MustCompile(`\[([A-Za-z_][A-Za-z0-9_.]*)\]`).ReplaceAllStringFunc(v, func(m string) string { return p[m[1:len(m)-1]] })
}

// evalCond evaluates a Windows Installer condition: NOT / AND / OR,
// parentheses, properties (true when not empty), "strings", and the string
// comparisons = <> << >> (~ ignores case). Enough for longarm.wxs's.
func evalCond(t *testing.T, cond string, p map[string]string) bool {
	t.Helper()
	e := &condEval{p: p}
	e.lex(cond)
	v, err := e.or()
	if err == nil && e.i != len(e.toks) {
		err = fmt.Errorf("trailing %q", e.toks[e.i:])
	}
	if err != nil {
		t.Fatalf("condition %q: %v", cond, err)
	}
	return v
}

type condEval struct {
	toks []string
	i    int
	p    map[string]string
}

func (e *condEval) lex(s string) {
	for i := 0; i < len(s); {
		c := rune(s[i])
		switch {
		case unicode.IsSpace(c):
			i++
		case c == '(' || c == ')':
			e.toks = append(e.toks, string(c))
			i++
		case c == '"':
			j := strings.IndexByte(s[i+1:], '"')
			if j < 0 {
				j = len(s) - i - 1
			}
			e.toks = append(e.toks, s[i:i+j+2])
			i += j + 2
		case strings.ContainsRune("~=<>", c):
			j := i
			for j < len(s) && strings.ContainsRune("~=<>", rune(s[j])) {
				j++
			}
			e.toks = append(e.toks, s[i:j])
			i = j
		default:
			j := i
			for j < len(s) && !unicode.IsSpace(rune(s[j])) && !strings.ContainsRune(`()"~=<>`, rune(s[j])) {
				j++
			}
			e.toks = append(e.toks, s[i:j])
			i = j
		}
	}
}

func (e *condEval) peek() string {
	if e.i < len(e.toks) {
		return e.toks[e.i]
	}
	return ""
}

func (e *condEval) or() (bool, error) {
	v, err := e.and()
	for err == nil && e.peek() == "OR" {
		e.i++
		var r bool
		r, err = e.and()
		v = v || r
	}
	return v, err
}

func (e *condEval) and() (bool, error) {
	v, err := e.not()
	for err == nil && e.peek() == "AND" {
		e.i++
		var r bool
		r, err = e.not()
		v = v && r
	}
	return v, err
}

func (e *condEval) not() (bool, error) {
	if e.peek() == "NOT" {
		e.i++
		v, err := e.not()
		return !v, err
	}
	if e.peek() == "(" {
		e.i++
		v, err := e.or()
		if err == nil && e.peek() != ")" {
			err = fmt.Errorf("no ) at %d", e.i)
		}
		e.i++
		return v, err
	}
	l, err := e.value()
	if err != nil {
		return false, err
	}
	op := e.peek()
	if op == "" || !strings.ContainsAny(op[:1], "~=<>") {
		return l != "", nil
	}
	e.i++
	r, err := e.value()
	if err != nil {
		return false, err
	}
	if strings.HasPrefix(op, "~") {
		op, l, r = op[1:], strings.ToLower(l), strings.ToLower(r)
	}
	switch op {
	case "=":
		return l == r, nil
	case "<>":
		return l != r, nil
	case "<<":
		return strings.HasPrefix(l, r), nil
	case ">>":
		return strings.HasSuffix(l, r), nil
	}
	return false, fmt.Errorf("operator %q", op)
}

func (e *condEval) value() (string, error) {
	tok := e.peek()
	if tok == "" || tok == ")" || tok == "AND" || tok == "OR" {
		return "", fmt.Errorf("no value at %d", e.i)
	}
	e.i++
	if strings.HasPrefix(tok, `"`) {
		return strings.Trim(tok, `"`), nil
	}
	if strings.HasPrefix(tok, "%") {
		return os.Getenv(tok[1:]), nil
	}
	return e.p[tok], nil
}
