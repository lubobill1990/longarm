//go:build !windows

package toolchain

import (
	"context"
	"os/exec"
	"time"
)

func refreshPath(_ ...string) error { return nil }

func probeOutput(ctx context.Context, argv ...string) ([]byte, error) {
	cmd, err := systemCommand(ctx, argv...)
	if err != nil {
		return nil, err
	}
	cmd.WaitDelay = 250 * time.Millisecond
	return cmd.Output()
}

func systemCommand(ctx context.Context, argv ...string) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, argv[0], argv[1:]...), nil
}
