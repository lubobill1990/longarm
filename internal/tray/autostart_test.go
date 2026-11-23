package tray

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRunKeys is a registry: "hkcu|key|name" / "hklm|key|name" -> value.
type fakeRunKeys struct {
	strs map[string]string
	bins map[string][]byte
}

func newFakeRunKeys() *fakeRunKeys {
	return &fakeRunKeys{strs: map[string]string{}, bins: map[string][]byte{}}
}

func regPath(machine bool, key, name string) string {
	if machine {
		return "hklm|" + key + "|" + name
	}
	return "hkcu|" + key + "|" + name
}

func (f *fakeRunKeys) String(machine bool, key, name string) (string, bool, error) {
	v, ok := f.strs[regPath(machine, key, name)]
	return v, ok, nil
}

func (f *fakeRunKeys) Binary(machine bool, key, name string) ([]byte, bool, error) {
	v, ok := f.bins[regPath(machine, key, name)]
	return v, ok, nil
}

func (f *fakeRunKeys) SetString(key, name, value string) error {
	f.strs[regPath(false, key, name)] = value
	return nil
}

func (f *fakeRunKeys) Delete(key, name string) error {
	delete(f.strs, regPath(false, key, name))
	delete(f.bins, regPath(false, key, name))
	return nil
}

// TestRunKeyAutostart: the HKCU Run value is the switch; the MSI's HKLM
// value counts as on, is never doubled, and turning it off takes the
// administrator step (HKLM StartupApproved), canceled or not; Task
// Manager's "Disabled" reads as off.
func TestRunKeyAutostart(t *testing.T) {
	reg := newFakeRunKeys()
	elevated := 0
	var elevateErr error
	a := &runKeyAutostart{reg: reg, exe: `C:\Program Files\Longarm\longarm-tray.exe`, disableMachine: func() error {
		elevated++
		if elevateErr != nil {
			return elevateErr
		}
		reg.bins[regPath(true, approvedKeyPath, autostartValue)] = []byte{3, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
		return nil
	}}
	state := func() AutostartState {
		t.Helper()
		st, err := a.State()
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	if st := state(); st.Enabled || st.MachineWide {
		t.Fatalf("nothing: %+v", st)
	}
	if err := a.Set(true); err != nil {
		t.Fatal(err)
	}
	if v := reg.strs[regPath(false, runKeyPath, autostartValue)]; v != `"C:\Program Files\Longarm\longarm-tray.exe"` {
		t.Fatalf("HKCU value: %q", v)
	}
	if st := state(); !st.Enabled || st.MachineWide || !strings.HasPrefix(st.Where, `HKCU\`) {
		t.Fatalf("on: %+v", st)
	}
	// Task Manager > Startup apps > Disabled: off, and on again clears it
	reg.bins[regPath(false, approvedKeyPath, autostartValue)] = []byte{3, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	if st := state(); st.Enabled {
		t.Fatalf("disabled in Task Manager: %+v", st)
	}
	a.Set(true)
	if _, ok := reg.bins[regPath(false, approvedKeyPath, autostartValue)]; ok || !state().Enabled {
		t.Fatal("on again kept Task Manager's Disabled")
	}
	if err := a.Set(false); err != nil || state().Enabled || elevated != 0 {
		t.Fatalf("off: %v %+v elevated %d", err, state(), elevated)
	}

	// the MSI's value for every user: on, and on writes no second value
	reg.strs[regPath(true, runKeyPath, autostartValue)] = `"C:\Program Files\Longarm\longarm-tray.exe"`
	if st := state(); !st.Enabled || !st.MachineWide || !strings.HasPrefix(st.Where, `HKLM\`) {
		t.Fatalf("MSI: %+v", st)
	}
	a.Set(true)
	if _, ok := reg.strs[regPath(false, runKeyPath, autostartValue)]; ok {
		t.Fatal("a second Run value: the tray would start twice")
	}
	// off: the administrator prompt, declined
	elevateErr = ErrElevationCanceled
	if err := a.Set(false); !errors.Is(err, ErrElevationCanceled) || !state().Enabled || elevated != 1 {
		t.Fatalf("declined: %v %+v", err, state())
	}
	elevateErr = nil
	if err := a.Set(false); err != nil || state().Enabled || elevated != 2 {
		t.Fatalf("off for every user: %v %+v", err, state())
	}
	// on again: this user's own value (no administrator)
	if err := a.Set(true); err != nil || !state().Enabled || state().MachineWide || elevated != 2 {
		t.Fatalf("on after the machine's off: %v %+v", err, state())
	}
	// no way to elevate: says who can
	a.disableMachine = nil
	delete(reg.bins, regPath(true, approvedKeyPath, autostartValue))
	if err := a.Set(false); err == nil || !strings.Contains(err.Error(), "administrator") {
		t.Fatalf("no elevation: %v", err)
	}
}

// TestFileAutostart: the LaunchAgent and the .desktop file are the
// switch; a desktop's own "off" (Hidden, X-GNOME-Autostart-enabled) reads
// as off.
func TestFileAutostart(t *testing.T) {
	home := t.TempDir()
	exe := "/Applications/Long arm.app/Contents/MacOS/longarm-tray"
	la := launchAgent(home, exe)
	if st, err := la.State(); err != nil || st.Enabled {
		t.Fatalf("none: %+v %v", st, err)
	}
	if err := la.Set(true); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(home, "Library", "LaunchAgents", "com.longarm.tray.plist"))
	if !strings.Contains(string(b), "<string>"+exe+"</string>") || !strings.Contains(string(b), "<key>RunAtLoad</key>") {
		t.Fatalf("plist:\n%s", b)
	}
	if st, _ := la.State(); !st.Enabled || st.Where == "" {
		t.Fatalf("on: %+v", st)
	}
	if err := la.Set(false); err != nil {
		t.Fatal(err)
	}
	if st, _ := la.State(); st.Enabled {
		t.Fatalf("off: %+v", st)
	}
	if err := la.Set(false); err != nil {
		t.Fatalf("off twice: %v", err)
	}

	cfg := t.TempDir()
	xa := xdgAutostart(cfg, "/opt/long arm/longarm-tray")
	if err := xa.Set(true); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg, "autostart", "longarm-tray.desktop")
	b, _ = os.ReadFile(path)
	if !strings.Contains(string(b), `Exec="/opt/long arm/longarm-tray"`) || !strings.HasPrefix(string(b), "[Desktop Entry]\n") {
		t.Fatalf("desktop file:\n%s", b)
	}
	if st, _ := xa.State(); !st.Enabled {
		t.Fatalf("on: %+v", st)
	}
	os.WriteFile(path, append(b, []byte("Hidden=true\n")...), 0o644)
	if st, _ := xa.State(); st.Enabled {
		t.Fatalf("Hidden=true: %+v", st)
	}
	xa.Set(false)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("off left the file: %v", err)
	}
	for in, want := range map[string]string{
		"/usr/bin/longarm-tray": "/usr/bin/longarm-tray",
		`/a b/"x"$y`:            `"/a b/\\"x\\"\\$y"`,
		"/a%b":                  "/a%%b",
	} {
		if got := desktopQuote(in); got != want {
			t.Errorf("desktopQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

type fakeAutostart struct {
	on  bool
	err error
}

func (f *fakeAutostart) State() (AutostartState, error) { return AutostartState{Enabled: f.on}, nil }
func (f *fakeAutostart) Set(on bool) error {
	if f.err != nil {
		return f.err
	}
	f.on = on
	return nil
}

// TestAutostartSettings: /trayapi/settings reports the OS's state and
// changes it; other settings leave it alone.
func TestAutostartSettings(t *testing.T) {
	s, err := NewServer(Options{LongarmConfigDir: t.TempDir(), LongarmdDataDir: t.TempDir(), Roles: "client"}, &fakeDesk{}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fa := &fakeAutostart{on: true}
	s.autostart = fa
	call := func(h http.HandlerFunc, body string) (int, traySettings, string) {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest("POST", "/", strings.NewReader(body)))
		var out traySettings
		json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out, rec.Body.String()
	}
	if _, got, _ := call(s.getSettings, ""); got.Autostart == nil || !got.Autostart.Supported || !got.Autostart.Enabled {
		t.Fatalf("get: %+v", got.Autostart)
	}
	if code, got, _ := call(s.setSettings, `{"autostart": false}`); code != 200 || got.Autostart.Enabled || fa.on {
		t.Fatalf("off: %d %+v", code, got.Autostart)
	}
	if _, got, _ := call(s.setSettings, `{"mute_jobs": true}`); got.Autostart.Enabled || fa.on {
		t.Fatalf("another setting turned it on: %+v", got.Autostart)
	}
	fa.err = ErrElevationCanceled
	if code, _, body := call(s.setSettings, `{"autostart": true}`); code != http.StatusConflict || !strings.Contains(body, "administrator") {
		t.Fatalf("declined: %d %s", code, body)
	}
	s.autostart = nil
	if _, got, _ := call(s.getSettings, ""); got.Autostart == nil || got.Autostart.Supported {
		t.Fatalf("unsupported: %+v", got.Autostart)
	}
	if code, _, _ := call(s.setSettings, `{"autostart": true}`); code != http.StatusNotImplemented {
		t.Fatalf("unsupported set: %d", code)
	}
}
