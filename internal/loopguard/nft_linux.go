//go:build linux

package loopguard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Supported reports whether the platform has loopback isolation (Linux).
const Supported = true

// nftPath finds the nft binary (root's PATH under systemd has /usr/sbin;
// a login shell's may not).
func nftPath() (string, error) {
	if p, err := exec.LookPath("nft"); err == nil {
		return p, nil
	}
	for _, p := range []string{"/usr/sbin/nft", "/sbin/nft", "/usr/local/sbin/nft"} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", errors.New("nft not found (install the nftables package)")
}

// run feeds script to nft -f -.
func run(script string) error {
	if os.Geteuid() != 0 {
		return errors.New("loopback isolation needs root")
	}
	nft, err := nftPath()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, nft, "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("nft: %v: %s", err, strings.TrimSpace(out.String()))
	}
	return nil
}

// Apply installs (or replaces) the table for users.
func Apply(users []User) error {
	script, err := Ruleset(users)
	if err != nil {
		return err
	}
	return run(script)
}

// Remove deletes the table (nothing to do if it or nft is missing).
func Remove() error {
	if os.Geteuid() != 0 {
		return nil
	}
	if _, err := nftPath(); err != nil {
		return nil
	}
	return run(RemoveScript())
}
