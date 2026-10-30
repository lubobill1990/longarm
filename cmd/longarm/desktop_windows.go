//go:build windows

package main

import (
	"os/exec"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// openDesktopURL hands an ms-avd: or https link to the shell (Windows App
// registers ms-avd:).
func openDesktopURL(u string) error {
	verb, err := windows.UTF16PtrFromString("open")
	if err != nil {
		return err
	}
	target, err := windows.UTF16PtrFromString(u)
	if err != nil {
		return err
	}
	return windows.ShellExecute(0, verb, target, nil, nil, windows.SW_SHOWNORMAL)
}

// launchRDP opens the .rdp file in mstsc and returns a function waiting
// for it to exit.
func launchRDP(rdpFile, _ string) (func() error, error) {
	mstsc := filepath.Join(windowsDir(), "System32", "mstsc.exe")
	cmd := exec.Command(mstsc, rdpFile)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd.Wait, nil
}

func windowsDir() string {
	if d, err := windows.GetWindowsDirectory(); err == nil {
		return d
	}
	return `C:\Windows`
}
