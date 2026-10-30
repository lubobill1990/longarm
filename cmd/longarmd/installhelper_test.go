package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/longarmd"
	"github.com/lubobill1990/longarm/internal/msiupgrade"
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

type fakeSvc struct{ running map[string]bool }

func (s *fakeSvc) Running(n string) (bool, error) { return s.running[n], nil }
func (s *fakeSvc) Stop(n string) error            { s.running[n] = false; return nil }
func (s *fakeSvc) Start(n string) error           { s.running[n] = true; return nil }

type existSched struct {
	nopSched
	exist map[string]bool
}

func (s *existSched) Exists(n string) bool { return s.exist[n] }

// TestMSIRestart: after an upgrade the restart step starts what the upgrade
// helper recorded - services, worker tasks, the trays (not the one
// LongarmTrayLaunch starts) - removes the record and the old binaries
// nothing holds; a non-administrator gets only their own worker and tray.
func TestMSIRestart(t *testing.T) {
	const user, admin, other = "S-1-5-21-1-2-3-1001", "S-1-5-21-1-2-3-500", "S-1-5-21-1-2-3-1002"
	if longarmd.SessionTaskName(user) != msiupgrade.WorkerTaskName(user) || longarmd.TrayTaskName(user) != msiupgrade.TrayTaskName(user) {
		t.Fatal("msiupgrade's task names differ from longarmd's")
	}
	for _, c := range []struct {
		asker     string
		wantRuns  []string
		wantStart bool
	}{
		{admin, []string{"run " + longarmd.SessionTaskName(user), "run " + longarmd.SessionTaskName(other),
			"register " + longarmd.TrayTaskName(other), "run " + longarmd.TrayTaskName(other)}, true},
		{other, []string{"run " + longarmd.SessionTaskName(other), "register " + longarmd.TrayTaskName(other), "run " + longarmd.TrayTaskName(other)}, false},
	} {
		pd, own := t.TempDir(), t.TempDir()
		os.WriteFile(filepath.Join(own, "longarm-tray.exe"), nil, 0o644)
		os.MkdirAll(filepath.Join(own, msiupgrade.AsideDir), 0o755)
		os.WriteFile(filepath.Join(own, msiupgrade.AsideDir, "longarmd-1.exe"), nil, 0o644)
		rec := &msiupgrade.Record{Created: time.Now().UTC(), Services: []string{"longarmd-admin"},
			Workers: []string{user, other}, Trays: []string{user, other}}
		if err := rec.Save(pd); err != nil {
			t.Fatal(err)
		}
		svc := &fakeSvc{running: map[string]bool{}}
		sched := &existSched{exist: map[string]bool{longarmd.SessionTaskName(user): true, longarmd.SessionTaskName(other): true}}
		env := msiEnv{
			isAdmin:     func(sid string) (bool, error) { return sid == admin, nil },
			userSID:     func(string) error { return nil },
			ownDir:      func() (string, error) { return own, nil },
			programData: func() string { return pd },
			sched:       sched, services: svc, out: io.Discard,
		}
		err := runMSIStep(context.Background(), "restart", `x install-helper msi restart `+longarmd.MSIDataMarker+"sid="+user+"|launch=1|usersid="+c.asker, env)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(sched.calls, ",") != strings.Join(c.wantRuns, ",") {
			t.Errorf("%s: tasks %v, want %v", c.asker, sched.calls, c.wantRuns)
		}
		if svc.running["longarmd-admin"] != c.wantStart {
			t.Errorf("%s: service started %v", c.asker, svc.running["longarmd-admin"])
		}
		if _, err := os.Stat(filepath.Join(pd, msiupgrade.RecordFile)); err == nil {
			t.Error("record kept")
		}
		if _, err := os.Stat(filepath.Join(own, msiupgrade.AsideDir)); err == nil {
			t.Error("old binaries kept")
		}
	}
}
