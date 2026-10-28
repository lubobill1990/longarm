// Package msiupgrade stops Longarm's own processes before the MSI replaces
// their files, and starts again what ran, so that an upgrade never shows
// Windows Installer's "files in use" / Restart Manager dialog and never
// needs a reboot (packaging/windows/longarm.wxs, docs/usage.md "Upgrading").
//
// The pieces, in the order the MSI runs them:
//
//   - stop-tray (immediate, as the installing user, before InstallValidate,
//     where Windows Installer looks for files in use): the user's own tray
//     is asked to exit (its stop event, internal/stopsignal), else ended by
//     its pid. The tray is the only Longarm process with a window, the only
//     one the in-use check would list (Restart Manager is off:
//     MSIRESTARTMANAGERCONTROL=Disable); the installer starts it again
//     (LongarmTrayLaunch).
//   - stop (deferred, elevated, before RemoveExistingProducts): records and
//     stops the worker services (longarmd-admin, a RUN=service longarmd),
//     then every user's tray, user-session worker host and `longarmd run`
//     whose image is in the install dir - asked first, ended by pid after a
//     timeout - and moves binaries that are still in use (job shims and
//     console hosts are longarmd.exe processes, a `longarm` client in a
//     terminal is longarm.exe: running jobs and sessions must survive) aside
//     into <install dir>\old, which Windows allows for running images: the
//     new files go in without a reboot. What ran is recorded in
//     ProgramData\Longarm (Record).
//   - rollback: the moved files back, then the recorded services, workers
//     and trays started again.
//   - restart (`longarmd install-helper msi restart`, the new longarmd, at
//     the end): the recorded services, the recorded users' worker tasks and
//     trays (Restart); old binaries no longer in use are removed.
//
// Everything that touches processes goes through System, Services, Tasks
// and Files, so the logic is tested without Windows.
package msiupgrade

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Kind of a Longarm process the upgrade stops.
type Kind string

const (
	// KindTray: longarm-tray.exe with a UI (the icon, the popup).
	KindTray Kind = "tray"
	// KindHost: the user-session worker's host (longarm-tray.exe
	// --worker-host, longarmd.exe session-mode run).
	KindHost Kind = "host"
	// KindDaemon: longarmd.exe run (the host's child, or one started by
	// hand; a service's is stopped by the service manager first).
	KindDaemon Kind = "daemon"
)

// Proc is a running process.
type Proc struct {
	PID int
	// Start: its creation time (with PID: the same process, never a
	// reused pid).
	Start uint64
	// Exe: the full image path.
	Exe string
	// Args: the command line without the program (nil when unreadable).
	Args    []string
	SID     string
	Session uint32
}

// Classify tells which of the stopped kinds p is. Everything else from the
// install dir is left running: job shims and console hosts (`longarmd shim`,
// `longarmd conhost`), per-user helpers, `longarm` clients and the client
// daemon - their files are moved aside instead.
func Classify(p Proc) (Kind, bool) {
	_, base := splitWin(p.Exe)
	switch strings.ToLower(base) {
	case "longarm-tray.exe":
		for _, a := range p.Args {
			if a == "--worker-host" || a == "-worker-host" {
				return KindHost, true
			}
		}
		if p.Args == nil {
			return "", false // unknown: neither asked nor ended
		}
		return KindTray, true
	case "longarmd.exe":
		cmd := positional(p.Args)
		switch {
		case len(cmd) >= 1 && cmd[0] == "run":
			return KindDaemon, true
		case len(cmd) >= 2 && cmd[0] == "session-mode" && cmd[1] == "run":
			return KindHost, true
		}
	}
	return "", false
}

// positional: the arguments that are not flags (flags of longarmd's root
// command come before the subcommand only as --flag=value).
func positional(args []string) []string {
	var out []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		out = append(out, a)
	}
	return out
}

// InDir reports whether exe is a file directly in dir (case-insensitive,
// as Windows paths; a subdir such as dir\old is not dir).
func InDir(dir, exe string) bool {
	if dir == "" || exe == "" {
		return false
	}
	d := strings.TrimRight(strings.ReplaceAll(dir, "/", `\`), `\`)
	parent, base := splitWin(exe)
	return base != "" && !strings.Contains(exe, "..") && strings.EqualFold(parent, d)
}

// splitWin splits a Windows path into its dir and base name (on every
// platform: the tests run everywhere).
func splitWin(p string) (string, string) {
	p = strings.ReplaceAll(p, "/", `\`)
	i := strings.LastIndex(p, `\`)
	if i < 0 {
		return "", p
	}
	return p[:i], p[i+1:]
}

// Task names: the same as longarmd.SessionTaskName / TrayTaskName (a test
// there checks it).
func WorkerTaskName(sid string) string { return "LongarmWorker-" + sid }
func TrayTaskName(sid string) string   { return "LongarmTray-" + sid }

var sidRE = regexp.MustCompile(`^S-1-[0-9]+(-[0-9]+){1,14}$`)

// UserSID accepts a user's SID (not LocalSystem, LocalService,
// NetworkService).
func UserSID(sid string) bool {
	if !sidRE.MatchString(sid) {
		return false
	}
	switch sid {
	case "S-1-5-18", "S-1-5-19", "S-1-5-20":
		return false
	}
	return true
}

// DataMarker starts the data on the helper's command line, as on longarmd
// install-helper's (longarmd.MSIDataMarker).
const DataMarker = "#LONGARM-MSI#"

// Steps of longarm-msihelper and the fields each gets (longarm.wxs passes
// exactly these; a test checks it).
var Steps = map[string][]string{
	"stop-tray":   {"usersid"},
	"stop":        {"usersid"},
	"stop-remove": {"usersid"},
	"rollback":    {"usersid"},
}

var values = map[string]*regexp.Regexp{
	"usersid": regexp.MustCompile(`^(S-1-[0-9]+(-[0-9]+){1,14})?$`),
}

// ParseData returns the fields after DataMarker in raw (the command line):
// each of keys exactly once, nothing else, every value matching its pattern
// (Windows Installer formats properties into the command line unquoted:
// the same rules as longarmd.MSIData).
func ParseData(raw string, keys ...string) (map[string]string, error) {
	i := strings.Index(raw, DataMarker)
	if i < 0 {
		return nil, errors.New("no installer data on the command line")
	}
	data := strings.TrimRight(raw[i+len(DataMarker):], " \t\r\n")
	want := map[string]bool{}
	for _, k := range keys {
		if values[k] == nil {
			return nil, fmt.Errorf("internal: no pattern for %q", k)
		}
		want[k] = true
	}
	out := map[string]string{}
	for _, f := range strings.Split(data, "|") {
		if f == "" {
			continue
		}
		k, v, ok := strings.Cut(f, "=")
		if !ok || !want[k] {
			return nil, fmt.Errorf("unexpected installer data %q", f)
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("installer data %q given twice", k)
		}
		if !values[k].MatchString(v) {
			return nil, fmt.Errorf("installer data %q is invalid", k)
		}
		out[k] = v
	}
	for k := range want {
		if _, ok := out[k]; !ok {
			return nil, fmt.Errorf("installer data without %q", k)
		}
	}
	return out, nil
}
