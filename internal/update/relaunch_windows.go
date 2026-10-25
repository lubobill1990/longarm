package update

import (
	"os/exec"

	"github.com/lubobill1990/longarm/internal/platform"
)

// Relaunch starts exe detached (exec mode restart after an update); the
// caller exits right after.
func Relaunch(exe string, args []string) error {
	cmd, err := platform.StartDetached(func() *exec.Cmd { return exec.Command(exe, args...) })
	if err == nil {
		cmd.Process.Release()
	}
	return err
}
