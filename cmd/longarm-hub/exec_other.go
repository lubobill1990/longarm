//go:build !unix

package main

import (
	"os"
	"os/exec"
	"os/signal"
	"strconv"
)

// execve runs argv as a child and exits with its status (no exec(2) here;
// the container entrypoint is meant for Linux images).
func execve(path string, argv []string) error {
	c := exec.Command(path, argv[1:]...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Start(); err != nil {
		return err
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		for range sig {
			c.Process.Kill()
		}
	}()
	err := c.Wait()
	signal.Stop(sig)
	if ee, ok := err.(*exec.ExitError); ok {
		os.Exit(ee.ExitCode())
	}
	return err
}

// sigKill: no signals here (Windows); the restore never ends by one.
const sigKill = 9

func exitSignal(*os.ProcessState) (int, bool) { return 0, false }

func signalName(sig int) string { return "signal " + strconv.Itoa(sig) }
