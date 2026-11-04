package msiupgrade

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	dir   = `C:\Program Files\Longarm`
	alice = "S-1-5-21-1-2-3-1001"
	bob   = "S-1-5-21-1-2-3-1002"
)

func TestClassify(t *testing.T) {
	for _, c := range []struct {
		exe  string
		args []string
		want Kind
		ok   bool
	}{
		{`C:\Program Files\Longarm\longarm-tray.exe`, []string{}, KindTray, true},
		{`C:\Program Files\Longarm\longarm-tray.exe`, []string{"--signin"}, KindTray, true},
		{`C:\Program Files\Longarm\longarm-tray.exe`, []string{"--worker-host"}, KindHost, true},
		{`C:\Program Files\Longarm\longarm-tray.exe`, nil, "", false}, // unreadable command line
		{`C:\Program Files\Longarm\LONGARMD.EXE`, []string{"run", "--data-dir", `C:\x`}, KindDaemon, true},
		{`C:\Program Files\Longarm\longarmd.exe`, []string{"session-mode", "run"}, KindHost, true},
		// left running: jobs, sessions, clients, helpers
		{`C:\Program Files\Longarm\longarmd.exe`, []string{"shim", `C:\x\jobs\1`}, "", false},
		{`C:\Program Files\Longarm\longarmd.exe`, []string{"conhost", "--pty", "--", "cmd"}, "", false},
		{`C:\Program Files\Longarm\longarmd.exe`, []string{"login"}, "", false},
		{`C:\Program Files\Longarm\longarm.exe`, []string{"daemon", "run"}, "", false},
		{`C:\Program Files\Longarm\longarm.exe`, []string{"session", "attach", "x"}, "", false},
		{`C:\Program Files\Other\notepad.exe`, []string{"run"}, "", false},
	} {
		k, ok := Classify(Proc{Exe: c.exe, Args: c.args})
		if k != c.want || ok != c.ok {
			t.Errorf("Classify(%s %v) = %q %v, want %q %v", filepath.Base(c.exe), c.args, k, ok, c.want, c.ok)
		}
	}
}

func TestInDir(t *testing.T) {
	for _, c := range []struct {
		exe  string
		want bool
	}{
		{`C:\Program Files\Longarm\longarmd.exe`, true},
		{`c:\program files\longarm\LONGARMD.exe`, true},
		{`C:\Program Files\Longarm\old\longarmd-1.exe`, false}, // moved aside: left alone
		{`C:\Program Files\Longarm2\longarmd.exe`, false},
		{`C:\Users\x\Downloads\longarmd.exe`, false},
		{``, false},
	} {
		if got := InDir(dir+`\`, c.exe); got != c.want {
			t.Errorf("InDir(%s) = %v", c.exe, got)
		}
	}
}

func TestParseData(t *testing.T) {
	d, err := ParseData(`"C:\Windows\Installer\MSI1.tmp" stop #LONGARM-MSI#usersid=`+alice, "usersid")
	if err != nil || d["usersid"] != alice {
		t.Fatalf("%v %v", d, err)
	}
	for _, bad := range []string{
		`stop`,                                // no marker
		`stop #LONGARM-MSI#`,                  // missing key
		`stop #LONGARM-MSI#usersid=x" --evil`, // pattern
		`stop #LONGARM-MSI#usersid=` + alice + `|usersid=` + bob, // twice
		`stop #LONGARM-MSI#usersid=` + alice + `|dir=C:\evil`,    // unknown key
	} {
		if _, err := ParseData(bad, "usersid"); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if UserSID("S-1-5-18") || !UserSID(alice) || UserSID("S-1-5-21-1;x") {
		t.Error("UserSID")
	}
}

// fakeSystem: processes that exit when asked (graceful) or only when
// terminated; a host may start its longarmd again.
type fakeSystem struct {
	mu        sync.Mutex
	procs     []*fakeProc
	signaled  []int
	ended     []int
	nextPID   int
	respawned bool
}

type fakeProc struct {
	Proc
	graceful bool // has a stop event and honours it
	hangs    bool // has one but ignores it
	alive    bool
	respawn  bool // a host: when its daemon goes, it starts one more (once)
	undying  bool // Terminate fails
}

func (f *fakeSystem) add(p *fakeProc) *fakeProc {
	f.nextPID++
	p.PID, p.Start, p.alive = f.nextPID, uint64(1000+f.nextPID), true
	if p.Exe == "" {
		p.Exe = dir + `\longarmd.exe`
	}
	f.procs = append(f.procs, p)
	return p
}

func (f *fakeSystem) Processes() ([]Proc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Proc
	for _, p := range f.procs {
		if p.alive {
			out = append(out, p.Proc)
		}
	}
	return out, nil
}

func (f *fakeSystem) find(p Proc) *fakeProc {
	for _, x := range f.procs {
		if x.PID == p.PID && x.Start == p.Start {
			return x
		}
	}
	return nil
}

func (f *fakeSystem) gone(x *fakeProc) {
	x.alive = false
	if k, _ := Classify(x.Proc); k == KindDaemon && !f.respawned {
		for _, h := range f.procs {
			if h.alive && h.respawn && h.SID == x.SID {
				f.respawned = true
				f.add(&fakeProc{Proc: Proc{SID: x.SID, Args: []string{"run"}}, graceful: true})
			}
		}
	}
}

func (f *fakeSystem) Signal(p Proc) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	x := f.find(p)
	if x == nil || !(x.graceful || x.hangs) {
		return false
	}
	f.signaled = append(f.signaled, p.PID)
	if x.graceful {
		f.gone(x)
	}
	return true
}

func (f *fakeSystem) WaitExit(p Proc, d time.Duration) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	x := f.find(p)
	return x == nil || !x.alive
}

func (f *fakeSystem) Terminate(p Proc) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	x := f.find(p)
	if x == nil || !x.alive {
		return errors.New("gone")
	}
	if x.undying {
		return errors.New("access denied")
	}
	f.ended = append(f.ended, p.PID)
	f.gone(x)
	return nil
}

func TestStopProcesses(t *testing.T) {
	f := &fakeSystem{}
	tray := f.add(&fakeProc{Proc: Proc{Exe: dir + `\longarm-tray.exe`, Args: []string{}, SID: alice}, graceful: true})
	oldTray := f.add(&fakeProc{Proc: Proc{Exe: dir + `\longarm-tray.exe`, Args: []string{}, SID: bob}}) // v0.1.5: no event
	host := f.add(&fakeProc{Proc: Proc{Exe: dir + `\longarm-tray.exe`, Args: []string{"--worker-host"}, SID: alice}, graceful: true, respawn: true})
	daemon := f.add(&fakeProc{Proc: Proc{Args: []string{"run", "--data-dir", `C:\Users\alice\.longarm`}, SID: alice}, hangs: true})
	shim := f.add(&fakeProc{Proc: Proc{Args: []string{"shim", `C:\x`}, SID: alice}})
	client := f.add(&fakeProc{Proc: Proc{Exe: dir + `\longarm.exe`, Args: []string{"daemon", "run"}, SID: alice}})
	other := f.add(&fakeProc{Proc: Proc{Exe: `C:\Tools\longarmd.exe`, Args: []string{"run"}, SID: alice}}) // not ours
	var log bytes.Buffer
	st, err := StopProcesses(f, StopOptions{Dir: dir, Timeout: time.Millisecond, Log: &log})
	if err != nil {
		t.Fatal(err)
	}
	if tray.alive || oldTray.alive || host.alive || daemon.alive {
		t.Errorf("still running: tray %v old tray %v host %v daemon %v\n%s", tray.alive, oldTray.alive, host.alive, daemon.alive, log.String())
	}
	if !shim.alive || !client.alive || !other.alive {
		t.Error("stopped a job shim, a client or another program")
	}
	// graceful ones were asked, never ended; the old tray (no event) and
	// the daemon that ignored its event were ended by pid
	if !reflect.DeepEqual(f.ended, []int{oldTray.PID, daemon.PID}) {
		t.Errorf("ended %v, want old tray %d and daemon %d", f.ended, oldTray.PID, daemon.PID)
	}
	// the host was asked before its daemon
	if len(f.signaled) < 2 || f.signaled[0] != host.PID {
		t.Errorf("signaled %v: the host must go first", f.signaled)
	}
	if !reflect.DeepEqual(st.Trays, []string{alice, bob}) || !reflect.DeepEqual(st.Workers, []string{alice}) || len(st.Left) != 0 {
		t.Errorf("stopped %+v", st)
	}
}

func TestStopProcessesRespawnAndFilter(t *testing.T) {
	f := &fakeSystem{}
	// an old host (no event) whose daemon is ended first by mistake would
	// start another one: a later round catches it
	f.add(&fakeProc{Proc: Proc{Args: []string{"run"}, SID: alice}})
	f.add(&fakeProc{Proc: Proc{Args: []string{"session-mode", "run"}, SID: alice}, undying: true, respawn: true})
	bobTray := f.add(&fakeProc{Proc: Proc{Exe: dir + `\longarm-tray.exe`, Args: []string{}, SID: bob}})
	st, _ := StopProcesses(f, StopOptions{Dir: dir, SID: alice, Timeout: time.Millisecond})
	if !bobTray.alive {
		t.Error("stopped another user's tray")
	}
	if len(st.Left) != 1 || st.Left[0].Args[0] != "session-mode" {
		t.Errorf("left %+v, want the host that can't be ended", st.Left)
	}
	alive := 0
	for _, p := range f.procs {
		if p.alive && p.SID == alice {
			if k, _ := Classify(p.Proc); k == KindDaemon {
				alive++
			}
		}
	}
	if alive != 0 {
		t.Errorf("%d daemons left (a respawned one was not caught)", alive)
	}
	// kinds: the tray only
	f2 := &fakeSystem{}
	tr := f2.add(&fakeProc{Proc: Proc{Exe: dir + `\longarm-tray.exe`, Args: []string{}, SID: alice}, graceful: true})
	d := f2.add(&fakeProc{Proc: Proc{Args: []string{"run"}, SID: alice}, graceful: true})
	StopProcesses(f2, StopOptions{Dir: dir, SID: alice, Kinds: []Kind{KindTray}, Timeout: time.Millisecond})
	if tr.alive || !d.alive {
		t.Errorf("tray-only stop: tray alive %v, daemon alive %v", tr.alive, d.alive)
	}
}

type fakeServices struct {
	running map[string]bool
	calls   []string
}

func (s *fakeServices) Running(n string) (bool, error) { return s.running[n], nil }
func (s *fakeServices) Stop(n string) error {
	s.calls = append(s.calls, "stop "+n)
	s.running[n] = false
	return nil
}
func (s *fakeServices) Start(n string) error {
	s.calls = append(s.calls, "start "+n)
	s.running[n] = true
	return nil
}

type fakeTasks struct {
	exist map[string]bool
	ran   []string
}

func (t *fakeTasks) Exists(n string) bool { return t.exist[n] }
func (t *fakeTasks) Run(n string) error {
	t.ran = append(t.ran, n)
	return nil
}

func TestRestart(t *testing.T) {
	svc := &fakeServices{running: map[string]bool{"longarmd-admin": true, "longarmd": true}}
	stopped := StopServices(svc, []string{"longarmd-admin", "longarmd", "absent"}, nil)
	if !reflect.DeepEqual(stopped, []string{"longarmd-admin", "longarmd"}) {
		t.Fatalf("stopped %v", stopped)
	}
	svc.running["longarmd"] = true // the installer's ServiceControl started it already
	tasks := &fakeTasks{exist: map[string]bool{WorkerTaskName(alice): true, TrayTaskName(bob): true}}
	var launched []string
	rec := &Record{Services: stopped, Workers: []string{alice, bob}, Trays: []string{alice, bob}}
	err := Restart(rec, RestartOptions{Services: svc, Tasks: tasks, SkipTray: alice,
		LaunchTray: func(sid string) error { launched = append(launched, sid); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(svc.calls, []string{"stop longarmd-admin", "stop longarmd", "start longarmd-admin"}) {
		t.Errorf("service calls %v", svc.calls)
	}
	// bob has no worker task (any more): not started; alice's tray the installer starts
	if !reflect.DeepEqual(tasks.ran, []string{WorkerTaskName(alice)}) || !reflect.DeepEqual(launched, []string{bob}) {
		t.Errorf("ran %v, launched %v", tasks.ran, launched)
	}
	// a non-administrator: their own only, no services; without LaunchTray
	// the existing tray task runs
	svc2 := &fakeServices{running: map[string]bool{}}
	tasks2 := &fakeTasks{exist: map[string]bool{WorkerTaskName(bob): true, TrayTaskName(bob): true, WorkerTaskName(alice): true}}
	Restart(rec, RestartOptions{Services: svc2, Tasks: tasks2, OnlySID: bob})
	if len(svc2.calls) != 0 || !reflect.DeepEqual(tasks2.ran, []string{WorkerTaskName(bob), TrayTaskName(bob)}) {
		t.Errorf("non-admin restart: services %v, tasks %v", svc2.calls, tasks2.ran)
	}
}

func TestRecord(t *testing.T) {
	d := t.TempDir()
	now := time.Now()
	if r, err := LoadRecord(d, now); r != nil || err != nil {
		t.Fatalf("empty: %v %v", r, err)
	}
	r := &Record{Created: now.UTC(), Services: []string{"longarmd-admin"}, Workers: []string{alice}, Trays: []string{bob},
		Aside: []Aside{{From: `C:\a\longarmd.exe`, To: `C:\a\old\longarmd-1.exe`}}}
	if err := r.Save(d); err != nil {
		t.Fatal(err)
	}
	got, err := LoadRecord(d, now)
	if err != nil || got == nil || !reflect.DeepEqual(got.Workers, r.Workers) || !reflect.DeepEqual(got.Aside, r.Aside) {
		t.Fatalf("load: %+v %v", got, err)
	}
	if got, _ := LoadRecord(d, now.Add(3*time.Hour)); got != nil {
		t.Error("a stale record was used")
	}
	if _, err := os.Stat(filepath.Join(d, RecordFile)); err == nil {
		t.Error("a stale record was kept")
	}
	for _, bad := range []*Record{
		{Created: now, Workers: []string{"S-1-5-18"}},
		{Created: now, Trays: []string{"x & calc"}},
		{Created: now, Services: []string{`a" b`}},
	} {
		bad.Save(d)
		if _, err := LoadRecord(d, now); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestMoveAside(t *testing.T) {
	d := t.TempDir()
	for _, n := range Binaries {
		os.WriteFile(filepath.Join(d, n), []byte("old "+n), 0o644)
	}
	inUse := map[string]bool{filepath.Join(d, "longarmd.exe"): true, filepath.Join(d, "longarm.exe"): true}
	f := Files{InUse: func(p string) bool { return inUse[p] }}
	var log bytes.Buffer
	as, err := f.MoveAside(d, time.Unix(0, 42), &log)
	if err != nil || len(as) != 2 {
		t.Fatalf("%v %v", as, err)
	}
	for _, a := range as {
		if _, err := os.Stat(a.From); err == nil {
			t.Errorf("%s still there", a.From)
		}
		if filepath.Dir(a.To) != filepath.Join(d, AsideDir) || !strings.HasSuffix(a.To, "-42.exe") {
			t.Errorf("moved to %s", a.To)
		}
	}
	if _, err := os.Stat(filepath.Join(d, "longarm-tray.exe")); err != nil {
		t.Error("a binary not in use was moved")
	}
	// rollback: back where nothing replaced them; a path outside the dir is refused
	os.WriteFile(filepath.Join(d, "longarm.exe"), []byte("restored by the installer"), 0o644)
	bad := append(as, Aside{From: filepath.Join(d, "..", "evil.exe"), To: as[0].To})
	if err := RestoreAside(d, bad, &log); err == nil {
		t.Error("RestoreAside accepted a foreign path")
	}
	if b, _ := os.ReadFile(filepath.Join(d, "longarmd.exe")); string(b) != "old longarmd.exe" {
		t.Errorf("longarmd.exe not restored: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(d, "longarm.exe")); string(b) != "restored by the installer" {
		t.Error("RestoreAside replaced a file that was there")
	}
	// clean: what nothing holds goes, then the dir
	CleanAside(d, &log)
	if _, err := os.Stat(filepath.Join(d, AsideDir)); err == nil {
		t.Errorf("aside dir left: %s", log.String())
	}
}

// TestRemoveAside: an uninstall removes what nothing holds from dir\old and
// has the rest (a client that still runs) deleted at the next start, then
// the dir.
func TestRemoveAside(t *testing.T) {
	d := t.TempDir()
	od := filepath.Join(d, AsideDir)
	os.MkdirAll(od, 0o755)
	held, free := filepath.Join(od, "longarm-1.exe"), filepath.Join(od, "longarmd-2.exe")
	os.WriteFile(held, []byte("x"), 0o644)
	os.WriteFile(free, []byte("x"), 0o644)
	// a file in use can't be deleted: on Windows keep it open (no FILE_SHARE_DELETE); elsewhere a
	// read-only dir stands in once the free file is gone
	if runtime.GOOS == "windows" {
		h, err := os.Open(held)
		if err != nil {
			t.Fatal(err)
		}
		defer h.Close()
	} else {
		if os.Geteuid() == 0 {
			t.Skip("root deletes from read-only dirs")
		}
		os.Remove(free)
		os.Chmod(od, 0o555)
		defer os.Chmod(od, 0o755)
	}
	var atReboot []string
	f := Files{DeleteAtReboot: func(p string) error { atReboot = append(atReboot, p); return nil }}
	var log bytes.Buffer
	f.RemoveAside(d, &log)
	if _, err := os.Stat(free); err == nil {
		t.Error("a binary nothing holds was left")
	}
	if !reflect.DeepEqual(atReboot, []string{held, od}) {
		t.Errorf("deleted at the next start: %v (want %s, then the dir)\n%s", atReboot, held, log.String())
	}
}

func TestRunStep(t *testing.T) {
	state := t.TempDir()
	newEnv := func(me string, admin bool) (Env, *fakeSystem, *fakeServices, *fakeTasks) {
		f := &fakeSystem{}
		svc := &fakeServices{running: map[string]bool{"longarmd-admin": true}}
		tasks := &fakeTasks{exist: map[string]bool{WorkerTaskName(alice): true, TrayTaskName(alice): true, TrayTaskName(bob): true}}
		return Env{
			InstallDir:   func() (string, error) { return dir, nil },
			CurrentSID:   func() (string, error) { return me, nil },
			IsAdmin:      func(sid string) (bool, error) { return admin && sid == me, nil },
			ServiceNames: func() []string { return []string{"longarmd-admin", "longarmd"} },
			StateDir:     func() (string, error) { return state, nil },
			System:       f, Services: svc, Tasks: tasks,
			Files:   Files{InUse: func(string) bool { return false }},
			Timeout: time.Millisecond,
		}, f, svc, tasks
	}
	data := func(sid string) string { return `"x.tmp" step ` + DataMarker + "usersid=" + sid }

	// stop-early without an administrator's token: the installing user's own trays, nothing else
	env, f, _, _ := newEnv(alice, false)
	at := f.add(&fakeProc{Proc: Proc{Exe: dir + `\longarm-tray.exe`, Args: []string{}, SID: alice}})
	bt := f.add(&fakeProc{Proc: Proc{Exe: dir + `\longarm-tray.exe`, Args: []string{}, SID: bob}})
	ah := f.add(&fakeProc{Proc: Proc{Exe: dir + `\longarm-tray.exe`, Args: []string{"--worker-host"}, SID: alice}})
	if err := RunStep("stop-early", data(alice), env); err != nil {
		t.Fatal(err)
	}
	if at.alive || !bt.alive || !ah.alive {
		t.Errorf("stop-early: alice's tray %v, bob's %v, alice's host %v", at.alive, bt.alive, ah.alive)
	}
	if r, _ := LoadRecord(state, time.Now()); r != nil {
		t.Errorf("stop-early unelevated recorded %+v", r)
	}
	// ... uninstalling: their worker too
	RunStep("stop-early-remove", data(alice), env)
	if ah.alive || !bt.alive {
		t.Errorf("stop-early-remove: alice's host %v, bob's tray %v", ah.alive, bt.alive)
	}
	// ... and nothing when it runs neither as the user it was asked for nor elevated
	env, f, _, _ = newEnv("S-1-5-21-1-2-3-1003", false)
	other := f.add(&fakeProc{Proc: Proc{Exe: dir + `\longarm-tray.exe`, Args: []string{}, SID: alice}})
	RunStep("stop-early", data(alice), env)
	if !other.alive {
		t.Error("stop-early as another user stopped a tray")
	}

	// stop-early elevated (the update service's install as SYSTEM): what stop does, before
	// InstallValidate; the deferred stop then adds to its record, and the rollback starts it all
	env, f, svc, tasks := newEnv("S-1-5-18", true)
	env.Elevated = func() bool { return true }
	inUse := map[string]bool{}
	env.Files.InUse = func(p string) bool { return inUse[p] }
	inst := t.TempDir()
	env.InstallDir = func() (string, error) { return inst, nil }
	for _, n := range Binaries {
		os.WriteFile(filepath.Join(inst, n), []byte(n), 0o644)
	}
	inUse[filepath.Join(inst, "longarm.exe")] = true // a `longarm` client in a terminal
	f.add(&fakeProc{Proc: Proc{Exe: inst + `\longarm-tray.exe`, Args: []string{}, SID: bob}})
	eh := f.add(&fakeProc{Proc: Proc{Exe: inst + `\longarm-tray.exe`, Args: []string{"--worker-host"}, SID: alice}})
	if err := RunStep("stop-early", data("S-1-5-18"), env); err != nil {
		t.Fatal(err)
	}
	rec, err := LoadRecord(state, time.Now())
	if err != nil || rec == nil || !rec.Early || eh.alive || svc.running["longarmd-admin"] || len(rec.Aside) != 1 {
		t.Fatalf("stop-early elevated: record %+v %v, host alive %v, services %v", rec, err, eh.alive, svc.running)
	}
	delete(inUse, filepath.Join(inst, "longarm.exe"))
	inUse[filepath.Join(inst, "longarmd.exe")] = true // a job's shim, started meanwhile
	f.add(&fakeProc{Proc: Proc{Exe: inst + `\longarm-tray.exe`, Args: []string{}, SID: alice}})
	if err := RunStep("stop", data("S-1-5-18"), env); err != nil {
		t.Fatal(err)
	}
	rec, _ = LoadRecord(state, time.Now())
	if rec == nil || !reflect.DeepEqual(rec.Services, []string{"longarmd-admin"}) || !reflect.DeepEqual(rec.Workers, []string{alice}) ||
		!reflect.DeepEqual(rec.Trays, []string{bob, alice}) || len(rec.Aside) != 2 {
		t.Fatalf("the deferred stop did not add to stop-early's record: %+v", rec)
	}
	if _, err := os.Stat(rec.Aside[0].To); err != nil {
		t.Errorf("the deferred stop cleaned what stop-early moved aside: %v", err)
	}
	if err := RunStep("rollback", data("S-1-5-18"), env); err != nil {
		t.Fatal(err)
	}
	if !svc.running["longarmd-admin"] || !reflect.DeepEqual(tasks.ran, []string{WorkerTaskName(alice), TrayTaskName(bob), TrayTaskName(alice)}) {
		t.Errorf("rollback after stop-early: services %v, tasks %v", svc.calls, tasks.ran)
	}
	for _, n := range []string{"longarm.exe", "longarmd.exe"} {
		if _, err := os.Stat(filepath.Join(inst, n)); err != nil {
			t.Errorf("%s not moved back: %v", n, err)
		}
	}

	// an uninstall elevated: stop-early-remove moves the client's binary aside; stop-remove has
	// what still runs from there deleted at the next start, and drops the record
	env, f, _, _ = newEnv("S-1-5-18", true)
	env.Elevated = func() bool { return true }
	env.InstallDir = func() (string, error) { return inst, nil }
	inUse = map[string]bool{filepath.Join(inst, "longarm.exe"): true}
	var atReboot []string
	env.Files = Files{InUse: func(p string) bool { return inUse[p] }, DeleteAtReboot: func(p string) error { atReboot = append(atReboot, p); return nil }}
	if err := RunStep("stop-early-remove", data("S-1-5-18"), env); err != nil {
		t.Fatal(err)
	}
	r, _ := LoadRecord(state, time.Now())
	if r == nil || len(r.Aside) != 1 {
		t.Fatalf("stop-early-remove: record %+v", r)
	}
	if err := RunStep("stop-remove", data("S-1-5-18"), env); err != nil {
		t.Fatal(err)
	}
	if r, _ := LoadRecord(state, time.Now()); r != nil {
		t.Error("stop-remove kept the record")
	}
	// (nothing holds the fake client's file: removed at once; TestRemoveAside: one still in use)
	if _, err := os.Stat(r.Aside[0].To); err == nil && len(atReboot) == 0 {
		t.Errorf("%s left without being deleted at the next start", r.Aside[0].To)
	}

	// stop (an administrator's upgrade): services, every user, a record
	env, f, svc, tasks = newEnv(alice, true)
	f.add(&fakeProc{Proc: Proc{Exe: dir + `\longarm-tray.exe`, Args: []string{}, SID: bob}})
	f.add(&fakeProc{Proc: Proc{Exe: dir + `\longarm-tray.exe`, Args: []string{"--worker-host"}, SID: alice}})
	if err := RunStep("stop", data(alice), env); err != nil {
		t.Fatal(err)
	}
	rec, err = LoadRecord(state, time.Now())
	if err != nil || rec == nil {
		t.Fatalf("record: %v %v", rec, err)
	}
	if !reflect.DeepEqual(rec.Services, []string{"longarmd-admin"}) || !reflect.DeepEqual(rec.Workers, []string{alice}) || !reflect.DeepEqual(rec.Trays, []string{bob}) {
		t.Errorf("record %+v", rec)
	}
	// rollback: started again, record gone
	if err := RunStep("rollback", data(alice), env); err != nil {
		t.Fatal(err)
	}
	if !svc.running["longarmd-admin"] || !reflect.DeepEqual(tasks.ran, []string{WorkerTaskName(alice), TrayTaskName(bob)}) {
		t.Errorf("rollback: services %v, tasks %v", svc.calls, tasks.ran)
	}
	if r, _ := LoadRecord(state, time.Now()); r != nil {
		t.Error("record kept after the rollback")
	}

	// stop by a non-administrator: their own processes only, no services
	env, f, svc, _ = newEnv(bob, false)
	a := f.add(&fakeProc{Proc: Proc{Exe: dir + `\longarm-tray.exe`, Args: []string{}, SID: alice}})
	b := f.add(&fakeProc{Proc: Proc{Exe: dir + `\longarm-tray.exe`, Args: []string{}, SID: bob}})
	RunStep("stop", data(bob), env)
	if !a.alive || b.alive || len(svc.calls) != 0 {
		t.Errorf("non-admin stop: alice's tray %v, bob's %v, services %v", a.alive, b.alive, svc.calls)
	}
	if err := RunStep("stop", `x stop #LONGARM-MSI#usersid=`+bob+`|usersid=`+alice, env); err == nil {
		t.Error("RunStep accepted bad data")
	}
}
