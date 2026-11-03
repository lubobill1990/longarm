//go:build !windows

package toolchain

import (
	"context"
	"os/exec"
)

func refreshPath() error { return nil }

func systemCommand(ctx context.Context, argv ...string) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, argv[0], argv[1:]...), nil
}
