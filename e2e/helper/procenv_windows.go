//go:build windows

package main

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func homeVar() string { return "USERPROFILE" }

// procEnv opens pid for reading its memory (where its environment lives):
// that is the access another account must not get.
func procEnv(pid int) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_VM_READ|windows.PROCESS_QUERY_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", err
	}
	windows.CloseHandle(h)
	return fmt.Sprintf("opened process %d for reading", pid), nil
}
