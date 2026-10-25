package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/transport"
	"github.com/lubobill1990/longarm/internal/transport/longnet"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

var punchHintOnce sync.Once

// On Windows, a failed hole punch with no answer at all is usually Windows
// Firewall dropping unsolicited inbound UDP to longarm.exe (it acts like a
// port-restricted NAT): a missing allow rule or a Block rule left by a
// dismissed firewall prompt. Point at it once per process.
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
				fmt.Fprintf(os.Stderr, "longarm: hint: hole punching to %s got no answer (%d pings): %s.\n  Fix (elevated): %s\n",
					peer, sent, st.Problem(), platform.FirewallFixCommand(exe, exe))
			}
		})
	}
}

// firewallRuleName is the name of the rules `longarm firewall allow` manages
// for a program ("Longarm longarm" for longarm.exe, the MSI's names; rules are
// always added and removed for one program path, so the MSI's rules for
// another path are left alone).
func firewallRuleName(program string) string {
	return "longarm " + strings.TrimSuffix(strings.ToLower(filepath.Base(program)), ".exe")
}

// relayedKind reports whether a path kind means a relay
// (longnet relay, tailscale DERP / peer relay).
func relayedKind(kind string) bool {
	switch transport.PathKind(kind) {
	case transport.PathRelay, transport.PathDERP, transport.PathPeerRelay:
		return true
	}
	return false
}

// firewallNote is the line longarm ls / longarm pool print when some legs are
// relayed and the firewall is a likely reason ("" = nothing to say).
func firewallNote(fw *pb.FirewallStatus, relayed []string) string {
	if fw == nil || fw.Ok || len(relayed) == 0 {
		return ""
	}
	return fmt.Sprintf("warning: relayed to %s - %s.\n  fix (elevated): %s", strings.Join(relayed, ", "), fw.Problem, fw.Fix)
}

// firewallCmd is `longarm firewall [status|allow|remove]`: Windows Firewall drops
// unsolicited inbound UDP to programs without an allow rule (and always to
// programs with a Block rule), which makes hole punching fail (the
// connection stays on the relay). Installs without the MSI, or after a
// dismissed firewall prompt, use this to fix it for longarm.exe (or --program,
// e.g. longarmd.exe), elevated.
func firewallCmd() *cobra.Command {
	var program string
	c := &cobra.Command{
		Use:   "firewall [status|allow|remove]",
		Short: "Windows Firewall inbound rules for longarm (needed for direct, hole-punched connections; MSI installs have them)",
		Long: `Windows Firewall drops unsolicited inbound UDP to a program without an
inbound allow rule, and always to a program with an inbound Block rule
(dismissing the "allow access?" prompt adds one). Hole punching (longnet,
tailscale) then fails and connections stay on relays.

  status  allow rule present and no Block rule? (exit 1 if not)
  allow   remove the program's inbound Block rules, add allow rules (elevated)
  remove  remove the allow rules added for the program (elevated)

--program checks/fixes another executable (e.g. longarmd.exe) instead of this longarm.exe.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			action := "status"
			if len(args) == 1 {
				action = args[0]
			}
			self, err := os.Executable()
			if err != nil {
				return err
			}
			exe := self
			if program != "" {
				if exe, err = filepath.Abs(program); err != nil {
					return err
				}
			}
			if runtime.GOOS != "windows" && (action != "status" || os.Getenv(platform.TestFirewallRulesEnv) == "") {
				fmt.Println("nothing to do: longarm manages host firewalls only on Windows (see the README for ufw/firewalld and the macOS application firewall)")
				return nil
			}
			name := firewallRuleName(exe)
			switch action {
			case "status":
				st, err := platform.CheckFirewall(exe)
				if err != nil {
					return err
				}
				if st.OK() {
					fmt.Printf("ok: inbound allow rule for %s, no Block rule\n", exe)
					return nil
				}
				what := "missing"
				if len(st.Blocking) > 0 {
					what = "blocked"
				}
				fmt.Printf("%s: %s\nfix (elevated): %s\n", what, st.Problem(), platform.FirewallFixCommand(self, exe))
				return &exitError{code: exitLongarmError}
			case "allow", "remove":
				if !platform.Elevated() {
					return fmt.Errorf("longarm firewall %s needs an elevated (administrator) shell", action)
				}
				if action == "remove" {
					return platform.RemoveFirewallRules(name, exe)
				}
				removed, err := platform.RemoveFirewallBlockRules(exe)
				if len(removed) > 0 {
					fmt.Printf("firewall: removed inbound Block rules %q for %s\n", removed, exe)
				}
				if err != nil {
					return err
				}
				if err := platform.AddFirewallRules(name, exe); err != nil {
					return err
				}
				fmt.Printf("firewall: inbound UDP+TCP allowed for %s (rules %q)\n", exe, platform.FirewallRuleNames(name))
				return nil
			}
			return fmt.Errorf("unknown action %q (status, allow, remove)", action)
		},
	}
	c.Flags().StringVar(&program, "program", "", "the executable to check or fix (default: this longarm.exe)")
	return c
}
