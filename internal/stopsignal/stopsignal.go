// Package stopsignal lets the installer ask a Longarm process to stop
// gracefully (Windows): the tray, the user-session worker's host and
// `longarmd run` each create a named kernel event, Global\LongarmStop-<pid>,
// that only the process's own user, SYSTEM and Administrators may set
// (NotifyContext); the MSI's upgrade step sets it (Signal) and waits, and
// terminates the process by its pid only when it has no such event (an
// older version) or doesn't exit in time.
//
// The name is keyed by the process id, so the stopper signals exactly the
// process it found (by image path, user and start time). A process that
// finds its name taken (squatted by another user) does without: it is then
// terminated instead of asked, never stopped by someone else's event.
package stopsignal

import "strconv"

// Name is the stop event of process pid.
func Name(pid int) string { return `Global\LongarmStop-` + strconv.Itoa(pid) }
