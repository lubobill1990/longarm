package longarmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/lubobill1990/longarm/internal/platform"
)

// DefaultServiceName is the longarmd service's name (SCM / systemd /
// launchd).
const DefaultServiceName = "longarmd"

// ServiceInstallOptions for InstallService (`longarmd service install`).
type ServiceInstallOptions struct {
	Name string // default DefaultServiceName
	// Exe: the longarmd binary the service runs (default: this
	// executable if it is longarmd, else longarmd next to it - e.g. the
	// tray's).
	Exe string
	// DataDir and whether it was chosen explicitly (--data-dir /
	// LONGARM_DATA_DIR); else ResolveServiceDataDir decides.
	DataDir         string
	DataDirExplicit bool
	// User the service runs as (Windows ".\name" / "DOMAIN\name"; its
	// Password); "" = LocalSystem / root (warned about).
	User, Password string
	Isolation      string   // "" / "shared" or "per-user"
	Listen         []string // --listen URLs
	DryRun         bool     // print what would happen
	Out, Warn      io.Writer
}

// InstallService installs longarmd as a system service that starts at
// boot (`longarmd service install`). It needs an elevated process (root
// / Administrator); a GUI runs `longarmd service install` elevated.
func InstallService(o ServiceInstallOptions) error {
	if o.Name == "" {
		o.Name = DefaultServiceName
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Warn == nil {
		o.Warn = io.Discard
	}
	exe, err := serviceExe(o.Exe)
	if err != nil {
		return err
	}
	dataDir, err := filepath.Abs(o.DataDir)
	if err != nil {
		return err
	}
	perUser := o.Isolation == "per-user"
	switch {
	case perUser && o.User != "":
		return errors.New("--isolation per-user runs the service as LocalSystem / root: drop --user")
	case perUser:
		if err := platform.CanIsolate(); err != nil && runtime.GOOS == "darwin" {
			return err
		}
	case o.Isolation != "" && o.Isolation != "shared":
		return fmt.Errorf("--isolation %q: want shared or per-user", o.Isolation)
	}
	sd, err := ResolveServiceDataDir(ServiceDirRequest{DataDir: dataDir, Explicit: o.DataDirExplicit, PerUser: perUser, User: o.User})
	if err != nil {
		return err
	}
	dataDir = sd.Dir
	args := []string{"run", "--data-dir", dataDir, "--service-name", o.Name}
	for _, l := range o.Listen {
		args = append(args, "--listen", l)
	}
	cfg := platform.ServiceConfig{
		Name: o.Name, DisplayName: "Longarm worker (" + o.Name + ")",
		Description: "Longarm worker daemon: runs remote jobs for longarm clients",
		Exe:         exe, Args: args, User: o.User, Password: o.Password,
	}
	if o.DryRun {
		fmt.Fprintf(o.Out, "data dir %s\n", dataDir)
		if sd.MigrateFrom != "" {
			fmt.Fprintf(o.Out, "would copy the enrollment from %s to %s\n", sd.MigrateFrom, dataDir)
		}
		if perUser {
			fmt.Fprintf(o.Out, "users dir %s\nwould set [isolation] mode = \"per-user\" in %s\n", sd.UsersDir, filepath.Join(dataDir, ConfigFile))
		}
		fmt.Fprintf(o.Out, "would install service %q (%s %v, user %q)\n", o.Name, exe, args, o.User)
		return nil
	}
	if sd.MigrateFrom != "" {
		// Check first: an old service (e.g. root's shared one on the old
		// dir) must go before its enrollment is copied.
		if st, err := platform.QueryService(o.Name); err == nil && st != platform.ServiceNotInstalled {
			return fmt.Errorf("service %q is already installed (%s): longarmd service uninstall --name %s first", o.Name, st, o.Name)
		}
		fmt.Fprintf(o.Out, "copying the enrollment from %s to %s:\n", sd.MigrateFrom, dataDir)
		if err := MigrateEnrollment(sd.MigrateFrom, dataDir, o.Out); err != nil {
			return err
		}
		fmt.Fprintf(o.Out, "%s is no longer used by this service; remove it once the service runs (it holds a copy of the node key)\n", sd.MigrateFrom)
	}
	switch {
	case perUser:
		if err := EnsureSystemDataDir(dataDir); err != nil {
			return err
		}
		if err := SetIsolation(dataDir, o.Isolation); err != nil {
			return err
		}
		fmt.Fprintf(o.Warn, "per-user isolation: the service runs as LocalSystem / root (data dir %s) and runs every user's work as an OS account of its own\n", dataDir)
	case o.User == "":
		fmt.Fprintln(o.Warn, "warning: no --user given; the service runs as LocalSystem/root, which DESIGN §3.1 advises against")
	}
	if err := platform.InstallService(cfg); err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "installed service %q (%s %v)\n", o.Name, exe, args)
	ServiceFirewall(o.Name, exe, true, o.Out, o.Warn)
	return nil
}

// EnsureWorkerService makes the worker service run after a sign-in:
// installed (InstallService) when missing, started when stopped. A
// service already running is left alone (one waiting for its sign-in
// starts serving by itself). Installing and starting need an elevated
// process (a GUI runs `longarmd service install` / `start` elevated).
func EnsureWorkerService(o ServiceInstallOptions) error {
	if o.Name == "" {
		o.Name = DefaultServiceName
	}
	st, err := platform.QueryService(o.Name)
	if err != nil {
		return err
	}
	switch st {
	case platform.ServiceRunning:
		return nil
	case platform.ServiceNotInstalled:
		if err := InstallService(o); err != nil {
			return err
		}
	}
	return platform.StartService(o.Name)
}

// serviceExe is the longarmd binary a service runs.
func serviceExe(exe string) (string, error) {
	if exe != "" {
		return filepath.Abs(exe)
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(strings.ToLower(filepath.Base(self)), "longarmd") {
		return self, nil
	}
	name := "longarmd"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	p := filepath.Join(filepath.Dir(self), name)
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("no longarmd next to %s: %w", self, err)
	}
	return p, nil
}

// ServiceFirewall adds (or removes) the inbound allow rules for the
// service's longarmd.exe on Windows when elevated: without them Windows
// Firewall drops unsolicited inbound UDP, and hole punching / tailscale
// direct paths fail. The MSI manages its own rules for longarm, longarmd
// and longarm-tray.
func ServiceFirewall(service, exe string, add bool, out, warn io.Writer) {
	if runtime.GOOS != "windows" {
		return
	}
	name := "Longarm longarmd service " + service
	if !add {
		platform.RemoveFirewallRules(name, exe)
		return
	}
	if !platform.Elevated() {
		fmt.Fprintf(warn, "note: not elevated, no firewall rule added; direct (punched) connections need an inbound allow rule for %s\n", exe)
		return
	}
	if err := platform.AddFirewallRules(name, exe); err != nil {
		fmt.Fprintf(warn, "warning: firewall rule: %v\n", err)
		return
	}
	fmt.Fprintf(out, "firewall: inbound UDP+TCP allowed for %s (rules %q)\n", exe, platform.FirewallRuleNames(name))
}
