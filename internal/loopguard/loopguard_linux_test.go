//go:build linux

package loopguard

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestRulesetNftCheck lets the kernel's nftables validate the ruleset
// without applying it (nft -c), where possible: Linux, root, nft.
func TestRulesetNftCheck(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs Linux and root")
	}
	if _, err := nftPath(); err != nil {
		t.Skip("warning: nft not installed; the ruleset is not validated here")
	}
	script, _ := Ruleset([]User{{"la-a", 61001}, {"la-b", 61002}})
	nft, _ := nftPath()
	cmd := exec.Command(nft, "-c", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("nft -c rejects the ruleset: %v\n%s\n%s", err, out, script)
	}
}
