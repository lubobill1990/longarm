package shim

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/lubobill1990/longarm/internal/platform"
)

// ConsoleHost implements `longarmd conhost [--pty] [--raw] -- argv...` (Windows).
//
// Default: it gives the command a private hidden console with UTF-8 code
// pages - detached from any console longarmd itself may have inherited from a
// terminal - and passes stdio and the exit code through. longarmd's Exec wraps
// commands with it so short commands encode output exactly like jobs do.
// The caller's Job Object still applies: the helper and its children are
// created inside it.
//
// --pty: the helper already runs on a pseudo console (session, ssh pty); it
// only switches that console to UTF-8 and ignores console control events, so
// a Ctrl+C typed into the terminal interrupts the foreground program but
// never ends the session's main process.
//
// --raw: argv is exactly [program, commandline]; the command line is passed
// verbatim (ssh commands for cmd.exe /c must not be re-quoted).
func ConsoleHost(argv []string) int {
	var pty, raw bool
	for len(argv) > 0 {
		switch argv[0] {
		case "--pty":
			pty = true
		case "--raw":
			raw = true
		case "--":
			argv = argv[1:]
			goto run
		default:
			goto run
		}
		argv = argv[1:]
	}
run:
	if len(argv) == 0 || (raw && len(argv) != 2) {
		fmt.Fprintln(os.Stderr, "longarmd conhost: missing command")
		return 2
	}
	if pty {
		platform.UTF8Console()
		signal.Ignore(os.Interrupt, syscall.SIGTERM)
	} else if err := platform.OwnConsole(); err != nil {
		fmt.Fprintln(os.Stderr, "longarmd conhost: console:", err)
	}
	var cmd *exec.Cmd
	if raw {
		cmd = exec.Command(argv[0])
		if err := platform.SetCmdLine(cmd, argv[1]); err != nil {
			fmt.Fprintln(os.Stderr, "longarmd conhost:", err)
			return 2
		}
	} else {
		cmd = exec.Command(argv[0], argv[1:]...)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		return ee.ExitCode()
	default:
		fmt.Fprintf(os.Stderr, "longarmd: failed to start %q: %v\n", argv[0], err)
		return 127
	}
}
