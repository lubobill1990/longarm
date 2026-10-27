package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/lubobill1990/longarm/internal/longarmd"
	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/transport/longnet"
)

// serviceFirewall: longarmd.ServiceFirewall on the console.
func serviceFirewall(service, exe string, add bool) {
	longarmd.ServiceFirewall(service, exe, add, os.Stdout, os.Stderr)
}

var punchHintOnce sync.Once

func init() {
	longnet.PunchFailedHook = func(peer string, sent, received int) {
		if runtime.GOOS != "windows" || received > 0 {
			return
		}
		go punchHintOnce.Do(func() {
			exe, err := os.Executable()
			if err != nil {
				return
			}
			if st, err := platform.CheckFirewall(exe); err == nil && !st.OK() {
				fmt.Fprintf(os.Stderr, "longarmd: hole punching to %s got no answer (%d pings): %s. Fix (elevated): %s\n",
					peer, sent, st.Problem(), platform.FirewallFixCommand(longarmBeside(exe), exe))
			}
		})
	}
}

// longarmBeside is the longarm.exe installed next to exe (the MSI puts them in one
// directory), else "" (longarm on PATH).
func longarmBeside(exe string) string {
	longarm := filepath.Join(filepath.Dir(exe), "longarm.exe")
	if _, err := os.Stat(longarm); err == nil {
		return longarm
	}
	return ""
}
