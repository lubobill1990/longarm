package longarmd

import (
	"strings"
	"testing"
)

const (
	alice = "S-1-12-1-111-222-333-444"
	bob   = "S-1-5-21-1-2-3-1002"
)

// TestPlanRoleChange: the rules of a role change from the tray (see
// PlanRoleChange) - what is added and removed, what is recorded, and what
// is refused.
func TestPlanRoleChange(t *testing.T) {
	client := MachineRoles{Installed: true, Client: true, SessionSID: alice, RunMode: "session"}
	both := MachineRoles{Installed: true, Client: true, Worker: true, SessionSID: alice, RunMode: "session"}
	bothAdmin := both
	bothAdmin.Admin = true
	legacy := client // v0.1.3 "Use this computer" + the admin checkbox
	legacy.Admin = true
	for _, c := range []struct {
		name string
		cur  MachineRoles
		want RoleChange
		plan RolePlan
		err  string
	}{
		{name: "client adds the worker", cur: client, want: RoleChange{Client: true, Worker: true, SID: alice},
			plan: RolePlan{AddWorker: true, Roles: "both", SessionSID: alice}},
		{name: "client adds worker and admin worker", cur: client, want: RoleChange{Client: true, Worker: true, Admin: true, SID: alice},
			plan: RolePlan{AddWorker: true, AddAdmin: true, Roles: "both", SessionSID: alice}},
		{name: "admin worker without the worker", cur: client, want: RoleChange{Client: true, Admin: true, SID: alice}, err: "comes with the worker role"},
		{name: "legacy client + admin keeps it", cur: legacy, want: RoleChange{Client: true, Admin: true, SID: alice},
			plan: RolePlan{Roles: "client", SessionSID: alice}},
		{name: "legacy adds the worker", cur: legacy, want: RoleChange{Client: true, Worker: true, Admin: true, SID: alice},
			plan: RolePlan{AddWorker: true, Roles: "both", SessionSID: alice}},
		{name: "both drops the client", cur: both, want: RoleChange{Worker: true, SID: alice},
			plan: RolePlan{Roles: "worker", SessionSID: alice}},
		{name: "both drops the worker and the admin", cur: bothAdmin, want: RoleChange{Client: true, SID: alice},
			plan: RolePlan{RemoveWorker: true, RemoveAdmin: true, Roles: "client", SessionSID: alice}},
		{name: "nothing left", cur: both, want: RoleChange{SID: alice}, err: "at least one role"},
		{name: "not installed", cur: MachineRoles{}, want: RoleChange{Client: true, SID: alice}, err: "install the MSI"},
		{name: "SYSTEM", cur: client, want: RoleChange{Client: true, Worker: true, SID: "S-1-5-18"}, err: "system account"},
		{name: "a service worker", cur: MachineRoles{Installed: true, Client: true, SessionSID: alice, RunMode: "service"},
			want: RoleChange{Client: true, Worker: true, SID: alice}, err: "Windows service"},
		{name: "another user's worker stays theirs", cur: both, want: RoleChange{Client: true, SID: bob}, err: "only that user"},
		{name: "another user adds a worker where the installer has none", cur: client, want: RoleChange{Client: true, Worker: true, SID: bob},
			plan: RolePlan{AddWorker: true, Roles: "both", SessionSID: bob, DropTrayTaskOf: alice}},
		{name: "not where the installer has an admin worker", cur: legacy, want: RoleChange{Client: true, Worker: true, Admin: true, SID: bob}, err: "another user"},
		{name: "admin worker of another user's worker", cur: both, want: RoleChange{Client: true, Worker: true, Admin: true, SID: bob}, err: "user whose worker runs here"},
		{name: "deployed as SYSTEM: the first user's worker", cur: MachineRoles{Installed: true, Client: true, SessionSID: "S-1-5-18", RunMode: "session"},
			want: RoleChange{Client: true, Worker: true, SID: bob}, plan: RolePlan{AddWorker: true, Roles: "both", SessionSID: bob}},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, err := PlanRoleChange(c.cur, c.want)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err %v, want %q", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if p != c.plan {
				t.Fatalf("plan %+v, want %+v", p, c.plan)
			}
		})
	}
	if p, _ := PlanRoleChange(both, RoleChange{Client: true, Worker: true, SID: alice}); !p.Empty(both) {
		t.Errorf("no change: %+v", p)
	}
}

// TestRolesArgs: the tray's elevated command line parses as the helper's
// strict data, and an injected value never does.
func TestRolesArgs(t *testing.T) {
	args := RolesArgs(RoleChange{Client: true, Worker: true, SID: alice})
	if args[0] != "install-helper" || args[1] != "roles" {
		t.Fatalf("args %v", args)
	}
	d, err := MSIData(`"C:\Program Files\Longarm\longarmd.exe" `+strings.Join(args, " "), RolesKeys...)
	if err != nil {
		t.Fatal(err)
	}
	if d["client"] != "1" || d["worker"] != "1" || d["admin"] != "" || d["sid"] != alice {
		t.Fatalf("data %v", d)
	}
	for _, bad := range []string{
		"client=1|worker=1|admin=|sid=" + alice + "|sid=" + bob,
		"client=yes|worker=1|admin=|sid=" + alice,
		`client=1|worker=1|admin=|sid=S-1-5 --exe C:\evil.exe`,
		"client=1|worker=1|sid=" + alice,
	} {
		if _, err := MSIData("longarmd install-helper roles "+MSIDataMarker+bad, RolesKeys...); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// TestLoadMachineRoles: the MSI's record, including an install from before
// RunMode (a service).
func TestLoadMachineRoles(t *testing.T) {
	s := &memSettings{}
	if m := LoadMachineRoles(s); m.Installed {
		t.Fatalf("empty: %+v", m)
	}
	s.Set(RegRoles, "both")
	s.Set(RegSessionUserSID, alice)
	s.Set(RegAdminWorker, "1")
	s.Set("ServiceAccount", `.\alice`)
	m := LoadMachineRoles(s)
	if !m.Installed || !m.Client || !m.Worker || !m.Admin || m.SessionSID != alice || m.RunMode != "service" {
		t.Fatalf("%+v", m)
	}
	s.Set(RegRunMode, "session")
	if m := LoadMachineRoles(s); m.RunMode != "session" {
		t.Fatalf("%+v", m)
	}
}

type fakeProbe struct {
	name     string
	services map[string]bool
}

func (p fakeProbe) ComputerName() string           { return p.name }
func (p fakeProbe) ServiceExists(name string) bool { return p.services[name] }

// TestLooksLikeCloudPC: Windows 365 Cloud PCs and Dev Boxes (CPC-...
// names, the Dev Box agent's service) are suggested as workers; an
// ordinary laptop is not.
func TestLooksLikeCloudPC(t *testing.T) {
	for _, c := range []struct {
		p    fakeProbe
		want bool
	}{
		{fakeProbe{name: "CPC-alice-AB12C"}, true},
		{fakeProbe{name: "cpc-bob-1"}, true},
		{fakeProbe{name: "DEVBOX-7", services: map[string]bool{"Microsoft.DevCenter.DevBoxAgent": true}}, true},
		{fakeProbe{name: "box", services: map[string]bool{"DevBoxAgent": true}}, true},
		{fakeProbe{name: "ALICE-LAPTOP"}, false},
		{fakeProbe{name: "CPC"}, false},
		{fakeProbe{name: "MYCPC-1"}, false},
	} {
		got, why := LooksLikeCloudPC(c.p)
		if got != c.want || got && why == "" {
			t.Errorf("%+v: %v %q", c.p, got, why)
		}
	}
}
