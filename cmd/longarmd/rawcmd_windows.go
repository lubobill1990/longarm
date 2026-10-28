//go:build windows

package main

import "golang.org/x/sys/windows"

// rawCommandLine is the command line as Windows Installer formatted it (the
// MSI data after longarmd.MSIDataMarker is read from it verbatim).
func rawCommandLine() string { return windows.UTF16PtrToString(windows.GetCommandLine()) }
