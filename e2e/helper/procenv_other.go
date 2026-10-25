//go:build !windows

package main

import (
	"fmt"
	"os"
	"strings"
)

func homeVar() string { return "HOME" }

// procEnv reads /proc/<pid>/environ (Linux; ptrace-mode access).
func procEnv(pid int) (string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(string(b), "\x00", "\n"), nil
}
