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

// helperLog appends an install-helper's output to logFile (and stdout).
func helperLog(logFile string) (io.Writer, func()) {
	if logFile == "" {
		return os.Stdout, func() {}
	}
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return os.Stdout, func() {}
	}
	return io.MultiWriter(os.Stdout, f), func() { f.Close() }
}

// hostCommand is what the user-session task runs for host (a path):
// longarm-tray.exe --worker-host, or longarmd.exe session-mode run.
func hostCommand(host string) (string, []string) {
	if strings.HasPrefix(strings.ToLower(filepath.Base(host)), "longarmd") {
		return host, []string{"session-mode", "run"}
	}
	return host, []string{"--worker-host"}
}

// sessionHelperCmds are the MSI's custom actions (as SYSTEM) of the
// user-session worker, the tray's first start and the admin worker.
func sessionHelperCmds() []*cobra.Command {
	var sid, host, logFile, exe string
	var keep bool
	userDataDir := func() (string, error) {
		profile, err := longarmd.ProfileDirOfSID(sid)
		if err != nil {
			return "", err
		}
		return longarmd.ProfileDataDir(profile), nil
	}
	session := &cobra.Command{
		Use: "session", Args: cobra.NoArgs,
		Short: "Register and (re)start the user-session worker's task for --user-sid",
		RunE: func(*cobra.Command, []string) error {
			w, done := helperLog(logFile)
			defer done()
			fmt.Fprintf(w, "%s longarmd install-helper session --user-sid %s\n", time.Now().Format(time.RFC3339), sid)
			err := func() error {
				if err := longarmd.ValidUserSID(sid); err != nil {
					return err
				}
				dir, err := userDataDir()
				if err != nil {
					return err
				}
				fmt.Fprintf(w, "user-session worker: data dir %s\n", dir)
				hexe, hargs := hostCommand(host)
				return longarmd.InstallSessionTask(longarmd.SessionInstallOptions{UserSID: sid, HostExe: hexe, HostArgs: hargs,
					Start: true, DataDir: dir, Sched: longarmd.Schtasks{}, Out: w})
			}()
			if err != nil {
				fmt.Fprintln(w, "error:", err)
			}
			return err
		},
	}
	session.Flags().StringVar(&sid, "user-sid", "", "the user (SID) whose logon starts the worker")
	session.Flags().StringVar(&host, "host", "", "the host the task runs (longarm-tray.exe, else longarmd.exe)")
	session.Flags().StringVar(&logFile, "log", "", "append output here")

	remove := &cobra.Command{
		Use: "session-remove", Args: cobra.NoArgs,
		Short: "Remove the user-session worker of --user-sid: its tasks, its processes, its enrollment (uninstall)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			w, done := helperLog(logFile)
			defer done()
			fmt.Fprintf(w, "%s longarmd install-helper session-remove --user-sid %s\n", time.Now().Format(time.RFC3339), sid)
			dir, derr := userDataDir()
			if derr != nil {
				fmt.Fprintln(w, "warning:", derr)
			}
			if err := longarmd.RemoveSessionTask(longarmd.Schtasks{}, sid, dir, w); err != nil {
				fmt.Fprintln(w, "warning:", err) // never block an uninstall
			}
			if dir != "" {
				ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
				defer cancel()
				if _, err := longarmd.Leave(ctx, dir, longarmd.LeaveOptions{Reason: "uninstall", Force: true, KillJobs: true, Purge: !keep, Out: w}); err != nil {
					fmt.Fprintln(w, "warning:", err)
				}
			}
			return nil
		},
	}
	remove.Flags().StringVar(&sid, "user-sid", "", "the user (SID)")
	remove.Flags().BoolVar(&keep, "keep-data", false, "keep jobs and mirrors (credentials are removed)")
	remove.Flags().StringVar(&logFile, "log", "", "append output here")

	tray := &cobra.Command{
		Use: "launch-tray", Args: cobra.NoArgs,
		Short: "Start longarm-tray in --user-sid's session (the MSI runs elevated: not as the user)",
		RunE: func(*cobra.Command, []string) error {
			w, done := helperLog(logFile)
			defer done()
			if err := longarmd.ValidUserSID(sid); err != nil {
				fmt.Fprintln(w, "launch-tray:", err)
				return nil // nobody to show it to: never fail the install
			}
			if err := longarmd.LaunchInSession(longarmd.Schtasks{}, sid, exe, []string{"--signin"}); err != nil {
				fmt.Fprintln(w, "warning: starting the tray:", err)
			}
			return nil
		},
	}
	tray.Flags().StringVar(&sid, "user-sid", "", "the user (SID)")
	tray.Flags().StringVar(&exe, "exe", "", "longarm-tray.exe")
	tray.Flags().StringVar(&logFile, "log", "", "append output here")

	var dataDir, handoff, svcName, hub, fp string
	adminSetup := &cobra.Command{
		Use: "admin-setup", Args: cobra.NoArgs,
		Short: "Install the admin worker: a LocalSystem longarmd service waiting for its sign-in",
		RunE: func(cmd *cobra.Command, _ []string) error {
			w, done := helperLog(logFile)
			defer done()
			fmt.Fprintf(w, "%s longarmd install-helper admin-setup --data-dir %s\n", time.Now().Format(time.RFC3339), dataDir)
			err := adminSetup(cmd.Context(), w, adminSetupOptions{dataDir: dataDir, handoff: handoff, sid: sid, service: svcName, exe: exe, hub: hub, fp: fp})
			if err != nil {
				fmt.Fprintln(w, "error:", err)
			}
			return err
		},
	}
	adminSetup.Flags().StringVar(&dataDir, "data-dir", "", "the admin worker's data dir (required)")
	adminSetup.Flags().StringVar(&handoff, "handoff", "", "the sign-in hand-off dir (required)")
	adminSetup.Flags().StringVar(&sid, "user-sid", "", "the installing user (SID), who signs it in")
	adminSetup.Flags().StringVar(&svcName, "service-name", longarmd.AdminServiceName, "service name")
	adminSetup.Flags().StringVar(&exe, "exe", "", "longarmd.exe the service runs (default: this one)")
	adminSetup.Flags().StringVar(&hub, "hub", "", "hub address")
	adminSetup.Flags().StringVar(&fp, "ca-fingerprint", "", "hub CA fingerprint")
	adminSetup.Flags().StringVar(&logFile, "log", "", "append output here")
	adminSetup.MarkFlagRequired("data-dir")
	adminSetup.MarkFlagRequired("handoff")

	adminRemove := &cobra.Command{
		Use: "admin-remove", Args: cobra.NoArgs,
		Short: "Remove the admin worker: leave the hub, remove the service and its data",
		RunE: func(cmd *cobra.Command, _ []string) error {
			w, done := helperLog(logFile)
			defer done()
			if err := adminRemove(cmd.Context(), w, dataDir, handoff, svcName, keep); err != nil {
				fmt.Fprintln(w, "warning:", err) // never block an uninstall
			}
			return nil
		},
	}
	adminRemove.Flags().StringVar(&dataDir, "data-dir", "", "the admin worker's data dir (required)")
	adminRemove.Flags().StringVar(&handoff, "handoff", "", "the sign-in hand-off dir")
	adminRemove.Flags().StringVar(&svcName, "service-name", longarmd.AdminServiceName, "service name")
	adminRemove.Flags().BoolVar(&keep, "keep-data", false, "keep jobs (credentials are removed)")
	adminRemove.Flags().StringVar(&logFile, "log", "", "append output here")
	adminRemove.MarkFlagRequired("data-dir")
	return []*cobra.Command{session, remove, tray, adminSetup, adminRemove}
}

type adminSetupOptions struct {
	dataDir, handoff, sid, service, exe, hub, fp string
}

func adminSetup(ctx context.Context, w io.Writer, o adminSetupOptions) error {
	if o.hub == "" {
		return errors.New("the admin worker signs in with Microsoft: it needs a hub (HUB, or the built-in default)")
	}
	if err := longarmd.ValidUserSID(o.sid); err != nil {
		return fmt.Errorf("the admin worker is signed in by the user who installs it: %w", err)
	}
	if o.service == "" {
		o.service = longarmd.AdminServiceName
	}
	// a new version: stop the old service first (its enrollment stays)
	if st, err := platform.QueryService(o.service); err == nil && st != platform.ServiceNotInstalled {
		platform.StopService(o.service)
		if err := platform.UninstallService(o.service); err != nil {
			return fmt.Errorf("replace service %s: %w", o.service, err)
		}
	}
	sctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := longarmd.InstallSetup(sctx, o.dataDir, longarmd.SetupOptions{ServiceUser: "LocalSystem", Hub: o.hub, Fingerprint: o.fp,
		Listen: []string{longarmd.AdminListen}, Login: true, Admin: true, Out: w}); err != nil {
		return err
	}
	// the hand-off dir: SYSTEM, Administrators and the installing user
	if err := platform.SecureDir(o.handoff, o.sid); err != nil {
		return fmt.Errorf("secure %s: %w", o.handoff, err)
	}
	if err := longarmd.WriteAdminMarker(o.dataDir, o.handoff); err != nil {
		return err
	}
	if err := longarmd.InstallService(longarmd.ServiceInstallOptions{Name: o.service, Exe: o.exe, DataDir: o.dataDir,
		DataDirExplicit: true, Out: w, Warn: io.Discard}); err != nil {
		return err
	}
	if err := platform.StartService(o.service); err != nil {
		return fmt.Errorf("start %s: %w", o.service, err)
	}
	fmt.Fprintf(w, "admin worker: service %s started; it waits for the sign-in (the tray)\n", o.service)
	return nil
}

func adminRemove(ctx context.Context, w io.Writer, dataDir, handoff, service string, keep bool) error {
	if service == "" {
		service = longarmd.AdminServiceName
	}
	var errs []error
	if st, err := platform.QueryService(service); err == nil && st != platform.ServiceNotInstalled {
		platform.StopService(service)
	}
	if dataDir != "" {
		lctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		if _, err := longarmd.Leave(lctx, dataDir, longarmd.LeaveOptions{Reason: "admin worker removed", Force: true, KillJobs: true, Purge: !keep, Out: w}); err != nil {
			errs = append(errs, err)
		}
	}
	if st, err := platform.QueryService(service); err == nil && st != platform.ServiceNotInstalled {
		if err := platform.UninstallService(service); err != nil {
			errs = append(errs, err)
		} else {
			fmt.Fprintf(w, "removed service %s\n", service)
		}
	}
	if handoff != "" {
		os.RemoveAll(handoff)
	}
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
automatically - only by its name (longarm exec <worker>-admin ...).`,
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
			if a.DataDir == "" {
				return errors.New("no admin worker recorded by the installer")
			}
			if !platform.Elevated() {
				return errors.New("run it elevated (as administrator): the admin worker is a LocalSystem service")
			}
			if err := adminRemove(cmd.Context(), os.Stdout, a.DataDir, a.Handoff, a.Service, keep); err != nil {
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
