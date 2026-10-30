//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

func openDesktopURL(u string) error {
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	cmd := exec.Command(opener, u)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// launchRDP opens the .rdp file in Windows App (macOS) or FreeRDP
// (elsewhere). Both keep running on their own: nil waiter.
func launchRDP(rdpFile, addr string) (func() error, error) {
	if runtime.GOOS == "darwin" {
		return nil, exec.Command("open", rdpFile).Run()
	}
	for _, c := range []string{"xfreerdp3", "xfreerdp"} {
		if p, err := exec.LookPath(c); err == nil {
			cmd := exec.Command(p, "/v:"+addr)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := cmd.Start(); err != nil {
				return nil, err
			}
			return cmd.Wait, nil
		}
	}
	fmt.Fprintf(os.Stderr, "longarm: no Remote Desktop client found (install FreeRDP); connect yours to %s\n", addr)
	return nil, nil
}
