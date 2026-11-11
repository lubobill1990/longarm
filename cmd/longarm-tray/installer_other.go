//go:build !windows

package main

import (
	"context"
	"log/slog"
	"time"
)

// waitForInstaller: only Windows Installer starts the tray mid-install.
func waitForInstaller(context.Context, *slog.Logger, time.Duration) {}
