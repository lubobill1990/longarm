// Package fwcheck reports the Windows Firewall verdict for the running
// executable (longarm daemon's longarm.exe, longarmd.exe) in the local API's form: a
// Block rule left by a dismissed firewall prompt, or a missing allow rule,
// silently keeps hole-punched connections (longnet, tailscale) on relays.
package fwcheck

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/lubobill1990/longarm/internal/platform"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Status checks program; longarmExe is the longarm.exe for the fix command ("" =
// longarm on PATH). nil where nothing is checked (outside Windows) or the
// rules can't be read.
func Status(program, longarmExe string) *pb.FirewallStatus {
	st, err := platform.CheckFirewall(program)
	if err != nil || !st.Checked {
		return nil
	}
	fs := &pb.FirewallStatus{Program: st.Program, Ok: st.OK(), Allowed: st.Allowed, Blocking: st.Blocking, Problem: st.Problem()}
	if !st.OK() {
		if fix, err := platform.FirewallFixCommand(longarmExe, st.Program); err == nil {
			fs.Fix = fix
		} else {
			// no command to paste: the tray's Allow still works
			fs.Problem += " (no fix command: the " + err.Error() + ")"
		}
	}
	return fs
}

// Self checks this process' executable. longarm.exe is its own fix command;
// anything else (longarmd.exe) uses the longarm.exe installed next to it, if any.
func Self() *pb.FirewallStatus {
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	longarm := exe
	if base := filepath.Base(exe); base != "longarm.exe" && base != "longarm" {
		longarm = filepath.Join(filepath.Dir(exe), "longarm.exe")
		if _, err := os.Stat(longarm); err != nil {
			longarm = ""
		}
	}
	return Status(exe, longarm)
}

// TTL: how long a Cache reuses a check.
const TTL = 30 * time.Second

// Cache is Self, re-checked at most every TTL (status calls and pool views
// come often; the check reads every firewall rule).
type Cache struct {
	mu sync.Mutex
	at time.Time
	st *pb.FirewallStatus
}

// Get returns the (cached) verdict; nil outside Windows.
func (c *Cache) Get() *pb.FirewallStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at.IsZero() || time.Since(c.at) > TTL {
		c.st, c.at = Self(), time.Now()
	}
	if c.st == nil {
		return nil
	}
	return proto.Clone(c.st).(*pb.FirewallStatus)
}
