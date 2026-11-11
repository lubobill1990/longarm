//go:build !windows

package main

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/lubobill1990/longarm/internal/tray"
)

// The progress window is for the Windows installer's updates
// (updateprogress_windows.go); elsewhere the tray never starts it.

func progressDir() string { return filepath.Join(os.TempDir(), "longarm-update-progress") }

func launchUpdateProgress(tray.Options, string) error {
	return errors.New("the update progress window is Windows only")
}
