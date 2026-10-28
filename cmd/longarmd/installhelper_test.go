package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lubobill1990/longarm/internal/longarmd"
)

type nopSched struct{ calls []string }

func (s *nopSched) Register(n string, _ []byte) error {
	s.calls = append(s.calls, "register "+n)
	return nil
}
func (s *nopSched) Delete(n string) error { s.calls = append(s.calls, "delete "+n); return nil }
func (s *nopSched) Run(n string) error    { s.calls = append(s.calls, "run "+n); return nil }
func (s *nopSched) End(n string) error    { s.calls = append(s.calls, "end "+n); return nil }
func (s *nopSched) Exists(string) bool    { return false }

// TestMSIStepsPolicy: the elevated steps refuse what a non-administrator
// may not ask for - a repair or change of the installed product, a worker
// service, the admin worker, a task for another user - and never act on an
// injected value. (The registry, services and tasks are not touched: the
// refusals come first.)
func TestMSIStepsPolicy(t *testing.T) {
	const user, admin, other = "S-1-5-21-1-2-3-1001", "S-1-5-21-1-2-3-500", "S-1-5-21-1-2-3-1002"
	sched := &nopSched{}
	var ownDirCalls int
	env := msiEnv{
		isAdmin: func(sid string) (bool, error) { return sid == admin, nil },
		userSID: func(string) error { return nil },
		ownDir: func() (string, error) {
			ownDirCalls++
			return "", errors.New("test: no installed binaries")
		},
		programData: func() string { return `C:\ProgramData\Longarm` },
		sched:       sched,
	}
	run := func(step, data string) error {
		var out bytes.Buffer
		env.out = &out
		return runMSIStep(context.Background(), step, `"C:\Program Files\Longarm\longarmd.exe" install-helper msi `+step+" "+longarmd.MSIDataMarker+data, env)
	}
	// a non-administrator's repair (the HUB / SESSION_SID / --exe injection path)
	if err := run("check", "installed=2026/10/28 12:00:00|usersid="+user); err == nil || !strings.Contains(err.Error(), "administrator") {
		t.Fatalf("non-admin repair: %v", err)
	}
	if err := run("check", "installed=2026/10/28 12:00:00|usersid="+admin); err != nil {
		t.Fatalf("admin repair: %v", err)
	}
	if err := run("check", "installed=|usersid="+user); err != nil {
		t.Fatalf("first install: %v", err)
	}
	// the admin worker / a service: administrators only
	if err := run("admin-setup", "hub=longarm.example.com|ca=|sid="+user+"|usersid="+user); err == nil || !strings.Contains(err.Error(), "administrator") {
		t.Fatalf("non-admin admin worker: %v", err)
	}
	if err := run("admin-setup", "hub=longarm.example.com|ca=|sid="+other+"|usersid="+admin); err == nil || !strings.Contains(err.Error(), "administrator") {
		t.Fatalf("admin worker handed to a non-admin: %v", err)
	}
	if err := run("setup", `datadir=C:\ProgramData\Longarm\longarmd|user=LocalSystem|hub=h.example.com|code=|ca=|listen=|name=|isolation=|login=1|usersid=`+user); err == nil ||
		!strings.Contains(err.Error(), "administrator") {
		t.Fatalf("non-admin service: %v", err)
	}
	// injected values: refused before anything happens
	if err := run("admin-setup", `hub=x" --exe "C:\Users\attacker\evil.exe" --hub "x|ca=|sid=`+admin+"|usersid="+admin); err == nil {
		t.Fatal("injected HUB accepted")
	}
	if err := run("setup", `datadir=C:\Users\attacker\planted|user=LocalSystem|hub=|code=|ca=|listen=|name=|isolation=|login=|usersid=`+admin); err == nil ||
		!strings.Contains(err.Error(), "below") {
		t.Fatalf("data dir outside ProgramData Longarm: %v", err)
	}
	// a task for another user: administrators only; for oneself fine (then
	// the binaries' directory must be protected: refused here)
	if err := run("session", "sid="+other+"|usersid="+user); err == nil || !strings.Contains(err.Error(), "administrator") {
		t.Fatalf("task for another user: %v", err)
	}
	if err := run("launch-tray", "sid="+other+"|usersid="+user); err == nil || !strings.Contains(err.Error(), "administrator") {
		t.Fatalf("tray for another user: %v", err)
	}
	ownDirCalls = 0
	if err := run("session", "sid="+user+"|usersid="+user); err == nil || ownDirCalls != 1 {
		t.Fatalf("own task with unprotected binaries: %v (ownDir %d)", err, ownDirCalls)
	}
	if len(sched.calls) != 0 {
		t.Fatalf("tasks touched: %v", sched.calls)
	}
}
