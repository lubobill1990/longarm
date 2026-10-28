package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lubobill1990/longarm/internal/longarmd"
)

type mapSettings map[string]string

func (m mapSettings) Get(n string) (string, bool) { v, ok := m[n]; return v, ok }
func (m mapSettings) Set(n, v string) error       { m[n] = v; return nil }
func (m mapSettings) Delete(n string) error       { delete(m, n); return nil }

// TestRolesHelper: `install-helper roles` (the tray's role change, run
// elevated) refuses what the MSI's custom actions refuse - a caller that
// is not an elevated administrator, injected data, the admin worker for a
// non-administrator, an admin worker without the worker role - and
// otherwise changes the machine and the MSI's record together: the
// user-session task for the tray's user, the admin worker from HKLM's hub
// (never from the command line), Roles / SessionUserSID / AdminWorker.
func TestRolesHelper(t *testing.T) {
	const user, admin = "S-1-5-21-1-2-3-1001", "S-1-5-21-1-2-3-500"
	type call struct{ what, arg string }
	var calls []call
	var self = admin
	var elevated = true
	defer func() { testRoleOps = roleOps{} }()
	testRoleOps = roleOps{
		elevated: func() bool { return elevated },
		selfSID:  func() (string, error) { return self, nil },
		adminSetup: func(_ context.Context, _ io.Writer, o adminSetupOptions) error {
			calls = append(calls, call{"admin-setup", o.sid + " " + o.hub + " " + o.exe})
			return nil
		},
		adminRemove: func(context.Context, io.Writer, string, bool) error {
			calls = append(calls, call{"admin-remove", ""})
			return nil
		},
		leave: func(_ context.Context, dir string, _ io.Writer) error {
			calls = append(calls, call{"leave", dir})
			return nil
		},
		fallbackHub: func() (string, string, string) { return "longarm.example.com", "", "installer" },
		userDir:     func(sid string) (string, error) { return `C:\Users\` + sid + `\.longarm`, nil },
	}
	sched := &nopSched{}
	reg := mapSettings{}
	ownDir := `C:\Program Files\Longarm`
	env := msiEnv{
		isAdmin:     func(sid string) (bool, error) { return sid == admin, nil },
		userSID:     func(string) error { return nil },
		ownDir:      func() (string, error) { return ownDir, nil },
		programData: func() string { return `C:\ProgramData\Longarm` },
		sched:       sched,
		settings:    reg,
	}
	run := func(data string) error {
		var out bytes.Buffer
		return runRolesStep(context.Background(), `"C:\Program Files\Longarm\longarmd.exe" install-helper roles `+longarmd.MSIDataMarker+data, env, &out)
	}
	reset := func(roles, admin string) {
		calls, sched.calls = nil, nil
		for k := range reg {
			delete(reg, k)
		}
		reg["Roles"], reg["SessionUserSID"], reg["RunMode"], reg["AdminWorker"] = roles, admin, "session", ""
	}

	// who may ask
	reset("client", admin)
	elevated = false
	if err := run("client=1|worker=1|admin=|sid=" + admin); err == nil || !strings.Contains(err.Error(), "elevated") {
		t.Fatalf("not elevated: %v", err)
	}
	elevated, self = true, user
	if err := run("client=1|worker=1|admin=|sid=" + user); err == nil || !strings.Contains(err.Error(), "administrator") {
		t.Fatalf("elevated non-administrator: %v", err)
	}
	self = admin
	// injected data
	for _, bad := range []string{
		"client=1|worker=1|admin=|sid=" + admin + `|exe=C:\evil.exe`,
		`client=1|worker=1|admin=|sid=S-1-5-21-1 --data-dir C:\x`,
		"client=1|worker=1|admin=1|admin=|sid=" + admin,
	} {
		if err := run(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	// the admin worker comes with the worker; for an administrator only
	if err := run("client=1|worker=|admin=1|sid=" + admin); err == nil || !strings.Contains(err.Error(), "worker role") {
		t.Fatalf("admin without worker: %v", err)
	}
	reset("client", user)
	if err := run("client=1|worker=1|admin=1|sid=" + user); err == nil || !strings.Contains(err.Error(), "administrator") {
		t.Fatalf("admin worker handed to a non-administrator: %v", err)
	}
	if len(calls) != 0 || len(sched.calls) != 0 || reg["Roles"] != "client" {
		t.Fatalf("refusals changed something: %v %v %v", calls, sched.calls, reg)
	}
	// unprotected binaries: nothing registered
	env.ownDir = func() (string, error) { return "", errors.New("the installed binaries are not protected") }
	if err := run("client=1|worker=1|admin=|sid=" + user); err == nil || len(sched.calls) != 0 {
		t.Fatalf("unprotected binaries: %v %v", err, sched.calls)
	}
	env.ownDir = func() (string, error) { return `C:\Program Files\Longarm`, nil }

	// "Also run jobs here" + admin commands, for the administrator who asks
	reset("client", admin)
	if err := run("client=1|worker=1|admin=1|sid=" + admin); err != nil {
		t.Fatal(err)
	}
	task := longarmd.SessionTaskName(admin)
	if strings.Join(sched.calls, ",") != "register "+task+",end "+task+",run "+task {
		t.Errorf("tasks %v", sched.calls)
	}
	if len(calls) != 1 || calls[0].what != "admin-setup" || calls[0].arg != admin+" longarm.example.com "+filepath.Join(`C:\Program Files\Longarm`, "longarmd.exe") {
		t.Errorf("calls %v", calls)
	}
	if reg["Roles"] != "both" || reg["AdminWorker"] != "1" || reg["SessionUserSID"] != admin {
		t.Errorf("record %v", reg)
	}
	// and back: the worker leaves (task gone, the hub told), the admin worker goes
	calls, sched.calls = nil, nil
	if err := run("client=1|worker=|admin=|sid=" + admin); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0].what != "admin-remove" || calls[1].what != "leave" || calls[1].arg != `C:\Users\`+admin+`\.longarm` {
		t.Errorf("calls %v", calls)
	}
	if reg["Roles"] != "client" || reg["AdminWorker"] != "0" {
		t.Errorf("record %v", reg)
	}
	// another user's worker: not theirs to remove
	reset("both", admin)
	if err := run("client=1|worker=|admin=|sid=" + user); err == nil || !strings.Contains(err.Error(), "another user") {
		t.Fatalf("removing another user's worker: %v", err)
	}
	// the client only: just the record
	reset("both", admin)
	if err := run("client=|worker=1|admin=|sid=" + admin); err != nil || reg["Roles"] != "worker" || len(calls)+len(sched.calls) != 0 {
		t.Fatalf("drop the client: %v %v %v %v", err, reg, calls, sched.calls)
	}
}
