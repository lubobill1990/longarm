package longarmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/tenancy"
	"github.com/lubobill1990/longarm/internal/transport"
	"github.com/lubobill1990/longarm/internal/userspace"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// LeaveOptions for Leave.
type LeaveOptions struct {
	// Reason is recorded by the hub (audit "self/<name> (reason)").
	Reason string
	// Force does the local part even when the hub can't be reached (the
	// node then stays registered there until an admin revokes it).
	Force bool
	// KillJobs ends running jobs and sessions (their shims) first.
	KillJobs bool
	// Purge removes the whole data dir (jobs, mirrors, logs) instead of only
	// the credentials and hub settings.
	Purge bool
	Out   io.Writer
	// locked: the caller (the running longarmd) already holds the data-dir lock.
	locked bool
	// logout logs the running longarmd's tailscale nodes out and returns their
	// state dirs (Leave starts the other ones from their state to log them
	// out).
	logout func(context.Context) []string
}

// Leave deregisters the worker from its hub (`longarmd leave`, the local API,
// uninstallers): the hub revokes the node's certificates, then the
// credentials (tls/, hub.toml, tsnet state) are removed locally. Without
// the local API, longarmd must not be running. A hub that can't be reached is an
// error unless Force is set. It returns the hub's message.
func Leave(ctx context.Context, dataDir string, o LeaveOptions) (string, error) {
	if o.Out == nil {
		o.Out = io.Discard
	}
	unlock := func() {}
	if !o.locked {
		u, err := lockDataDir(dataDir)
		if err != nil {
			return "", fmt.Errorf("%w (stop it first, or run longarmd leave while it runs to leave through it)", err)
		}
		var once sync.Once
		unlock = func() { once.Do(u) }
	}
	defer unlock()
	cfg, err := LoadConfig(dataDir, nil)
	if err != nil {
		return "", err
	}
	msg := "no hub configured"
	if cfg.Hub.Address != "" {
		b, err := pki.LoadBundle(filepath.Join(dataDir, TLSDir))
		if err != nil && !o.Force {
			return "", fmt.Errorf("no certificate in %s: %w (--force removes the hub settings anyway)", filepath.Join(dataDir, TLSDir), err)
		}
		if err == nil {
			m, herr := hubLeave(ctx, cfg.Hub.Address, b, o.Reason)
			switch {
			case herr == nil:
				msg = m
				fmt.Fprintf(o.Out, "left hub %s as %s\n", cfg.Hub.Address, b.Identity.Name)
			case !o.Force:
				return "", fmt.Errorf("hub %s: %w (use --force to leave locally anyway)", cfg.Hub.Address, herr)
			default:
				msg = "hub unreachable (" + herr.Error() + "); left locally only - revoke the node on the hub: longarm admin revoke " + b.Identity.Name
				fmt.Fprintln(o.Out, "warning:", msg)
			}
		}
	}
	logoutTailnet(ctx, dataDir, o)
	if o.KillJobs {
		if n := KillShims(dataDir); n > 0 {
			fmt.Fprintf(o.Out, "killed %d running job/session processes\n", n)
			time.Sleep(500 * time.Millisecond) // let them release their files
		}
	}
	if o.Purge && !o.locked {
		unlock()
		deleteUserAccounts(cfg, o.Out)
		if err := os.RemoveAll(dataDir); err != nil {
			return msg, fmt.Errorf("remove %s: %w", dataDir, err)
		}
		fmt.Fprintf(o.Out, "removed %s\n", dataDir)
		return msg, nil
	}
	for _, p := range []string{TLSDir, hubclient.File} {
		if err := os.RemoveAll(filepath.Join(dataDir, p)); err != nil {
			return msg, err
		}
	}
	// Best effort: a running longarmd may still hold its tsnet state open.
	hubclient.RemoveTailnetState(dataDir)
	os.RemoveAll(filepath.Join(dataDir, RevokedFile))
	fmt.Fprintf(o.Out, "removed the credentials from %s (jobs and mirrors kept; --purge removes everything)\n", dataDir)
	return msg, nil
}

func hubLeave(ctx context.Context, addr string, b *pki.Bundle, reason string) (string, error) {
	conn, err := hubclient.Dial(addr, b)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	r, err := conn.Node().Leave(cctx, &pb.LeaveRequest{Reason: reason})
	if err != nil {
		return "", err
	}
	return r.Message, nil
}

// KillShims kills the shims (and main processes) of running jobs and
// sessions in dataDir; it returns how many processes it killed.
func KillShims(dataDir string) int {
	n := 0
	roots := []string{dataDir}
	usersDir := filepath.Join(dataDir, userspace.UsersDir)
	if cfg, err := LoadConfig(dataDir, nil); err == nil && cfg.Isolation.UsersDir != "" {
		usersDir = cfg.Isolation.UsersDir
	}
	users, _ := os.ReadDir(usersDir)
	for _, u := range users {
		if u.IsDir() {
			roots = append(roots, filepath.Join(usersDir, u.Name()))
		}
	}
	for _, root := range roots {
		n += killShimsIn(root)
	}
	return n
}

// killShimsIn kills the shims of one space.
func killShimsIn(dataDir string) int {
	n := 0
	for _, kind := range []string{JobsDir, SessionsDir} {
		ents, _ := os.ReadDir(filepath.Join(dataDir, kind))
		for _, de := range ents {
			st, err := jobs.Dir(filepath.Join(dataDir, kind, de.Name())).ReadStatus()
			if err != nil || st.State.Terminal() {
				continue
			}
			if platform.ProcessAlive(st.ShimPID, st.ShimStart) && platform.KillProcess(st.ShimPID) == nil {
				n++
			}
			if platform.ProcessAlive(st.PID, st.PIDStart) && platform.KillProcess(st.PID) == nil {
				n++
			}
		}
	}
	return n
}

// SetupOptions for InstallSetup (the MSI's deferred custom action).
type SetupOptions struct {
	ServiceUser string // account the service runs as ("" = LocalSystem)
	Hub         string
	Code        string
	Fingerprint string
	Listen      []string // written to a new longarmd.toml (listen = [...])
	Name        string   // node name (default: hostname)
	Out         io.Writer
	// Isolation "per-user": longarmd (LocalSystem) runs each user's work as
	// an OS account of its own (docs/security.md).
	Isolation string
	// Login (the MSI's LOGIN=1, no join code): record Hub (and
	// Fingerprint) for a sign-in to come - the service waits for it, the
	// tray or `longarmd login --role both` does it (SetUpSignIn).
	Login bool
	// Admin: the admin worker (LocalSystem on purpose): no warning about
	// jobs running as SYSTEM.
	Admin bool
}

// InstallSetup prepares a machine-wide data dir for the longarmd service: the
// directory is created with an ACL for SYSTEM, Administrators and the
// service account only; the account gets "Log on as a service"; with a
// join code the worker enrolls (without waiting: the service finishes it).
func InstallSetup(ctx context.Context, dataDir string, o SetupOptions) error {
	if o.Out == nil {
		o.Out = io.Discard
	}
	perUser := o.Isolation == tenancy.PerUser
	if perUser && o.ServiceUser != "" && !strings.EqualFold(o.ServiceUser, "LocalSystem") {
		return fmt.Errorf("per-user isolation needs the service to run as LocalSystem, not %s", o.ServiceUser)
	}
	// The installer's own parent (C:\ProgramData\Longarm) is locked down
	// too, so nobody can swap the data dir underneath it. (Per-user
	// isolation: longarmd locks both down itself, leaving the accounts
	// only traverse rights to reach their data.)
	if parent := filepath.Dir(dataDir); strings.EqualFold(filepath.Base(parent), "longarm") && !perUser {
		if err := platform.SecureDir(parent, o.ServiceUser); err != nil {
			return fmt.Errorf("secure %s: %w", parent, err)
		}
	}
	if err := platform.SecureDir(dataDir, o.ServiceUser); err != nil {
		return fmt.Errorf("secure %s: %w", dataDir, err)
	}
	if err := SetIsolation(dataDir, o.Isolation); err != nil {
		return err
	}
	o.Listen = slices.DeleteFunc(o.Listen, func(s string) bool { return strings.TrimSpace(s) == "" })
	if len(o.Listen) > 0 {
		if err := writeListen(dataDir, o.Listen); err != nil {
			return err
		}
	}
	// A marker of an earlier installation is stale.
	os.Remove(filepath.Join(dataDir, InstallEnrolledFile))
	if strings.EqualFold(o.ServiceUser, "LocalSystem") {
		o.ServiceUser = ""
	}
	if o.ServiceUser != "" {
		if err := platform.GrantServiceLogon(o.ServiceUser); err != nil {
			return err
		}
	} else if o.Admin {
		fmt.Fprintln(o.Out, "admin worker: the longarmd service runs as LocalSystem; what its owner runs on it runs as SYSTEM")
	} else if perUser {
		fmt.Fprintln(o.Out, "per-user isolation: longarmd runs as LocalSystem and runs every Longarm user's work as an account of its own (la-<user>)")
	} else {
		fmt.Fprintln(o.Out, "WARNING: the longarmd service runs as LocalSystem (SERVICE_ACCOUNT=LocalSystem): jobs run as SYSTEM "+
			"with the system profile, not as a user with git / npm / claude settings (DESIGN §3.1 recommends a user account)")
	}
	if o.Login && o.Code == "" && o.Hub != "" {
		if err := SetUpSignIn(dataDir, o.Hub, o.Fingerprint); err != nil {
			return err
		}
		fmt.Fprintln(o.Out, "sign-in to come: the service waits until the Longarm tray or `longarmd login --role both` signs this machine in to "+o.Hub)
		return nil
	}
	if o.Code == "" || o.Hub == "" {
		fmt.Fprintln(o.Out, "no join code: join later with `longarmd join <hub> <code> --data-dir "+dataDir+"`")
		return nil
	}
	if err := Join(ctx, dataDir, o.Hub, o.Code, JoinOptions{Name: o.Name, Fingerprint: o.Fingerprint, NoWait: true, Out: o.Out}); err != nil {
		return err
	}
	// This install enrolled the node: if the install fails later (e.g. the
	// service account can't log on), InstallRollback takes it back.
	return os.WriteFile(filepath.Join(dataDir, InstallEnrolledFile), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
}

// InstallEnrolledFile marks a data dir whose enrollment the running
// installation made (InstallSetup); InstallRollback undoes only that.
const InstallEnrolledFile = "install-enrolled"

// InstallRollback is the MSI's rollback of InstallSetup: when this
// installation enrolled the node (marker), it leaves the hub again and
// removes the credentials; an enrollment that existed before (an earlier
// install, KEEP_DATA) is never touched.
func InstallRollback(ctx context.Context, dataDir string, out io.Writer) error {
	marker := filepath.Join(dataDir, InstallEnrolledFile)
	if _, err := os.Stat(marker); err != nil {
		fmt.Fprintln(out, "rollback: this install enrolled nothing; data dir left as is")
		return nil
	}
	_, err := Leave(ctx, dataDir, LeaveOptions{Reason: "install rolled back", Force: true, Out: out})
	os.Remove(marker)
	return err
}

// writeListen creates longarmd.toml with listen URLs (an existing file is kept).
func writeListen(dataDir string, listen []string) error {
	p := filepath.Join(dataDir, ConfigFile)
	if _, err := os.Stat(p); err == nil {
		return nil
	}
	var b strings.Builder
	b.WriteString("# written by the installer\nlisten = [")
	for i, l := range listen {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q", l)
	}
	b.WriteString("]\n")
	return os.WriteFile(p, []byte(b.String()), 0o600)
}

// logoutTailnet logs the worker's tailscale nodes out of their tailnets
// (best effort, bounded): the node key is invalidated (ephemeral nodes
// disappear at once). The hub can't delete devices (its Tailscale OAuth
// client only mints keys): a worker device stays listed, logged out, until
// an admin removes it in the admin console.
func logoutTailnet(ctx context.Context, dataDir string, o LeaveOptions) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var done []string
	if o.logout != nil {
		done = o.logout(ctx)
	}
	hf, _ := hubclient.LoadFile(dataDir)
	for _, dir := range transport.StateDirs(filepath.Join(dataDir, TSNetDir)) {
		if slices.Contains(done, dir) {
			continue
		}
		control, host := transport.JoinedControl(dir), ""
		if control == "" && hf.Tailnet.ControlURL != "" && dir == hubclient.TailnetStateDir(dataDir, hf.Tailnet.ControlURL) {
			control = hf.Tailnet.ControlURL // marker from before it recorded the control server
		}
		if transport.SameControl(control, hf.Tailnet.ControlURL) {
			host = hf.Tailnet.Hostname
		}
		if err := transport.LogoutDir(ctx, dir, control, host); err != nil {
			fmt.Fprintf(o.Out, "tailscale node in %s not logged out: %v\n", filepath.Base(dir), err)
		} else {
			fmt.Fprintf(o.Out, "tailscale node in %s logged out\n", filepath.Base(dir))
		}
	}
}
