package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/lubobill1990/longarm/internal/tray"
)

// The progress window of an update this tray started
// (internal/tray/updateprogress.go) runs from a copy of longarm-tray.exe
// in %LOCALAPPDATA%\Longarm\update-progress\run-*: the installer stops
// and replaces every Longarm process whose image is in the install dir,
// this tray among them; the copy outlives it. Each start gets a fresh
// directory (an earlier window may still run from its copy); those of
// windows that ended are removed then.

// progressDir is the progress window's directory (its copies, its
// WebView2 profile in webview\).
func progressDir() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "Longarm", "update-progress")
}

// launchUpdateProgress starts the progress window for version.
func launchUpdateProgress(o tray.Options, version string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	base := progressDir()
	if err := os.MkdirAll(base, 0o700); err != nil {
		return err
	}
	if old, _ := filepath.Glob(filepath.Join(base, "run-*")); len(old) > 0 {
		for _, d := range old {
			os.RemoveAll(d) // a running copy stays (its exe is in use)
		}
	}
	dir, err := os.MkdirTemp(base, "run-*")
	if err != nil {
		return err
	}
	exe := filepath.Join(dir, "longarm-update-progress.exe")
	if err := copyFile(self, exe); err != nil {
		os.RemoveAll(dir)
		return fmt.Errorf("copy the tray for the progress window: %w", err)
	}
	theme := tray.EffectiveTheme(tray.LoadPrefs(o.LongarmConfigDir).Theme, tray.SystemLightTheme())
	cmd := exec.Command(exe, "--update-progress", strings.TrimPrefix(version, "v"), "--theme", theme, "--config-dir", o.LongarmConfigDir)
	if err := cmd.Start(); err != nil {
		os.RemoveAll(dir)
		return err
	}
	return cmd.Process.Release()
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
