package e2e

import (
	"encoding/xml"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/lubobill1990/longarm/internal/longarmd"
	"github.com/lubobill1990/longarm/internal/msiupgrade"
	"github.com/lubobill1990/longarm/internal/updatesvc"
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
	actions := map[string]bool{"CostFinalize": true, "InstallInitialize": true, "InstallServices": true, "StopServices": true,
		"InstallValidate": true, "RemoveExistingProducts": true, "InstallFinalize": true}
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
		// (the worker service; the update service is every install's: TestMSIUpdateService)
		if comp.ID != "UpdateService" && (len(comp.Services) > 0 || len(comp.Env) > 0 && comp.ID == "LongarmdService") {
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

// TestMSIUpgradeRunning (static): an upgrade with Longarm running shows no
// "files in use" dialog and needs no reboot (internal/msiupgrade). Restart
// Manager is off; the upgrade helper comes from the Binary table (it must
// run before the installed files are replaced) and gets UserSID only; it
// stops the installing user's tray before InstallValidate (where the
// in-use check runs), everything else elevated before StopServices (with a
// rollback step before it), on an upgrade and on an uninstall; the new
// longarmd starts again what ran, at the end. None runs as the old product
// of a later upgrade. The worker services stop (and are waited for) on
// upgrades, repairs and uninstalls.
//
// The major upgrade installs over the old product and removes it afterwards
// (afterInstallExecute): with afterInstallInitialize, RemoveExistingProducts
// must come before every action that writes to the install script, and the
// elevated stop step is one - error 2613, every upgrade failed (0.1.8 to
// 0.1.13). Nothing is scheduled relative to RemoveExistingProducts (WiX
// places it); msi-sequence-check.ps1 checks the built table.
func TestMSIUpgradeRunning(t *testing.T) {
	b, err := os.ReadFile("../packaging/windows/longarm.wxs")
	if err != nil {
		t.Fatal(err)
	}
	src := regexp.MustCompile(`<\?[^>]*\?>`).ReplaceAll(b, nil)
	src = append([]byte(`<?xml version="1.0" encoding="utf-8"?>`), src...)
	type attrs struct {
		ID          string `xml:"Id,attr"`
		Action      string `xml:"Action,attr"`
		Value       string `xml:"Value,attr"`
		Before      string `xml:"Before,attr"`
		After       string `xml:"After,attr"`
		Condition   string `xml:"Condition,attr"`
		BinaryRef   string `xml:"BinaryRef,attr"`
		SameVer     string `xml:"AllowSameVersionUpgrades,attr"`
		Execute     string `xml:"Execute,attr"`
		Impersonate string `xml:"Impersonate,attr"`
		Return      string `xml:"Return,attr"`
		ExeCommand  string `xml:"ExeCommand,attr"`
		SourceFile  string `xml:"SourceFile,attr"`
		Schedule    string `xml:"Schedule,attr"`
		Name        string `xml:"Name,attr"`
		Stop        string `xml:"Stop,attr"`
		Wait        string `xml:"Wait,attr"`
	}
	var doc struct {
		Package struct {
			Upgrade    attrs   `xml:"MajorUpgrade"`
			Properties []attrs `xml:"Property"`
			Binaries   []attrs `xml:"Binary"`
			Actions    []attrs `xml:"CustomAction"`
			Controls   []attrs `xml:"ComponentGroup>Component>ServiceControl"`
			Sequence   []attrs `xml:"InstallExecuteSequence>Custom"`
		} `xml:"Package"`
	}
	if err := xml.Unmarshal(src, &doc); err != nil {
		t.Fatal(err)
	}
	p := doc.Package
	if p.Upgrade.Schedule != "afterInstallExecute" {
		t.Errorf("MajorUpgrade Schedule %q: the new version installs over the old one, which is removed afterwards (afterInstallExecute)", p.Upgrade.Schedule)
	}
	// the Longarm icon: Add/Remove Programs (packaging/icons)
	if !strings.Contains(string(src), `<Property Id="ARPPRODUCTICON" Value="LongarmIcon" />`) ||
		!strings.Contains(string(src), `<Icon Id="LongarmIcon" SourceFile="$(sys.SOURCEFILEDIR)..\icons\longarm.ico" />`) {
		t.Error("ARPPRODUCTICON: the MSI shows the Longarm icon (packaging/icons/longarm.ico)")
	}
	if p.Upgrade.SameVer != "yes" {
		t.Errorf("MajorUpgrade AllowSameVersionUpgrades %q: dev builds and release candidates share their MSI version (a.b.c) and must replace each other", p.Upgrade.SameVer)
	}
	for _, c := range p.Sequence {
		if c.Before == "RemoveExistingProducts" || c.After == "RemoveExistingProducts" {
			t.Errorf("%s is scheduled relative to RemoveExistingProducts (a deferred action before it is error 2613)", c.Action)
		}
	}
	rm := false
	for _, pr := range p.Properties {
		if pr.ID == "MSIRESTARTMANAGERCONTROL" && pr.Value == "Disable" {
			rm = true
		}
	}
	if !rm {
		t.Error("MSIRESTARTMANAGERCONTROL is not Disable")
	}
	if len(p.Binaries) != 1 || p.Binaries[0].ID != "LongarmMsiHelper" || !strings.HasSuffix(p.Binaries[0].SourceFile, `\longarm-msihelper.exe`) {
		t.Errorf("Binary table: %+v", p.Binaries)
	}
	ca := map[string]attrs{}
	for _, a := range p.Actions {
		ca[a.ID] = a
	}
	seq := map[string]attrs{}
	for _, c := range p.Sequence {
		seq[c.Action] = c
	}
	for id, want := range map[string]struct{ step, execute, before string }{
		"LongarmStopTray":            {"stop-tray", "immediate", "InstallValidate"},
		"LongarmUpgradeStop":         {"stop", "deferred", "StopServices"},
		"LongarmUpgradeStopRollback": {"rollback", "rollback", "LongarmUpgradeStop"},
		"LongarmRemoveStop":          {"stop-remove", "deferred", "StopServices"},
	} {
		a, ok := ca[id]
		if !ok {
			t.Errorf("no custom action %s", id)
			continue
		}
		if a.BinaryRef != "LongarmMsiHelper" || a.Execute != want.execute || a.Return != "ignore" {
			t.Errorf("%s: %+v", id, a)
		}
		if want.execute != "immediate" && a.Impersonate != "no" {
			t.Errorf("%s runs impersonated", id)
		}
		step, data, ok := strings.Cut(a.ExeCommand, " "+msiupgrade.DataMarker)
		if !ok || step != want.step {
			t.Errorf("%s: command %q", id, a.ExeCommand)
			continue
		}
		var keys []string
		for _, f := range strings.Split(data, "|") {
			k, v, _ := strings.Cut(f, "=")
			if v != "[UserSID]" {
				t.Errorf("%s: field %q (the helper gets UserSID only)", id, f)
			}
			keys = append(keys, k)
		}
		if strings.Join(keys, ",") != strings.Join(msiupgrade.Steps[step], ",") {
			t.Errorf("%s: fields %v, the helper expects %v", id, keys, msiupgrade.Steps[step])
		}
		s, ok := seq[id]
		if !ok || s.Before != want.before {
			t.Errorf("%s: scheduled %+v, want before %s", id, s, want.before)
		}
		if !strings.Contains(s.Condition, "NOT UPGRADINGPRODUCTCODE") {
			t.Errorf("%s: condition %q runs as the old product of an upgrade", id, s.Condition)
		}
	}
	if c := seq["LongarmStopTray"].Condition; !strings.Contains(c, "UserSID=SESSION_SID") || !strings.Contains(c, "WIX_UPGRADE_DETECTED") {
		t.Errorf("LongarmStopTray condition %q", c)
	}
	for _, id := range []string{"LongarmUpgradeStop", "LongarmUpgradeStopRollback", "LongarmdUpgradeRestart"} {
		if c := seq[id].Condition; !strings.Contains(c, "WIX_UPGRADE_DETECTED") || !strings.Contains(c, `NOT REMOVE="ALL"`) {
			t.Errorf("%s condition %q", id, c)
		}
	}
	if r := seq["LongarmdUpgradeRestart"]; r.Before != "InstallFinalize" || !strings.Contains(ca["LongarmdUpgradeRestart"].ExeCommand, "install-helper msi restart ") ||
		ca["LongarmdUpgradeRestart"].Impersonate != "no" {
		t.Errorf("LongarmdUpgradeRestart: %+v %+v", r, ca["LongarmdUpgradeRestart"])
	}
	stops := map[string]bool{}
	for _, c := range p.Controls {
		if c.Stop == "both" && c.Wait == "yes" {
			stops[c.ID] = true
		}
	}
	if !stops["AdminSvcStop"] || !stops["LongarmdSvcCtl"] {
		t.Errorf("the admin worker's and the worker's service must stop on install and uninstall and be waited for: %+v", p.Controls)
	}
}

// TestMSIUpdateService (static): every install has the Longarm Update
// service - unconditional, LocalSystem, from the install dir, auto start -
// stopped and waited for before its file changes, removed on uninstall,
// started without waiting (an install never fails for it); the names match
// what internal/updatesvc and its clients expect.
func TestMSIUpdateService(t *testing.T) {
	b, err := os.ReadFile("../packaging/windows/longarm.wxs")
	if err != nil {
		t.Fatal(err)
	}
	src := regexp.MustCompile(`<\?[^>]*\?>`).ReplaceAll(b, nil)
	src = append([]byte(`<?xml version="1.0" encoding="utf-8"?>`), src...)
	type control struct {
		ID     string `xml:"Id,attr"`
		Name   string `xml:"Name,attr"`
		Start  string `xml:"Start,attr"`
		Stop   string `xml:"Stop,attr"`
		Remove string `xml:"Remove,attr"`
		Wait   string `xml:"Wait,attr"`
	}
	type component struct {
		ID        string `xml:"Id,attr"`
		Condition string `xml:"Condition,attr"`
		File      []struct {
			Name   string `xml:"Name,attr"`
			Source string `xml:"Source,attr"`
		} `xml:"File"`
		Service []struct {
			Name      string `xml:"Name,attr"`
			Account   string `xml:"Account,attr"`
			Arguments string `xml:"Arguments,attr"`
			Start     string `xml:"Start,attr"`
			Type      string `xml:"Type,attr"`
		} `xml:"ServiceInstall"`
		Controls []control `xml:"ServiceControl"`
		RegKeys  []struct {
			Key    string `xml:"Key,attr"`
			Action string `xml:"Action,attr"`
		} `xml:"RemoveRegistryKey"`
	}
	var doc struct {
		Package struct {
			Components []component `xml:"ComponentGroup>Component"`
		} `xml:"Package"`
	}
	if err := xml.Unmarshal(src, &doc); err != nil {
		t.Fatal(err)
	}
	var c *component
	for i := range doc.Package.Components {
		if doc.Package.Components[i].ID == "UpdateService" {
			c = &doc.Package.Components[i]
		}
	}
	if c == nil {
		t.Fatal("no UpdateService component")
	}
	if c.Condition != "" {
		t.Errorf("the update service is conditional (%q): every install has it", c.Condition)
	}
	if len(c.File) != 1 || c.File[0].Name != updatesvc.BinaryName || !strings.HasSuffix(c.File[0].Source, `\`+updatesvc.BinaryName) {
		t.Errorf("file %+v", c.File)
	}
	if len(c.Service) != 1 || c.Service[0].Name != updatesvc.ServiceName || c.Service[0].Account != "LocalSystem" ||
		c.Service[0].Arguments != "run" || c.Service[0].Start != "auto" || c.Service[0].Type != "ownProcess" {
		t.Errorf("service %+v", c.Service)
	}
	var stop, start bool
	for _, sc := range c.Controls {
		if sc.Name != updatesvc.ServiceName {
			t.Errorf("control %+v names another service", sc)
		}
		if sc.Stop == "both" && sc.Remove == "uninstall" && sc.Wait == "yes" {
			stop = true
		}
		if sc.Start == "install" && sc.Wait == "no" {
			start = true
		}
	}
	if !stop || !start {
		t.Errorf("controls %+v: stop (both, waited, removed on uninstall) and start (install, not waited) wanted", c.Controls)
	}
	if len(c.RegKeys) != 1 || c.RegKeys[0].Key != `Software\Longarm\Update` || c.RegKeys[0].Action != "removeOnUninstall" {
		t.Errorf("the auto-update setting is not removed on uninstall: %+v", c.RegKeys)
	}
}

// TestMSIMajorUpgradeInPlace (static): the new version installs over the
// old one and RemoveExistingProducts uninstalls the old product afterwards
// (afterInstallExecute). That is only safe when
//   - every component keeps the GUID (and key file) every released version
//     has, so the old product's uninstall leaves the shared components alone
//     ("another client exists") instead of deleting the new files;
//   - no uninstall step (leave the hub, remove tasks, services, data) runs
//     as the old product of an upgrade (NOT UPGRADINGPRODUCTCODE);
//   - longarmd.exe never moves between its two components on an upgrade
//     (CheckLongarmdSwitch, from component searches of the same GUIDs).
func TestMSIMajorUpgradeInPlace(t *testing.T) {
	b, err := os.ReadFile("../packaging/windows/longarm.wxs")
	if err != nil {
		t.Fatal(err)
	}
	src := regexp.MustCompile(`<\?[^>]*\?>`).ReplaceAll(b, nil)
	src = append([]byte(`<?xml version="1.0" encoding="utf-8"?>`), src...)
	type file struct {
		ID   string `xml:"Id,attr"`
		Name string `xml:"Name,attr"`
	}
	type component struct {
		ID    string `xml:"Id,attr"`
		GUID  string `xml:"Guid,attr"`
		Dir   string `xml:"Directory,attr"`
		Files []file `xml:"File"`
	}
	type search struct {
		GUID string `xml:"Guid,attr"`
		Type string `xml:"Type,attr"`
	}
	type property struct {
		ID     string  `xml:"Id,attr"`
		Search *search `xml:"ComponentSearch"`
	}
	type custom struct {
		Action    string `xml:"Action,attr"`
		Before    string `xml:"Before,attr"`
		Condition string `xml:"Condition,attr"`
	}
	var doc struct {
		Package struct {
			Components []component `xml:"ComponentGroup>Component"`
			Properties []property  `xml:"Property"`
			Actions    []struct {
				ID    string `xml:"Id,attr"`
				Error string `xml:"Error,attr"`
			} `xml:"CustomAction"`
			Sequence []custom `xml:"InstallExecuteSequence>Custom"`
		} `xml:"Package"`
	}
	if err := xml.Unmarshal(src, &doc); err != nil {
		t.Fatal(err)
	}
	p := doc.Package
	// Component -> GUID ("" = WiX's, from the key file's path: the file
	// must keep its Id, Name and directory) and key file, as released
	// (v0.1.2 to v0.1.13; UpdateService since v0.1.13).
	want := map[string][2]string{
		"LongarmExe":      {"", "longarm.exe"},
		"UpdateService":   {"24b9f1c6-dc48-4bbc-8624-db45f8e3cee6", "longarm_update.exe"},
		"LongarmdService": {"3e96d676-1a38-455f-a841-7b7642e651c8", "longarmd.exe"},
		"LongarmdPlain":   {"0afe2b69-cb68-4395-a6fb-5a51b76aef5b", "longarmd.plain.exe"},
		"LongarmdStart":   {"5380e22b-7c1c-4756-8c00-1616ee2b4647", ""},
		"Settings":        {"165786a7-faa0-43e1-9643-f87e8ab45911", ""},
		"Tray":            {"", "longarm_tray.exe"},
		"TrayShortcut":    {"936cbf15-9d7c-4e3d-8b7a-173b2bb11b9d", ""},
	}
	got := map[string]component{}
	for _, c := range p.Components {
		got[c.ID] = c
		w, ok := want[c.ID]
		if !ok {
			t.Errorf("component %s is new: add it here (a new component is fine; a changed GUID or key file is not)", c.ID)
			continue
		}
		if !strings.EqualFold(c.GUID, w[0]) {
			t.Errorf("component %s: Guid %q, released versions have %q (the old product's uninstall would delete the new files)", c.ID, c.GUID, w[0])
		}
		if w[1] != "" && (len(c.Files) != 1 || c.Files[0].ID != w[1]) {
			t.Errorf("component %s: files %+v, released versions have %s", c.ID, c.Files, w[1])
		}
		if w[0] == "" && c.Dir != "" {
			t.Errorf("component %s (generated Guid) moved to %s", c.ID, c.Dir)
		}
	}
	for id := range want {
		if _, ok := got[id]; !ok {
			t.Errorf("no component %s", id)
		}
	}
	for _, c := range p.Sequence {
		cond := strings.ReplaceAll(c.Condition, `NOT REMOVE="ALL"`, "")
		if strings.Contains(cond, `REMOVE="ALL"`) && !strings.Contains(cond, "NOT UPGRADINGPRODUCTCODE") {
			t.Errorf("%s (%q) would run as the old product of an upgrade", c.Action, c.Condition)
		}
	}
	searches := map[string]string{}
	for _, pr := range p.Properties {
		if pr.Search != nil {
			if pr.Search.Type != "file" {
				t.Errorf("%s: ComponentSearch Type %q", pr.ID, pr.Search.Type)
			}
			searches[pr.ID] = pr.Search.GUID
		}
	}
	if !strings.EqualFold(searches["PREV_LONGARMD_SERVICE"], got["LongarmdService"].GUID) || !strings.EqualFold(searches["PREV_LONGARMD_PLAIN"], got["LongarmdPlain"].GUID) {
		t.Errorf("component searches %v do not match the longarmd components", searches)
	}
	var sw *custom
	for i, c := range p.Sequence {
		if c.Action == "CheckLongarmdSwitch" {
			sw = &p.Sequence[i]
		}
	}
	if sw == nil || sw.Before != "LongarmStopTray" { // before anything is stopped
		t.Fatalf("CheckLongarmdSwitch: %+v", sw)
	}
	for _, s := range []string{"WIX_UPGRADE_DETECTED", `NOT REMOVE="ALL"`, `PREV_LONGARMD_SERVICE AND NOT (IsWorker AND RUN="service")`, `PREV_LONGARMD_PLAIN AND IsWorker AND RUN="service"`} {
		if !strings.Contains(sw.Condition, s) {
			t.Errorf("CheckLongarmdSwitch condition %q lacks %s", sw.Condition, s)
		}
	}
	found := false
	for _, a := range p.Actions {
		if a.ID == "CheckLongarmdSwitch" && strings.Contains(a.Error, "uninstall first") {
			found = true
		}
	}
	if !found {
		t.Error("CheckLongarmdSwitch: no error message telling what to do")
	}
}
