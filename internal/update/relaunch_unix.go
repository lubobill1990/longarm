//go:build !windows

package update

import (
	"os"
	"syscall"
)

// Relaunch replaces the process with exe (exec mode restart after an
// update; same pid, same terminal).
func Relaunch(exe string, args []string) error {
	return syscall.Exec(exe, append([]string{exe}, args...), os.Environ())
}
