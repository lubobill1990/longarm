//go:build windows

package main

import (
	"context"
	"log/slog"
	"time"

	"golang.org/x/sys/windows"
)

// installerRunning reports whether Windows Installer is running an install
// (it holds the Global\_MSIExecute mutex while it executes one).
func installerRunning() bool {
	name, _ := windows.UTF16PtrFromString(`Global\_MSIExecute`)
	h, err := windows.OpenMutex(windows.SYNCHRONIZE, false, name)
	if err != nil {
		return false // ERROR_FILE_NOT_FOUND: no install runs
	}
	windows.CloseHandle(h)
	return true
}

// waitForInstaller holds the tray's start while an install runs, up to max:
// the installer starts the tray (LongarmTrayLaunch, the upgrade's restart)
// from its script, before it has finished - and a WebView2 created
// meanwhile came up blank for good (the popup "all white"). The icon and
// the popup come once it's done.
func waitForInstaller(ctx context.Context, log *slog.Logger, max time.Duration) {
	if !installerRunning() {
		return
	}
	log.Info("Windows Installer is running: waiting for it before starting the tray")
	start := time.Now()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for installerRunning() {
		if time.Since(start) >= max {
			log.Warn("Windows Installer still running: starting the tray anyway", "waited", time.Since(start).Round(time.Second))
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
	// the installer's last steps (its commit, the old product's removal)
	// end just before it lets go
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
	}
	log.Info("Windows Installer done: starting the tray", "waited", time.Since(start).Round(time.Second))
}
