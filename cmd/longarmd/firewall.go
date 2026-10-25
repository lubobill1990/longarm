package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/transport/longnet"
)

// serviceFirewall adds (or removes) the inbound allow rules for the
// service's longarmd.exe on Windows when elevated: without them Windows Firewall
// drops unsolicited inbound UDP, and hole punching / tailscale direct paths
// fail. The MSI manages its own rules for longarm, longarmd and longarm-tray.
func serviceFirewall(service, exe string, add bool) {
	if runtime.GOOS != "windows" {
		return
	}
	name := "Longarm longarmd service " + service
	if !add {
		platform.RemoveFirewallRules(name, exe)
		return
	}
	if !platform.Elevated() {
		fmt.Fprintf(os.Stderr, "note: not elevated, no firewall rule added; direct (punched) connections need an inbound allow rule for %s\n", exe)
		return
	}
	if err := platform.AddFirewallRules(name, exe); err != nil {
		fmt.Fprintf(os.Stderr, "warning: firewall rule: %v\n", err)
		return
	}
	fmt.Printf("firewall: inbound UDP+TCP allowed for %s (rules %q)\n", exe, platform.FirewallRuleNames(name))
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
