package e2e

import (
	"encoding/xml"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/lubobill1990/longarm/internal/longarmd"
)

// TestMSIPropertiesOverridable (static, every platform): a property given
// on the msiexec command line must win over the value an earlier install
// remembered in the registry - an upgrade may change the hub, replace the
// CA pin, switch between join code and LOGIN, between Default and Custom
// (MODE), between the user-session worker and the service (RUN), add or
// drop the admin worker. AppSearch overwrites the property it searches
// into, so HUB, CA_FINGERPRINT, LOGIN, LABELS, MODE, RUN, ADMIN_WORKER and
// SESSION_SID must not carry a RegistrySearch themselves; their SAVED_*
// twin does, and a SetProperty applies it only when the property is empty.
func TestMSIPropertiesOverridable(t *testing.T) {
	b, err := os.ReadFile("../packaging/windows/longarm.wxs")
	if err != nil {
		t.Fatal(err)
	}
	type search struct {
		Name string `xml:"Name,attr"`
	}
	type property struct {
		ID     string   `xml:"Id,attr"`
		Search []search `xml:"RegistrySearch"`
	}
	type setProperty struct {
		ID        string `xml:"Id,attr"`
		Value     string `xml:"Value,attr"`
		Condition string `xml:"Condition,attr"`
	}
	var doc struct {
		Package struct {
			Properties []property    `xml:"Property"`
			Sets       []setProperty `xml:"SetProperty"`
		} `xml:"Package"`
	}
	if err := xml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	props := map[string]property{}
	for _, p := range doc.Package.Properties {
		props[p.ID] = p
	}
	for _, name := range []string{"HUB", "CA_FINGERPRINT", "LOGIN", "LABELS", "MODE", "RUN", "ADMIN_WORKER", "SESSION_SID"} {
		p, ok := props[name]
		if !ok {
			t.Errorf("no property %s", name)
			continue
		}
		if len(p.Search) != 0 {
			t.Errorf("%s searches the registry itself: AppSearch would override the command line", name)
		}
		if s := props["SAVED_"+name]; len(s.Search) != 1 {
			t.Errorf("SAVED_%s: no registry search", name)
		}
		found := false
		for _, s := range doc.Package.Sets {
			if s.ID == name && s.Value == "[SAVED_"+name+"]" {
				found = true
				if !strings.Contains(s.Condition, "NOT "+name+" ") {
					t.Errorf("SetProperty %s applies the saved value unconditionally: %q", name, s.Condition)
				}
			}
		}
		if !found {
			t.Errorf("no SetProperty %s = [SAVED_%s]", name, name)
		}
	}
}

// TestMSIModeRun (static): the shape of MODE / RUN / ADMIN_WORKER in
// longarm.wxs, which CI's msi job builds and validates but never installs.
//   - MODE, RUN and ADMIN_WORKER have no fixed value (SetProperty decides:
//     remembered, else custom when a hub or code is given or there is no
//     built-in hub, else default; RUN: remembered, a pre-RUN service install
//     keeps its service, else session);
//   - the service, its start and LONGARM_DATA_DIR come only with RUN=service;
//     the user-session worker's task only with RUN=session;
//   - every SetProperty / custom action ordering names an action that exists;
//   - the checks that refuse a broken silent install are scheduled.
func TestMSIModeRun(t *testing.T) {
	b, err := os.ReadFile("../packaging/windows/longarm.wxs")
	if err != nil {
		t.Fatal(err)
	}
	// the preprocessor's conditionals aren't XML: drop them (both branches stay)
	src := regexp.MustCompile(`<\?[^>]*\?>`).ReplaceAll(b, nil)
	src = append([]byte(`<?xml version="1.0" encoding="utf-8"?>`), src...)
	type attrs struct {
		ID        string `xml:"Id,attr"`
		Action    string `xml:"Action,attr"`
		Value     string `xml:"Value,attr"`
		Before    string `xml:"Before,attr"`
		After     string `xml:"After,attr"`
		Condition string `xml:"Condition,attr"`
		Sequence  string `xml:"Sequence,attr"`
	}
	var doc struct {
		Package struct {
			Properties []attrs `xml:"Property"`
			Sets       []attrs `xml:"SetProperty"`
			Actions    []attrs `xml:"CustomAction"`
			Components []struct {
				attrs
				Services []attrs `xml:"ServiceInstall"`
				Env      []attrs `xml:"Environment"`
			} `xml:"ComponentGroup>Component"`
			Sequence []attrs `xml:"InstallExecuteSequence>Custom"`
		} `xml:"Package"`
	}
	if err := xml.Unmarshal(src, &doc); err != nil {
		t.Fatal(err)
	}
	p := doc.Package
	for _, pr := range p.Properties {
		switch pr.ID {
		case "MODE", "RUN", "ADMIN_WORKER", "HUB", "LOGIN", "SESSION_SID":
			if pr.Value != "" {
				t.Errorf("property %s has a fixed value %q", pr.ID, pr.Value)
			}
		}
	}
	actions := map[string]bool{"CostFinalize": true, "InstallInitialize": true, "InstallServices": true, "StopServices": true}
	sets := map[string]attrs{}
	for _, s := range p.Sets {
		id := s.Action
		if id == "" {
			id = "Set" + s.ID
		}
		actions[id] = true
		sets[id] = s
	}
	for _, a := range p.Actions {
		actions[a.ID] = true
	}
	for id, s := range sets {
		if s.Before != "" && !actions[s.Before] {
			t.Errorf("SetProperty %s before unknown action %s", id, s.Before)
		}
	}
	for _, c := range p.Sequence {
		if !actions[c.Action] {
			t.Errorf("sequence: unknown action %s", c.Action)
		}
		for _, o := range []string{c.Before, c.After} {
			if o != "" && !actions[o] {
				t.Errorf("sequence: %s ordered against unknown %s", c.Action, o)
			}
		}
	}
	want := map[string]string{
		"SetModeSaved": "[SAVED_MODE]", "SetModeCustom": "custom", "SetModeDefault": "default",
		"SetHubDefault": "[DEFAULT_HUB]", "SetLoginDefault": "1",
		"SetRunSaved": "[SAVED_RUN]", "SetRunLegacy": "service", "SetRunDefault": "session",
	}
	for id, v := range want {
		if s, ok := sets[id]; !ok || s.Value != v {
			t.Errorf("SetProperty %s = %q, want %q", id, s.Value, v)
		}
	}
	if c := sets["SetModeCustom"].Condition; !strings.Contains(c, "HUB") || !strings.Contains(c, "JOIN_CODE") || !strings.Contains(c, "NOT DEFAULT_HUB") {
		t.Errorf("SetModeCustom: %q", c)
	}
	if c := sets["SetRunLegacy"].Condition; !strings.Contains(c, "SAVED_SERVICE_ACCOUNT") {
		t.Errorf("SetRunLegacy: %q (an older service install must keep its service)", c)
	}
	if c := sets["SetHubDefault"].Condition; !strings.Contains(c, `MODE="default"`) {
		t.Errorf("SetHubDefault: %q", c)
	}
	for _, comp := range p.Components {
		if len(comp.Services) > 0 || len(comp.Env) > 0 && comp.ID == "LongarmdService" {
			if !strings.Contains(comp.Condition, `RUN="service"`) || strings.Contains(comp.Condition, "NOT (") {
				t.Errorf("component %s (service) condition %q", comp.ID, comp.Condition)
			}
		}
		if comp.ID == "LongarmdStart" && !strings.Contains(comp.Condition, `RUN="service"`) {
			t.Errorf("LongarmdStart condition %q", comp.Condition)
		}
		if comp.ID == "LongarmdPlain" && comp.Condition != `NOT (IsWorker AND RUN="service")` {
			t.Errorf("LongarmdPlain condition %q", comp.Condition)
		}
	}
	seq := map[string]string{}
	for _, c := range p.Sequence {
		seq[c.Action] = c.Condition
	}
	for action, must := range map[string][]string{
		"LongarmdSetup":          {`RUN="service"`},
		"CheckServiceAccount":    {`RUN="service"`},
		"LongarmdSessionTask":    {`RUN="session"`, "IsWorker"},
		"LongarmdSessionJoin":    {`RUN="session"`, "JOIN_CODE"},
		"CheckSessionUser":       {`RUN="session"`, "S-1-5-18"},
		"CheckDefaultHub":        {`MODE="default"`, "NOT DEFAULT_HUB"},
		"CheckDefaultCode":       {`MODE="default"`, "JOIN_CODE"},
		"CheckMode":              {"MODE"},
		"CheckRun":               {"RUN", "per-user"},
		"CheckAdminWorker":       {`ADMIN_WORKER="1"`, "HUB"},
		"LongarmdAdminSetup":     {`ADMIN_WORKER="1"`},
		"LongarmdAdminRemove":    {`SAVED_ADMIN_WORKER="1"`, `REMOVE="ALL"`, "NOT UPGRADINGPRODUCTCODE"},
		"LongarmdAdminDrop":      {`SAVED_ADMIN_WORKER="1"`, `ADMIN_WORKER&lt;&gt;"1"`},
		"LongarmdSessionRemove":  {`REMOVE="ALL"`, "NOT UPGRADINGPRODUCTCODE"},
		"LongarmTrayLaunch":      {"LAUNCH_TRAY", "S-1-5-18"},
		"LongarmdCheck":          {"Installed", "$Settings<>2"},
		"LongarmdSettings":       {"$Settings=3"},
		"LongarmdSettingsRemove": {"$Settings=2", "NOT UPGRADINGPRODUCTCODE"},
	} {
		c, ok := seq[action]
		if !ok {
			t.Errorf("%s is not scheduled", action)
			continue
		}
		for _, m := range must {
			m = strings.NewReplacer("&lt;", "<", "&gt;", ">").Replace(m)
			if !strings.Contains(c, m) {
				t.Errorf("%s: condition %q without %s", action, c, m)
			}
		}
	}
}

// TestMSICustomActionData (static): every install-helper custom action of
// longarm.wxs passes its values as structured data after the marker -
// exactly the keys its step expects (longarmd.MSISteps), each "key=[PROP]"
// - and names no option, program or path of its own: Windows Installer
// formats properties into command lines unquoted, so a property must never
// be able to become an option (the HUB='x" --exe ...' repair).
func TestMSICustomActionData(t *testing.T) {
	b, err := os.ReadFile("../packaging/windows/longarm.wxs")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`ExeCommand='([^']*)'`)
	step := regexp.MustCompile(`install-helper msi ([a-z-]+) ` + regexp.QuoteMeta(longarmd.MSIDataMarker) + `(.*)$`)
	field := regexp.MustCompile(`^([a-z]+)=(\[[A-Za-z_]+\]|1)?$`)
	n := 0
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		cmd := m[1]
		if !strings.Contains(cmd, "install-helper") {
			continue
		}
		n++
		s := step.FindStringSubmatch(cmd)
		if s == nil {
			t.Errorf("install-helper action without structured data: %s", cmd)
			continue
		}
		want, ok := longarmd.MSISteps[s[1]]
		if !ok {
			t.Errorf("unknown step %s", s[1])
			continue
		}
		var got []string
		for _, f := range strings.Split(s[2], "|") {
			if f == "" {
				continue
			}
			fm := field.FindStringSubmatch(f)
			if fm == nil {
				t.Errorf("%s: field %q is not key=[PROPERTY]", s[1], f)
				continue
			}
			got = append(got, fm[1])
		}
		sort.Strings(got)
		w := append([]string(nil), want...)
		sort.Strings(w)
		if strings.Join(got, ",") != strings.Join(w, ",") {
			t.Errorf("%s: fields %v, the helper expects %v", s[1], got, w)
		}
		if before := cmd[:strings.Index(cmd, longarmd.MSIDataMarker)]; strings.Contains(before, "--") || strings.Count(before, "[") > 1 {
			t.Errorf("%s: options or properties before the data: %s", s[1], before)
		}
	}
	if n < 12 {
		t.Errorf("only %d install-helper actions found", n)
	}
	// who asked comes from UserSID (private, not settable from the command
	// line), never from a public property
	for _, s := range []string{"check", "setup", "session", "launch-tray", "admin-setup", "admin-drop"} {
		if !strings.Contains(string(b), "install-helper msi "+s+" ") {
			t.Errorf("no %s action", s)
		}
	}
	if strings.Contains(string(b), "usersid=[SESSION_SID]") || !strings.Contains(string(b), "usersid=[UserSID]") {
		t.Error("the requester must be [UserSID]")
	}
}

// TestMSIMaintenanceCheck (static): the administrator check of a repair or
// change does not depend on the public REMOVE property (REMOVE=ALL
// REINSTALL=ALL reinstalls everything, and would skip a REMOVE-based
// check) but on the Settings component's calculated action state: only a
// real uninstall (2, absent) skips it. And no public sign-in default
// reaches HKLM through the Registry table: Hub, CaFingerprint, Login,
// Labels and Mode are written by the helper (validated, admin-checked).
func TestMSIMaintenanceCheck(t *testing.T) {
	b, err := os.ReadFile("../packaging/windows/longarm.wxs")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	m := regexp.MustCompile(`<Custom Action="LongarmdCheck"[^>]*Condition='([^']*)'`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("LongarmdCheck not scheduled")
	}
	cond := strings.NewReplacer("&lt;", "<", "&gt;", ">").Replace(m[1])
	if strings.Contains(cond, "REMOVE") || strings.Contains(cond, "REINSTALL") || !strings.Contains(cond, "$Settings<>2") {
		t.Errorf("LongarmdCheck condition %q: must use the component state, not REMOVE/REINSTALL", cond)
	}
	// the condition, evaluated for the cases that matter
	run := func(installed bool, settingsState int) bool { return installed && settingsState != 2 }
	for _, c := range []struct {
		name      string
		installed bool
		state     int
		want      bool
	}{
		{"first install", false, 3, false},
		{"repair", true, 3, true},
		{"REMOVE=ALL REINSTALL=ALL (reinstalled)", true, 3, true},
		{"change without reinstall", true, -1, true},
		{"real uninstall", true, 2, false},
	} {
		if got := run(c.installed, c.state); got != c.want {
			t.Errorf("%s: check runs %v, want %v", c.name, got, c.want)
		}
	}
	comp := regexp.MustCompile(`(?s)<Component Id="Settings".*?</Component>`).FindString(src)
	for _, name := range []string{"Hub", "CaFingerprint", "Login", "Labels", "Mode"} {
		if strings.Contains(comp, `Name="`+name+`"`) {
			t.Errorf("the Settings component writes %s from a property (the helper must)", name)
		}
	}
	if !strings.Contains(src, "install-helper msi settings #LONGARM-MSI#hub=[HUB]|ca=[CA_FINGERPRINT]") {
		t.Error("no LongarmdSettings action")
	}
}

// TestMSIRoleChoices (static): the Default page says what each role
// installs, the admin worker comes only with a worker role (both pages
// disable the checkbox for a client, Next never sets ADMIN_WORKER for one,
// a silent ROLES=client ADMIN_WORKER=1 is refused - except an install from
// before that rule that already has it), and the Finish page lists what
// was installed (with the tray icon's hiding place and the new terminal
// PATH needs).
func TestMSIRoleChoices(t *testing.T) {
	b, err := os.ReadFile("../packaging/windows/longarm.wxs")
	if err != nil {
		t.Fatal(err)
	}
	src := strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "", "&quot;", `"`).Replace(string(b))
	dialog := func(id string) string {
		m := regexp.MustCompile(`(?s)<Dialog Id="` + id + `".*?</Dialog>`).FindString(src)
		if m == "" {
			t.Fatalf("no dialog %s", id)
		}
		return m
	}
	mode := dialog("ModeDlg")
	for _, want := range []string{
		"This is my everyday computer: send jobs to workers",
		"Installs the client: longarm and the tray",
		"This machine runs jobs (Dev Box, build machine)",
		"It becomes your worker, and you can also send jobs from it",
		"starts at your logon, no password",
		"even before anyone logs on (runs as SYSTEM, owner-only)",
	} {
		if !strings.Contains(mode, want) {
			t.Errorf("ModeDlg without %q", want)
		}
	}
	control := func(dlg, id string) string {
		m := regexp.MustCompile(`(?s)<Control Id="` + id + `".*?/>`).FindString(dlg)
		if m == "" {
			t.Fatalf("no control %s", id)
		}
		return m
	}
	if c := control(mode, "Admin"); !strings.Contains(c, `DisableCondition='MODE_UI<>"default" OR DEFAULT_ROLES="client"'`) {
		t.Errorf("ModeDlg Admin: %s", c)
	}
	if c := control(dialog("RolesDlg"), "Admin"); !strings.Contains(c, `DisableCondition='ROLES="client"'`) {
		t.Errorf("RolesDlg Admin: %s", c)
	}
	for _, m := range regexp.MustCompile(`<Publish Dialog="ModeDlg" Control="Next" Property="ADMIN_WORKER" Value="1"[^\n]*/>`).FindAllString(src, -1) {
		if !strings.Contains(m, `DEFAULT_ROLES<>"client"`) {
			t.Errorf("ModeDlg sets ADMIN_WORKER for a client: %s", m)
		}
	}
	if !regexp.MustCompile(`<Publish Dialog="RolesDlg" Control="Next" Property="ADMIN_WORKER" Value="\{\}"[^>]*ROLES="client"`).MatchString(src) {
		t.Error("RolesDlg keeps ADMIN_WORKER for a client")
	}
	m := regexp.MustCompile(`<Custom Action="CheckAdminRole"[^>]*Condition='([^']*)'`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("CheckAdminRole not scheduled")
	}
	for _, want := range []string{`ADMIN_WORKER="1"`, "NOT IsWorker", `SAVED_ADMIN_WORKER<>"1"`, `NOT REMOVE="ALL"`} {
		if !strings.Contains(m[1], want) {
			t.Errorf("CheckAdminRole condition %q without %s", m[1], want)
		}
	}
	// the condition, for the cases that matter
	refused := func(admin string, worker bool, saved string) bool { return admin == "1" && !worker && saved != "1" }
	if !refused("1", false, "") || refused("1", true, "") || refused("1", false, "1") || refused("", false, "") {
		t.Error("CheckAdminRole logic")
	}
	// Finish
	fin := regexp.MustCompile(`(?s)<SetProperty Id="WIXUI_EXITDIALOGOPTIONALTEXT"[^>]*/>`).FindString(src)
	for _, want := range []string{"[FINISH_ROLES]", "[FINISH_SIGNIN]", "^ arrow", "new terminal", `NOT REMOVE="ALL"`} {
		if !strings.Contains(fin, want) {
			t.Errorf("Finish text without %q: %s", want, fin)
		}
	}
	for _, id := range []string{"SetFinishClient", "SetFinishBoth", "SetFinishWorker", "SetFinishBothSvc", "SetFinishWorkerSvc", "SetFinishAdmin", "SetFinishSignIn"} {
		if !strings.Contains(src, `Action="`+id+`"`) {
			t.Errorf("no %s", id)
		}
	}
}

// TestMSICloudPCDetection (static): the Default page preselects "This
// machine runs jobs" on a Cloud PC / Dev Box - by the same computer name
// prefix and Dev Box agent services as longarmd.LooksLikeCloudPC (the
// tray's hint), with read-only registry searches - in the UI sequence only
// (a silent install keeps ROLES=client unless given) and never over
// recorded roles.
func TestMSICloudPCDetection(t *testing.T) {
	b, err := os.ReadFile("../packaging/windows/longarm.wxs")
	if err != nil {
		t.Fatal(err)
	}
	src := strings.NewReplacer("&lt;", "<", "&gt;", ">").Replace(string(b))
	set := func(id string) string {
		m := regexp.MustCompile(`(?s)<SetProperty Id="[A-Z_]+"( Action="`+id+`")?[^>]*/>`).FindAllString(src, -1)
		for _, x := range m {
			if strings.Contains(x, `Action="`+id+`"`) || !strings.Contains(x, "Action=") && strings.Contains(x, `Id="`+strings.TrimPrefix(id, "Set")+`"`) {
				return x
			}
		}
		t.Fatalf("no SetProperty %s", id)
		return ""
	}
	cloud := set("SetCLOUD_PC")
	if !strings.Contains(cloud, `ComputerName ~<< "`+longarmd.CloudPCNamePrefix+`"`) || !strings.Contains(cloud, `Sequence="ui"`) {
		t.Errorf("CLOUD_PC: %s", cloud)
	}
	for _, svc := range longarmd.DevBoxAgentServices {
		re := regexp.MustCompile(`<Property Id="([A-Z_]+)">\s*<RegistrySearch [^>]*Key="` + regexp.QuoteMeta(`SYSTEM\CurrentControlSet\Services\`+svc) + `" Name="ImagePath" Type="raw"`)
		m := re.FindStringSubmatch(src)
		if m == nil {
			t.Errorf("no registry search for the service %s", svc)
			continue
		}
		if !strings.Contains(cloud, m[1]) {
			t.Errorf("CLOUD_PC does not use %s", m[1])
		}
	}
	for _, id := range []string{"SetDefaultRolesCloud", "SetRolesCloud"} {
		x := set(id)
		if !strings.Contains(x, `Value="both"`) || !strings.Contains(x, `Sequence="ui"`) || !strings.Contains(x, "NOT ROLES_RECORDED") || !strings.Contains(x, "NOT Installed") {
			t.Errorf("%s: %s", id, x)
		}
	}
	if !regexp.MustCompile(`<Property Id="ROLES_RECORDED">\s*<RegistrySearch [^>]*Name="Roles"`).MatchString(src) {
		t.Error("ROLES_RECORDED does not search the recorded roles")
	}
}
