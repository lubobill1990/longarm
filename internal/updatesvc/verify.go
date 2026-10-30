package updatesvc

import (
	"fmt"
	"path/filepath"
	"strings"
)

// serviceProcess is what the service manager says about the
// LongarmUpdate service (Windows: win.QueryServiceProcess, readable by
// any interactive user - the service's process itself is not).
type serviceProcess struct {
	Running bool
	PID     uint32 // its current process (0 unless running)
	Account string // its logon account as configured
	Command string // its command line as configured
}

// checkPipeServer decides whether serverPID - the process that created
// the pipe instance a client is connected to (GetNamedPipeServerProcessId)
// - is the LongarmUpdate service: the service is running, serverPID is
// its current process, it runs as LocalSystem and its configured binary is
// wantExe (the installed longarm-update.exe). Only administrators can
// install or change a service, and the service manager names the process
// it started: a process squatting the pipe name has another PID. Nothing
// here opens the server's process (a LocalSystem process can't be opened
// by the user's tray, which made #66's check fail for every user).
func checkPipeServer(serverPID uint32, sp serviceProcess, wantExe string) error {
	if !sp.Running || sp.PID == 0 {
		return fmt.Errorf("%w: pid %d serves the pipe while the %s service is not running", ErrImpostor, serverPID, ServiceName)
	}
	if serverPID == 0 || serverPID != sp.PID {
		return fmt.Errorf("%w: pid %d serves the pipe, the %s service is pid %d", ErrImpostor, serverPID, ServiceName, sp.PID)
	}
	if !localSystemAccount(sp.Account) {
		return fmt.Errorf("%w: the %s service runs as %q, not LocalSystem", ErrImpostor, ServiceName, sp.Account)
	}
	exe := commandExe(sp.Command)
	if wantExe == "" || !strings.EqualFold(filepath.Clean(exe), filepath.Clean(wantExe)) {
		return fmt.Errorf("%w: the %s service runs %q, not %q", ErrImpostor, ServiceName, exe, wantExe)
	}
	return nil
}

// localSystemAccount: the ways a service's configuration names
// LocalSystem.
func localSystemAccount(a string) bool {
	for _, s := range []string{"LocalSystem", `.\LocalSystem`, `NT AUTHORITY\SYSTEM`} {
		if strings.EqualFold(strings.TrimSpace(a), s) {
			return true
		}
	}
	return false
}

// commandExe is the program of a service's command line: the quoted
// part, else up to ".exe" (an unquoted path with spaces), else up to the
// first space.
func commandExe(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if rest, ok := strings.CutPrefix(cmd, `"`); ok {
		exe, _, _ := strings.Cut(rest, `"`)
		return exe
	}
	if i := strings.Index(strings.ToLower(cmd), ".exe"); i >= 0 && (len(cmd) == i+4 || cmd[i+4] == ' ') {
		return cmd[:i+4]
	}
	exe, _, _ := strings.Cut(cmd, " ")
	return exe
}
