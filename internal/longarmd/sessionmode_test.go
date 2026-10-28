package longarmd

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/signin"
	"github.com/lubobill1990/longarm/internal/version"
)

const testSID = "S-1-12-1-111-222-333-444"

// TestSessionTaskXML: the user-session worker's task runs as the user with
// an interactive token (no password stored), at that user's logon and
// every 5 minutes (a dead host comes back; a live one makes it ignore the
// trigger), without a time limit, not stopped on battery, at normal
// priority, not elevated; arguments are quoted for Windows.
func TestSessionTaskXML(t *testing.T) {
	b, err := SessionTaskXML(TaskSpec{UserSID: testSID, AtLogon: true, Exe: `C:\Program Files\Longarm\longarm-tray.exe`,
		Args: []string{"--worker-host", "--data-dir", `C:\Users\a b\.longarm`}, WorkDir: `C:\Program Files\Longarm`, Description: "x & <y>"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		`<?xml version="1.0" encoding="UTF-16"?>`,
		`<LogonType>InteractiveToken</LogonType>`, `<RunLevel>LeastPrivilege</RunLevel>`,
		`<UserId>` + testSID + `</UserId>`, `<Interval>PT5M</Interval>`,
		`<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>`, `<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>`,
		`<DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>`, `<StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>`,
		`<Priority>5</Priority>`, `<RestartOnFailure>`, `<Command>C:\Program Files\Longarm\longarm-tray.exe</Command>`,
		`<Arguments>--worker-host --data-dir &#34;C:\Users\a b\.longarm&#34;</Arguments>`, `x &amp; &lt;y&gt;`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("task XML without %s:\n%s", want, s)
		}
	}
	if strings.Contains(s, "Password") {
		t.Error("the task stores a password")
	}
	// well-formed, and the logon trigger is the user's
	var doc taskXMLDoc
	if err := xml.Unmarshal(bytes.Replace(b, []byte(`encoding="UTF-16"`), []byte(`encoding="UTF-8"`), 1), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Triggers.Logon == nil || doc.Triggers.Logon.UserID != testSID {
		t.Fatalf("logon trigger: %+v", doc.Triggers.Logon)
	}
	// on demand (the tray's first start): no trigger, no restart
	b, _ = SessionTaskXML(TaskSpec{UserSID: testSID, Exe: `C:\x.exe`})
	if strings.Contains(string(b), "LogonTrigger") || strings.Contains(string(b), "RestartOnFailure") {
		t.Errorf("on-demand task:\n%s", b)
	}
	for _, bad := range []string{"S-1-5-18", "s-1-5-19", "alice", "", `S-1-5-21-1"`} {
		if _, err := SessionTaskXML(TaskSpec{UserSID: bad, Exe: `C:\x.exe`}); err == nil {
			t.Errorf("user %q accepted", bad)
		}
	}
	f := TaskXMLFile([]byte("<a>é</a>"))
	if !bytes.HasPrefix(f, []byte{0xff, 0xfe, '<', 0}) || len(f) != 2+2*8 {
		t.Errorf("UTF-16 file: % x", f)
	}
}

func TestEscapeWinArg(t *testing.T) {
	for in, want := range map[string]string{"a": "a", "": `""`, "a b": `"a b"`, `a"b`: `"a\"b"`, `C:\x y\`: `"C:\x y\\"`, `C:\x\y`: `C:\x\y`} {
		if got := escapeWinArg(in); got != want {
			t.Errorf("%q: %s, want %s", in, got, want)
		}
	}
}

func TestRestartDelay(t *testing.T) {
	if d := RestartDelay(ExitRestart, time.Second, time.Minute); d != 0 {
		t.Errorf("self-update: %s", d)
	}
	if d := RestartDelay(1, time.Second, 0); d != restartMin {
		t.Errorf("first crash: %s", d)
	}
	if d := RestartDelay(1, time.Second, 4*time.Second); d != 8*time.Second {
		t.Errorf("crash loop: %s", d)
	}
	if d := RestartDelay(1, time.Second, restartMax); d != restartMax {
		t.Errorf("capped: %s", d)
	}
	if d := RestartDelay(1, restartReset, restartMax); d != restartMin {
		t.Errorf("after a long run: %s", d)
	}
}

type fakeProc struct {
	pid  int
	code int
}

func (p fakeProc) PID() int           { return p.pid }
func (p fakeProc) Wait() (int, error) { return p.code, nil }

// TestSessionHost: longarmd is restarted when it exits - at once after a
// self-update's exit 75, with a growing delay after crashes - a longarmd
// already holding the data dir is watched, not doubled, and a second host
// for the same data dir refuses to run.
func TestSessionHost(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	codes := []int{1, 1, ExitRestart, 0}
	var mu sync.Mutex
	var starts int
	var sleeps []time.Duration
	running := []bool{false, false, false, true, false}
	now := time.Unix(1000, 0)
	h := &SessionHost{DataDir: dir,
		PrepareSignIn: func() error { return nil },
		Running: func() bool {
			mu.Lock()
			defer mu.Unlock()
			if len(running) == 0 {
				return false
			}
			r := running[0]
			running = running[1:]
			return r
		},
		Start: func(context.Context) (Proc, error) {
			mu.Lock()
			defer mu.Unlock()
			starts++
			if starts > len(codes) {
				cancel()
				return nil, errors.New("enough")
			}
			return fakeProc{pid: os.Getpid(), code: codes[starts-1]}, nil
		},
		Now: func() time.Time { now = now.Add(time.Second); return now },
		Sleep: func(ctx context.Context, d time.Duration) error {
			mu.Lock()
			sleeps = append(sleeps, d)
			mu.Unlock()
			return ctx.Err()
		},
	}
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("host did not stop")
	}
	// crash (2s), crash (4s), self-update (no sleep), adopted longarmd
	// (poll), exit 0 after a short run (8s)
	want := []time.Duration{restartMin, 2 * restartMin, adoptPoll, restartMin}
	if len(sleeps) < len(want) {
		t.Fatalf("sleeps %v, want %v", sleeps, want)
	}
	for i, w := range want {
		if sleeps[i] != w {
			t.Errorf("sleep %d: %s, want %s (all %v)", i, sleeps[i], w, sleeps)
		}
	}
	st, err := LoadSessionState(dir)
	if err != nil || st == nil || st.HostPID != os.Getpid() || st.Restarts < 4 {
		t.Fatalf("state: %+v %v", st, err)
	}

	// one host per data dir: a second one refuses to run
	lock, err := os.OpenFile(filepath.Join(dir, SessionLockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := platform.LockFile(lock); err != nil {
		t.Fatal(err)
	}
	h2 := &SessionHost{DataDir: dir, Start: func(context.Context) (Proc, error) { t.Fatal("second host started longarmd"); return nil, nil }}
	if err := h2.Run(context.Background()); !errors.Is(err, ErrHostRunning) {
		t.Errorf("second host: %v", err)
	}
}

type fakeSched struct {
	tasks map[string][]byte
	calls []string
}

func (f *fakeSched) Register(n string, d []byte) error {
	if f.tasks == nil {
		f.tasks = map[string][]byte{}
	}
	f.tasks[n] = d
	f.calls = append(f.calls, "register "+n)
	return nil
}
func (f *fakeSched) Delete(n string) error { delete(f.tasks, n); f.calls = append(f.calls, "delete "+n); return nil }
func (f *fakeSched) Run(n string) error    { f.calls = append(f.calls, "run "+n); return nil }
func (f *fakeSched) End(n string) error    { f.calls = append(f.calls, "end "+n); return nil }
func (f *fakeSched) Exists(n string) bool  { _, ok := f.tasks[n]; return ok }

// TestSessionTaskInstall: install registers the user's task and restarts
// it (end, then run); the tray's first start is an on-demand task of its
// own; removing deletes both.
func TestSessionTaskInstall(t *testing.T) {
	s := &fakeSched{}
	err := InstallSessionTask(SessionInstallOptions{UserSID: testSID, HostExe: `C:\L\longarm-tray.exe`, HostArgs: []string{"--worker-host"},
		Start: true, DataDir: t.TempDir(), Sched: s})
	if err != nil {
		t.Fatal(err)
	}
	name := SessionTaskName(testSID)
	if got := strings.Join(s.calls, ","); got != "register "+name+",end "+name+",run "+name {
		t.Errorf("calls: %s", got)
	}
	if !bytes.Contains(s.tasks[name], []byte("<Arguments>--worker-host</Arguments>")) {
		t.Errorf("task: %s", s.tasks[name])
	}
	if err := InstallSessionTask(SessionInstallOptions{UserSID: "S-1-5-18", HostExe: `C:\x.exe`, Sched: s}); err == nil {
		t.Error("LocalSystem accepted")
	}
	if err := LaunchInSession(s, testSID, `C:\L\longarm-tray.exe`, []string{"--signin"}); err != nil {
		t.Fatal(err)
	}
	if !s.Exists(TrayTaskName(testSID)) {
		t.Fatal("no tray task")
	}
	s.calls = nil
	if err := RemoveSessionTask(s, testSID, "", nil); err != nil {
		t.Fatal(err)
	}
	if len(s.tasks) != 0 {
		t.Errorf("left: %v (calls %v)", s.tasks, s.calls)
	}
}

// TestPrepareSessionSignIn: a fresh user-session data dir waits for a
// sign-in to the installer's / built-in hub; a join code's enrollment or
// an enrolled worker is left alone; without any hub nothing is written.
func TestPrepareSessionSignIn(t *testing.T) {
	oh, oc := version.DefaultHub, version.DefaultHubCA
	t.Cleanup(func() { version.DefaultHub, version.DefaultHubCA = oh, oc })
	version.DefaultHub, version.DefaultHubCA = "", ""
	dir := t.TempDir()
	if err := PrepareSessionSignIn(dir); err != nil {
		t.Fatal(err)
	}
	if hf, _ := hubclient.LoadFile(dir); hf.Address != "" && hf.Address != installedHubForTest() {
		t.Fatalf("no hub, but hub.toml %+v", hf)
	}
	version.DefaultHub = "longarm.example.com"
	dir = t.TempDir()
	if err := PrepareSessionSignIn(dir); err != nil {
		t.Fatal(err)
	}
	hf, _ := hubclient.LoadFile(dir)
	if !hf.SignIn || (hf.Address != "longarm.example.com" && hf.Address != installedHubForTest()) {
		t.Fatalf("hub.toml %+v", hf)
	}
	// a pending join: untouched
	dir = t.TempDir()
	hubclient.SaveFile(dir, hubclient.Config{Address: "other:7443", PollToken: "tok"})
	PrepareSessionSignIn(dir)
	if hf, _ := hubclient.LoadFile(dir); hf.SignIn || hf.Address != "other:7443" {
		t.Fatalf("join changed: %+v", hf)
	}
	// enrolled: untouched
	dir = t.TempDir()
	os.MkdirAll(filepath.Join(dir, TLSDir), 0o700)
	os.WriteFile(filepath.Join(dir, TLSDir, pki.NodeCertFile), []byte("x"), 0o600)
	PrepareSessionSignIn(dir)
	if _, err := os.Stat(filepath.Join(dir, hubclient.File)); err == nil {
		t.Fatal("enrolled data dir got a hub.toml")
	}
}

// installedHubForTest is the hub an MSI on this machine recorded (it wins
// over the built-in one), "none" when none.
func installedHubForTest() string {
	if h, _, from := signin.FallbackHub(); from == signin.HubFromInstaller {
		return h
	}
	return "none"
}
