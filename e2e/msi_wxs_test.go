package e2e

import (
	"encoding/xml"
	"os"
	"regexp"
	"strings"
	"testing"
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
		"LongarmdSetup":         {`RUN="service"`},
		"CheckServiceAccount":   {`RUN="service"`},
		"LongarmdSessionTask":   {`RUN="session"`, "IsWorker"},
		"LongarmdSessionJoin":   {`RUN="session"`, "JOIN_CODE"},
		"CheckSessionUser":      {`RUN="session"`, "S-1-5-18"},
		"CheckDefaultHub":       {`MODE="default"`, "NOT DEFAULT_HUB"},
		"CheckDefaultCode":      {`MODE="default"`, "JOIN_CODE"},
		"CheckMode":             {"MODE"},
		"CheckRun":              {"RUN", "per-user"},
		"CheckAdminWorker":      {`ADMIN_WORKER="1"`, "HUB"},
		"LongarmdAdminSetup":    {`ADMIN_WORKER="1"`},
		"LongarmdAdminRemove":   {`SAVED_ADMIN_WORKER="1"`, `REMOVE="ALL"`, "NOT UPGRADINGPRODUCTCODE"},
		"LongarmdAdminDrop":     {`SAVED_ADMIN_WORKER="1"`, `ADMIN_WORKER&lt;&gt;"1"`},
		"LongarmdSessionRemove": {`REMOVE="ALL"`, "NOT UPGRADINGPRODUCTCODE"},
		"LongarmTrayLaunch":     {"LAUNCH_TRAY", "S-1-5-18"},
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
