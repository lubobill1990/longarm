package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lubobill1990/longarm/internal/longarmd"
	"github.com/lubobill1990/longarm/internal/platform"
)

// installHelperCmd is run by the MSI's custom actions:
//
//	longarmd install-helper msi <step> #LONGARM-MSI#key=value|key=value|...
//
// The data after the marker is taken verbatim from the raw command line and
// parsed strictly (longarmd.MSIData): Windows Installer formats public
// properties into the command line unquoted, so no value may become an
// option. Nothing in the data names a program: binaries are always the
// ones next to this longarmd, whose directory must be writable by
// administrators only. Steps that act as SYSTEM for somebody check who
// asked (the MSI's UserSID): an administrator, or the user they act for.
func installHelperCmd() *cobra.Command {
	c := &cobra.Command{Use: "install-helper", Short: "Installer steps (internal; used by the MSI)", Hidden: true}
	c.AddCommand(&cobra.Command{
		Use:                "msi <step> " + longarmd.MSIDataMarker + "key=value|...",
		Short:              "An MSI custom action (structured data, see longarmd.MSIData)",
		DisableFlagParsing: true,
		Args:               cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return errors.New("which step?")
			}
			return runMSIStep(cmd.Context(), args[0], rawCommandLine(), msiEnv{})
		},
	})
	return c
}

// msiEnv is what the steps need from the system (tests replace parts).
type msiEnv struct {
	isAdmin     func(sid string) (bool, error)
	userSID     func(sid string) error
	ownDir      func() (string, error)
	programData func() string
	sched       longarmd.TaskScheduler
	out         io.Writer
}

func (e msiEnv) fill() msiEnv {
	if e.isAdmin == nil {
		e.isAdmin = platform.IsLocalAdmin
	}
	if e.userSID == nil {
		e.userSID = platform.UserSID
	}
	if e.ownDir == nil {
		e.ownDir = trustedOwnDir
	}
	if e.programData == nil {
		e.programData = func() string {
			pd := os.Getenv("ProgramData")
			if pd == "" {
				pd = `C:\ProgramData`
			}
			return filepath.Join(pd, "Longarm")
		}
	}
	if e.sched == nil {
		e.sched = longarmd.Schtasks{}
	}
	return e
}

// trustedOwnDir is the directory of this longarmd, which must be writable by
// administrators only (Program Files): it is what services and tasks run.
func trustedOwnDir() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if p, err := filepath.EvalSymlinks(self); err == nil {
		self = p
	}
	dir := filepath.Dir(self)
	if err := platform.TrustedSystemPath(dir); err != nil {
		return "", fmt.Errorf("the installed binaries are not protected (%w): install into Program Files", err)
	}
	for _, f := range []string{self, filepath.Join(dir, "longarm-tray.exe")} {
		if _, err := os.Stat(f); err != nil {
			continue
		}
		if err := platform.TrustedSystemPath(f); err != nil {
			return "", fmt.Errorf("the installed binaries are not protected (%w)", err)
		}
	}
	return dir, nil
}

// msiLog opens the helper's log in the Longarm dir of ProgramData, which it
// secures first (a user may have created it): never a path a user could
// point elsewhere.
func msiLog(base string) (io.Writer, func()) {
	if err := os.MkdirAll(base, 0o700); err == nil {
		if platform.TrustedSystemPath(base) != nil {
			platform.SecureDir(base, "")
		}
	}
	p := filepath.Join(base, "install-helper.log")
	old, _ := os.ReadFile(p)
	os.Remove(p) // a link planted there goes, not its target
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return os.Stdout, func() {}
	}
	if len(old) > 0 && len(old) < 1<<20 {
		f.Write(old)
	}
	return io.MultiWriter(os.Stdout, f), func() { f.Close() }
}

func runMSIStep(ctx context.Context, step, raw string, env msiEnv) error {
	env = env.fill()
	keys, ok := longarmd.MSISteps[step]
	if !ok {
		return fmt.Errorf("unknown step %q", step)
	}
	out := env.out
	if out == nil {
		var done func()
		if step == "session-join" {
			out, done = os.Stdout, func() {} // as the user: no SYSTEM log
		} else {
			out, done = msiLog(env.programData())
		}
		defer done()
	}
	err := msiStep(ctx, step, raw, keys, env, out)
	if err != nil {
		fmt.Fprintf(out, "%s install-helper %s: error: %v\n", time.Now().Format(time.RFC3339), step, err)
	} else {
		fmt.Fprintf(out, "%s install-helper %s: ok\n", time.Now().Format(time.RFC3339), step)
	}
	switch step {
	case "rollback", "remove", "session-remove", "admin-remove", "admin-drop":
		return nil // never block a rollback or an uninstall
	}
	return err
}

func msiStep(ctx context.Context, step, raw string, keys []string, env msiEnv, out io.Writer) error {
	d, err := longarmd.MSIData(raw, keys...)
	if err != nil {
		return err
	}
	req := longarmd.MSIRequester{SID: d["usersid"], IsAdmin: env.isAdmin}
	base := env.programData()
	userDataDir := func(sid string) (string, error) {
		profile, err := longarmd.ProfileDirOfSID(sid)
		if err != nil {
			return "", err
		}
		return longarmd.ProfileDataDir(profile), nil
	}
	checkUser := func(sid string) error {
		if err := longarmd.ValidUserSID(sid); err != nil {
			return err
		}
		return env.userSID(sid)
	}
	switch step {
	case "check":
		// A repair or change of the installed product: Windows lets a
		// non-administrator run it elevated (per-machine), with the
		// public properties they choose. Only administrators may.
		if d["installed"] != "" {
			return req.RequireAdmin("repairing or changing the installed Longarm")
		}
		return nil

	case "setup":
		if err := req.RequireAdmin("installing the worker service"); err != nil {
			return err
		}
		dir, err := longarmd.MSIDataDir(d["datadir"], base)
		if err != nil {
			return err
		}
		if _, err := env.ownDir(); err != nil {
			return err
		}
		sctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		return longarmd.InstallSetup(sctx, dir, longarmd.SetupOptions{ServiceUser: d["user"], Hub: d["hub"], Code: d["code"],
			Fingerprint: d["ca"], Listen: splitList(d["listen"]), Name: d["name"], Out: out, Isolation: d["isolation"], Login: d["login"] == "1"})

	case "rollback", "remove":
		dir, err := longarmd.MSIDataDir(d["datadir"], base)
		if err != nil {
			return err
		}
		lctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		if step == "rollback" {
			return longarmd.InstallRollback(lctx, dir, out)
		}
		_, err = longarmd.Leave(lctx, dir, longarmd.LeaveOptions{Reason: "uninstall", Force: true, KillJobs: true, Purge: d["keep"] != "1", Out: out})
		return err

	case "session":
		sid := d["sid"]
		if err := checkUser(sid); err != nil {
			return err
		}
		if err := req.ForUser(sid, "the user-session worker"); err != nil {
			return err
		}
		own, err := env.ownDir()
		if err != nil {
			return err
		}
		dir, err := userDataDir(sid)
		if err != nil {
			return err
		}
		host, args := sessionHost(own)
		return longarmd.InstallSessionTask(longarmd.SessionInstallOptions{UserSID: sid, HostExe: host, HostArgs: args,
			Start: true, DataDir: dir, Sched: env.sched, Out: out})

	case "session-join":
		// runs as the installing user (impersonated): the data dir below
		// this process's own profile
		profile, err := longarmd.TokenProfileDir()
		if err != nil {
			return err
		}
		if d["hub"] == "" || d["code"] == "" {
			return errors.New("a join code needs HUB and JOIN_CODE")
		}
		jctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		return longarmd.Join(jctx, longarmd.ProfileDataDir(profile), d["hub"], d["code"],
			longarmd.JoinOptions{Name: d["name"], Fingerprint: d["ca"], NoWait: true, Out: out})

	case "session-remove":
		sid := d["sid"]
		if sid == "" || longarmd.ValidUserSID(sid) != nil {
			return nil // nothing recorded
		}
		dir, derr := userDataDir(sid)
		if derr != nil {
			fmt.Fprintln(out, "warning:", derr)
		}
		if err := longarmd.RemoveSessionTask(env.sched, sid, dir, out); err != nil {
			fmt.Fprintln(out, "warning:", err)
		}
		if dir == "" || (d["leave"] != "1" && d["leave"] != "keep") {
			return nil
		}
		// the user's own dir, read as SYSTEM: a reparse point there is
		// never followed (Leave and RemoveAll remove links, not targets)
		if fi, err := os.Lstat(dir); err != nil || fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 || !fi.IsDir() {
			return fmt.Errorf("%s is not a plain directory: left alone", dir)
		}
		lctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		_, err := longarmd.Leave(lctx, dir, longarmd.LeaveOptions{Reason: "uninstall", Force: true, KillJobs: true, Purge: d["leave"] != "keep", Out: out})
		return err

	case "launch-tray":
		sid := d["sid"]
		if err := checkUser(sid); err != nil {
			fmt.Fprintln(out, "launch-tray:", err)
			return nil // nobody to show it to: never fail the install
		}
		if err := req.ForUser(sid, "starting the tray"); err != nil {
			return err
		}
		own, err := env.ownDir()
		if err != nil {
			return err
		}
		tray := filepath.Join(own, "longarm-tray.exe")
		if _, err := os.Stat(tray); err != nil {
			return nil
		}
		if err := longarmd.LaunchInSession(env.sched, sid, tray, []string{"--signin"}); err != nil {
			fmt.Fprintln(out, "warning: starting the tray:", err)
		}
		return nil

	case "admin-setup":
		if err := req.RequireAdmin("the admin worker (it runs as SYSTEM)"); err != nil {
			return err
		}
		sid := d["sid"]
		if err := checkUser(sid); err != nil {
			return fmt.Errorf("the admin worker is signed in by the user who installs it: %w", err)
		}
		// the hand-off dir goes to this user: it must be the administrator
		// who asked (or, for a deployment as SYSTEM, an administrator)
		if !strings.EqualFold(sid, req.SID) {
			if err := (longarmd.MSIRequester{SID: sid, IsAdmin: env.isAdmin}).RequireAdmin("signing the admin worker in"); err != nil {
				return err
			}
		}
		if d["hub"] == "" {
			return errors.New("the admin worker signs in with Microsoft: it needs a hub (HUB, or the built-in default)")
		}
		own, err := env.ownDir()
		if err != nil {
			return err
		}
		return adminSetup(ctx, out, adminSetupOptions{base: base, sid: sid, exe: filepath.Join(own, "longarmd.exe"), hub: d["hub"], fp: d["ca"]})

	case "admin-remove":
		return adminRemove(ctx, out, base, d["keep"] == "1")

	case "admin-drop":
		if err := req.RequireAdmin("removing the admin worker"); err != nil {
			return err
		}
		return adminRemove(ctx, out, base, false)
	}
	return fmt.Errorf("unknown step %q", step)
}

func splitList(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// sessionHost is what the user-session task runs from the installed dir:
// the windowless tray as the host, else longarmd session-mode run.
func sessionHost(dir string) (string, []string) {
	if tray := filepath.Join(dir, "longarm-tray.exe"); fileExists(tray) {
		return tray, []string{"--worker-host"}
	}
	return filepath.Join(dir, "longarmd.exe"), []string{"session-mode", "run"}
}

type adminSetupOptions struct {
	base, sid, exe, hub, fp string
}

func adminSetup(ctx context.Context, w io.Writer, o adminSetupOptions) error {
	dataDir, handoff := longarmd.AdminDataDirIn(o.base), longarmd.AdminHandoffDirIn(o.base)
	// a new version: stop the old service first (its enrollment stays)
	if st, err := platform.QueryService(longarmd.AdminServiceName); err == nil && st != platform.ServiceNotInstalled {
		platform.StopService(longarmd.AdminServiceName)
		if err := platform.UninstallService(longarmd.AdminServiceName); err != nil {
			return fmt.Errorf("replace service %s: %w", longarmd.AdminServiceName, err)
		}
	}
	// the hand-off dir starts empty every time (whatever a user left there)
	if err := longarmd.FreshSystemDir(handoff, true, w); err != nil {
		return err
	}
	sctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	// InstallSetup keeps the data dir only if this installer made it
	if err := longarmd.InstallSetup(sctx, dataDir, longarmd.SetupOptions{ServiceUser: "LocalSystem", Hub: o.hub, Fingerprint: o.fp,
		Listen: []string{longarmd.AdminListen}, Login: true, Admin: true, Out: w}); err != nil {
		return err
	}
	// the hand-off dir: SYSTEM, Administrators and the installing administrator
	if err := platform.SecureDir(handoff, o.sid); err != nil {
		return fmt.Errorf("secure %s: %w", handoff, err)
	}
	if err := longarmd.WriteAdminMarker(dataDir, handoff); err != nil {
		return err
	}
	if err := longarmd.InstallService(longarmd.ServiceInstallOptions{Name: longarmd.AdminServiceName, Exe: o.exe, DataDir: dataDir,
		DataDirExplicit: true, Out: w, Warn: io.Discard}); err != nil {
		return err
	}
	if err := platform.StartService(longarmd.AdminServiceName); err != nil {
		return fmt.Errorf("start %s: %w", longarmd.AdminServiceName, err)
	}
	fmt.Fprintf(w, "admin worker: service %s started; it waits for the sign-in (the tray)\n", longarmd.AdminServiceName)
	return nil
}

func adminRemove(ctx context.Context, w io.Writer, base string, keep bool) error {
	dataDir, handoff := longarmd.AdminDataDirIn(base), longarmd.AdminHandoffDirIn(base)
	var errs []error
	if st, err := platform.QueryService(longarmd.AdminServiceName); err == nil && st != platform.ServiceNotInstalled {
		platform.StopService(longarmd.AdminServiceName)
	}
	if fi, err := os.Lstat(dataDir); err == nil && fi.IsDir() && fi.Mode()&(os.ModeSymlink|os.ModeIrregular) == 0 {
		lctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		if _, err := longarmd.Leave(lctx, dataDir, longarmd.LeaveOptions{Reason: "admin worker removed", Force: true, KillJobs: true, Purge: !keep, Out: w}); err != nil {
			errs = append(errs, err)
		}
	}
	if st, err := platform.QueryService(longarmd.AdminServiceName); err == nil && st != platform.ServiceNotInstalled {
		if err := platform.UninstallService(longarmd.AdminServiceName); err != nil {
			errs = append(errs, err)
		} else {
			fmt.Fprintf(w, "removed service %s\n", longarmd.AdminServiceName)
		}
	}
	longarmd.ForgetAdminWorker()
	os.RemoveAll(handoff)
	return errors.Join(errs...)
}

// adminWorkerCmd: the admin worker for its owner (Windows).
func adminWorkerCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "admin-worker",
		Short: "The optional admin worker (Windows): a LocalSystem service only you can use, never placed automatically",
		Long: `The admin worker (Windows; the MSI's "Allow remote administrator commands",
ADMIN_WORKER=1, off by default) is a second longarmd, a service running as
LocalSystem: it runs while nobody is logged on, and what you run on it runs
as SYSTEM (installing software, administration). The hub enrolls it with your
sign-in as <worker>-admin, marked longarm.io/admin=true: only you can use it,
it can't be shared, admins can't use it, and it is never chosen
automatically - only by its name (longarm exec <worker>-admin ...). It
ignores everything the hub starts on its own (toolchains, updates, port
links, relayed job requests): update it with the MSI.`,
	}
	var asJSON bool
	status := &cobra.Command{
		Use: "status", Short: "Show the admin worker's state", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			_, a := longarmd.InstalledWorker()
			st := longarmd.AdminWorkerStatus(a, queryService)
			if asJSON {
				b, _ := json.MarshalIndent(st, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			if !st.Enabled {
				fmt.Println("admin worker: not enabled (the MSI's ADMIN_WORKER=1 adds it)")
				return nil
			}
			fmt.Printf("admin worker: service %s %s, signed in %v, waiting for its sign-in %v\n", st.Service, st.State, st.SignedIn, st.Pending)
			if st.Error != "" {
				fmt.Println("last sign-in error:", st.Error)
			}
			return nil
		},
	}
	status.Flags().BoolVar(&asJSON, "json", false, "JSON")
	var keep bool
	disable := &cobra.Command{
		Use: "disable", Short: "Remove the admin worker: revoke it at the hub, remove its service and data (elevated)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, a := longarmd.InstalledWorker()
			if !a.Enabled && a.DataDir == "" {
				return errors.New("no admin worker recorded by the installer")
			}
			if !platform.Elevated() {
				return errors.New("run it elevated (as administrator): the admin worker is a LocalSystem service")
			}
			if err := adminRemove(cmd.Context(), os.Stdout, msiEnv{}.fill().programData(), keep); err != nil {
				return err
			}
			if err := longarmd.RecordAdminDisabled(); err != nil {
				fmt.Fprintln(os.Stderr, "warning: recording it in the registry:", err)
			}
			fmt.Println("admin worker disabled (reinstall the MSI with ADMIN_WORKER=1 to add it again)")
			return nil
		},
	}
	disable.Flags().BoolVar(&keep, "keep-data", false, "keep its jobs (credentials are removed)")
	c.AddCommand(status, disable)
	return c
}

func queryService(name string) (string, error) {
	st, err := platform.QueryService(name)
	if err != nil {
		return "", err
	}
	return fmt.Sprint(st), nil
}
