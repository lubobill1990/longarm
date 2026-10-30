package updatesvc

import (
	"errors"
	"testing"
)

// TestCheckPipeServer: the pipe's server is the service when the service
// manager names its PID as the service's, LocalSystem, the installed
// binary; a squatter (another PID), a stopped service, another account or
// binary are refused - and nothing needs the server's process opened.
func TestCheckPipeServer(t *testing.T) {
	const exe = `C:\Program Files\Longarm\longarm-update.exe`
	svc := serviceProcess{Running: true, PID: 7400, Account: "LocalSystem", Command: `"` + exe + `" run`}
	if err := checkPipeServer(7400, svc, exe); err != nil {
		t.Fatalf("the service: %v", err)
	}
	for name, c := range map[string]struct {
		pid  uint32
		mod  func(*serviceProcess)
		want string
	}{
		"squatter":       {pid: 4242},
		"no pid":         {pid: 0},
		"stopped":        {pid: 7400, mod: func(s *serviceProcess) { s.Running, s.PID = false, 0 }},
		"user account":   {pid: 7400, mod: func(s *serviceProcess) { s.Account = `.\alice` }},
		"other binary":   {pid: 7400, mod: func(s *serviceProcess) { s.Command = `"C:\Users\alice\longarm-update.exe" run` }},
		"no install dir": {pid: 7400, want: "-"},
	} {
		s := svc
		if c.mod != nil {
			c.mod(&s)
		}
		want := exe
		if c.want == "-" {
			want = ""
		}
		if err := checkPipeServer(c.pid, s, want); !errors.Is(err, ErrImpostor) {
			t.Errorf("%s passed: %v", name, err)
		}
	}
}

func TestCommandExe(t *testing.T) {
	for in, want := range map[string]string{
		`"C:\Program Files\Longarm\longarm-update.exe" run`: `C:\Program Files\Longarm\longarm-update.exe`,
		`C:\Program Files\Longarm\longarm-update.exe run`:   `C:\Program Files\Longarm\longarm-update.exe`,
		`C:\Longarm\longarm-update.EXE`:                     `C:\Longarm\longarm-update.EXE`,
		`C:\x\svc run`:                                      `C:\x\svc`,
		`"unterminated`:                                     `unterminated`,
	} {
		if got := commandExe(in); got != want {
			t.Errorf("commandExe(%q) = %q, want %q", in, got, want)
		}
	}
	for _, a := range []string{"LocalSystem", "localsystem", `.\LocalSystem`, `NT AUTHORITY\SYSTEM`} {
		if !localSystemAccount(a) {
			t.Errorf("%q is LocalSystem", a)
		}
	}
	for _, a := range []string{"", `NT AUTHORITY\LocalService`, `.\alice`} {
		if localSystemAccount(a) {
			t.Errorf("%q is not LocalSystem", a)
		}
	}
}
