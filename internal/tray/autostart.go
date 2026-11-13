package tray

import (
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// "Start Longarm at login" (Settings): the tray's own entry among the OS's
// login items - Windows: the Run key (HKCU, no administrator; the MSI's
// value in HKLM, for every user, counts too), macOS: a LaunchAgent in
// ~/Library/LaunchAgents, Linux: an XDG autostart .desktop file. The state
// is read from the OS every time, never from a saved preference: Task
// Manager's Startup apps or a deleted file change it too. Not the
// user-session worker's logon task (longarmd session-mode,
// LongarmWorker-<SID>): that one stays as the installer set it.

// AutostartState is Settings' "Start Longarm at login".
type AutostartState struct {
	// Supported: this OS has an implementation (else the switch is hidden).
	Supported bool `json:"supported"`
	Enabled   bool `json:"enabled"`
	// MachineWide: the installer's entry for every user of this computer
	// starts the tray (Windows: HKLM\...\Run, the MSI's); turning it off
	// asks for an administrator and turns it off for everybody.
	MachineWide bool `json:"machine_wide,omitempty"`
	// Where: the entry ("HKCU\...\Run longarm-tray", a file).
	Where string `json:"where,omitempty"`
	Error string `json:"error,omitempty"`
}

// autostarter reads and changes the login item (autostart_*.go pick one per
// OS; tests use fakes of the registry or the directory).
type autostarter interface {
	State() (AutostartState, error)
	Set(on bool) error
}

// autostartState is the state for /trayapi/settings (nil: unsupported).
func autostartState(a autostarter) *AutostartState {
	if a == nil {
		return &AutostartState{}
	}
	st, err := a.State()
	if err != nil {
		st.Error = err.Error()
	}
	st.Supported = true
	return &st
}

// ---------------------------------------------------------------- Windows: the Run key

const (
	autostartValue = "longarm-tray" // the MSI's and install.ps1's value name
	runKeyPath     = `Software\Microsoft\Windows\CurrentVersion\Run`
	// StartupApproved\Run: Task Manager's Startup apps switch per Run value
	// (a missing value or an even first byte: enabled; 03: disabled), under
	// HKCU for HKCU's Run values and under HKLM for HKLM's
	approvedKeyPath = `Software\Microsoft\Windows\CurrentVersion\Explorer\StartupApproved\Run`
)

// runKeys is the registry the Run-key autostart reads and writes: reads of
// HKCU or HKLM (machine), writes to HKCU only.
type runKeys interface {
	String(machine bool, key, name string) (string, bool, error)
	Binary(machine bool, key, name string) ([]byte, bool, error)
	SetString(key, name, value string) error
	// Delete removes an HKCU value (a missing one is no error).
	Delete(key, name string) error
}

// runKeyAutostart is the Windows login item: HKCU\...\Run\longarm-tray =
// "<exe>". The MSI's HKLM value starts the tray for every user; turning that
// off marks it disabled in HKLM's StartupApproved (as Task Manager does),
// which needs an administrator: disableMachine (the UAC prompt).
type runKeyAutostart struct {
	reg            runKeys
	exe            string
	disableMachine func() error
}

// approved: a Run value's StartupApproved entry lets it start.
func approved(b []byte, ok bool) bool { return !ok || len(b) == 0 || b[0]&1 == 0 }

func (a *runKeyAutostart) active(machine bool) (bool, error) {
	_, ok, err := a.reg.String(machine, runKeyPath, autostartValue)
	if err != nil || !ok {
		return false, err
	}
	b, bok, err := a.reg.Binary(machine, approvedKeyPath, autostartValue)
	if err != nil {
		return false, err
	}
	return approved(b, bok), nil
}

func (a *runKeyAutostart) State() (AutostartState, error) {
	user, err := a.active(false)
	if err != nil {
		return AutostartState{}, err
	}
	machine, err := a.active(true)
	if err != nil {
		return AutostartState{}, err
	}
	st := AutostartState{Enabled: user || machine, MachineWide: machine}
	switch {
	case user:
		st.Where = `HKCU\` + runKeyPath + ` ` + autostartValue
	case machine:
		st.Where = `HKLM\` + runKeyPath + ` ` + autostartValue
	}
	return st, nil
}

func (a *runKeyAutostart) Set(on bool) error {
	machine, err := a.active(true)
	if err != nil {
		return err
	}
	if on {
		if machine {
			return nil // the installer's value starts it already (a second one would start it twice)
		}
		if a.exe == "" {
			return errors.New("can't tell where longarm-tray.exe is")
		}
		if err := a.reg.SetString(runKeyPath, autostartValue, `"`+a.exe+`"`); err != nil {
			return err
		}
		// a Task Manager "Disabled" of an earlier value would keep it off
		return a.reg.Delete(approvedKeyPath, autostartValue)
	}
	if err := a.reg.Delete(runKeyPath, autostartValue); err != nil {
		return err
	}
	if !machine {
		return nil
	}
	if a.disableMachine == nil {
		return errors.New("Longarm was installed to start for every user of this computer: an administrator can turn that off (Task Manager > Startup apps)")
	}
	return a.disableMachine()
}

// machineDisableArgs: reg.exe marking the MSI's Run value disabled for
// every user, as Task Manager's Startup apps does (run elevated).
func machineDisableArgs() []string {
	return []string{"add", `HKLM\` + approvedKeyPath, "/v", autostartValue, "/t", "REG_BINARY", "/d", "030000000000000000000000", "/f", "/reg:64"}
}

// ---------------------------------------------------------------- macOS, Linux: a file

// fileAutostart is a login item that is a file: a LaunchAgent plist (macOS)
// or an XDG autostart .desktop file (Linux). It is on when the file is
// there and enabled says so.
type fileAutostart struct {
	path    string
	content func(exe string) []byte
	enabled func(b []byte) bool // nil: there = on
	exe     string
}

func (a *fileAutostart) State() (AutostartState, error) {
	b, err := os.ReadFile(a.path)
	if errors.Is(err, os.ErrNotExist) {
		return AutostartState{}, nil
	}
	if err != nil {
		return AutostartState{}, err
	}
	on := a.enabled == nil || a.enabled(b)
	return AutostartState{Enabled: on, Where: a.path}, nil
}

func (a *fileAutostart) Set(on bool) error {
	if !on {
		if err := os.Remove(a.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if a.exe == "" {
		return errors.New("can't tell where longarm-tray is")
	}
	if err := os.MkdirAll(filepath.Dir(a.path), 0o755); err != nil {
		return err
	}
	tmp := a.path + ".tmp"
	if err := os.WriteFile(tmp, a.content(a.exe), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.path)
}

// launchAgentLabel names the tray's LaunchAgent (its file: <label>.plist).
const launchAgentLabel = "com.longarm.tray"

// launchAgent is the macOS login item: ~/Library/LaunchAgents/com.longarm.tray.plist.
func launchAgent(home, exe string) *fileAutostart {
	return &fileAutostart{path: filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist"), exe: exe, content: launchAgentPlist}
}

func launchAgentPlist(exe string) []byte {
	var e strings.Builder
	xml.EscapeText(&e, []byte(exe))
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + launchAgentLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + e.String() + `</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>ProcessType</key>
	<string>Interactive</string>
	<key>LimitLoadToSessionType</key>
	<string>Aqua</string>
</dict>
</plist>
`)
}

// xdgAutostart is the Linux login item: $XDG_CONFIG_HOME/autostart/longarm-tray.desktop
// (default ~/.config).
func xdgAutostart(configHome, exe string) *fileAutostart {
	return &fileAutostart{path: filepath.Join(configHome, "autostart", "longarm-tray.desktop"), exe: exe, content: desktopEntry, enabled: desktopEnabled}
}

func desktopEntry(exe string) []byte {
	return []byte(fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=Longarm
Comment=Longarm tray: your workers, jobs and sessions
Exec=%s
Terminal=false
X-GNOME-Autostart-enabled=true
`, desktopQuote(exe)))
}

// desktopQuote quotes an Exec argument (the Desktop Entry spec: in double
// quotes, " ` $ \ escaped; % doubled).
func desktopQuote(s string) string {
	s = strings.ReplaceAll(s, "%", "%%")
	if !strings.ContainsAny(s, " \t\n\"'\\><~|&;$*?#()`") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\\\`, `"`, `\\"`, "`", "\\\\`", `$`, `\\$`)
	return `"` + r.Replace(s) + `"`
}

// desktopEnabled: a .desktop file that is there starts unless Hidden=true or
// X-GNOME-Autostart-enabled=false (how desktops switch one off).
func desktopEnabled(b []byte) bool {
	for _, l := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(l), "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.ToLower(strings.TrimSpace(v))
		if (k == "Hidden" && v == "true") || (k == "X-GNOME-Autostart-enabled" && v == "false") {
			return false
		}
	}
	return true
}
