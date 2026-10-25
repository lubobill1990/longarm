// Command fakehub stands in for longarm-hub releases in TestHubDeployScript
// (cheaper than building longarm-hub once per version): `--version` reports the
// version stamped in at build time, everything else (release verify) runs
// the real longarm-hub stamped in. With oldHub set it behaves like a longarm-hub
// that predates `release verify`: cobra prints the `release` help for the
// unknown subcommand (exit 0) and refuses its flags.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"slices"
)

var version, realHub, oldHub string

func main() {
	args := os.Args[1:]
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("longarm-hub version " + version)
		return
	}
	if oldHub != "" && len(args) > 0 && args[0] == "release" {
		if slices.Contains(args, "--help") {
			fmt.Println("Manage the release store (install scripts, self-update)\n\nUsage:\n  longarm-hub release [command]\n\nAvailable Commands:\n  add  keygen  ls  sign")
			return
		}
		fmt.Fprintln(os.Stderr, "longarm-hub: unknown flag: --version")
		os.Exit(1)
	}
	cmd := exec.Command(realHub, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
