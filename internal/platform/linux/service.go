//go:build linux

package linux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/lubobill1990/longarm/internal/platform/ptypes"
)

func unitPath(name string) string { return filepath.Join("/etc/systemd/system", name+".service") }

// UnitFile renders the systemd unit. KillMode=process is essential: the
// default (control-group) would kill every job shim when longarmd restarts.
func UnitFile(cfg ptypes.ServiceConfig) string {
	var b strings.Builder
	q := make([]string, 0, len(cfg.Args)+1)
	for _, a := range append([]string{cfg.Exe}, cfg.Args...) {
		q = append(q, systemdQuote(a))
	}
	fmt.Fprintf(&b, "[Unit]\nDescription=%s\nAfter=network-online.target\nWants=network-online.target\n\n", cfg.Description)
	fmt.Fprintf(&b, "[Service]\nType=simple\nExecStart=%s\n", strings.Join(q, " "))
	if cfg.User != "" {
		fmt.Fprintf(&b, "User=%s\n", cfg.User)
	}
	b.WriteString("Restart=always\nRestartSec=5\nKillMode=process\nLimitNOFILE=65536\n\n[Install]\nWantedBy=multi-user.target\n")
	return b.String()
}

func systemdQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"'\\$%") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `$$`, `%`, `%%`)
	return `"` + r.Replace(s) + `"`
}

func systemctl(args ...string) error {
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// InstallService writes the unit, reloads systemd and enables it.
func InstallService(cfg ptypes.ServiceConfig) error {
	if _, err := os.Stat(unitPath(cfg.Name)); err == nil {
		return fmt.Errorf("unit %s already exists", unitPath(cfg.Name))
	}
	if err := os.WriteFile(unitPath(cfg.Name), []byte(UnitFile(cfg)), 0o644); err != nil {
		return err
	}
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	return systemctl("enable", cfg.Name+".service")
}

// UninstallService stops, disables and removes the unit.
func UninstallService(name string) error {
	_ = systemctl("stop", name+".service")
	_ = systemctl("disable", name+".service")
	if err := os.Remove(unitPath(name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return systemctl("daemon-reload")
}

func StartService(name string) error { return systemctl("start", name+".service") }
func StopService(name string) error  { return systemctl("stop", name+".service") }

// QueryService maps `systemctl is-active` to a coarse status.
func QueryService(name string) (ptypes.ServiceStatus, error) {
	if _, err := os.Stat(unitPath(name)); err != nil {
		return ptypes.ServiceNotInstalled, nil
	}
	out, _ := exec.Command("systemctl", "is-active", name+".service").Output()
	switch strings.TrimSpace(string(out)) {
	case "active":
		return ptypes.ServiceRunning, nil
	case "inactive", "failed":
		return ptypes.ServiceStopped, nil
	case "activating", "deactivating", "reloading":
		return ptypes.ServicePending, nil
	}
	return ptypes.ServiceUnknown, nil
}

// RunAsService: systemd runs longarmd as a plain foreground process.
func RunAsService(string, func(context.Context) error) (bool, error) { return false, nil }
