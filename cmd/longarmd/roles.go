package main

import (
	"context"
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
	"github.com/lubobill1990/longarm/internal/signin"
)

// rolesHelperCmd: `longarmd install-helper roles #LONGARM-MSI#client=1|worker=1|admin=|sid=S-...`,
// run elevated (a UAC prompt) by the tray's Account tab to change this
// computer's roles without reinstalling (internal/longarmd/roles.go). Its
// data is parsed as strictly as the MSI's custom actions' (MSIData); who
// asks is this process's own token, which must be an elevated
// administrator; the user-session worker is the user SID's (the tray's
// user), the admin worker's sign-in that user's too, under the same rules
// as the MSI (an administrator; binaries only from this longarmd's
// administrator-only directory; hub and CA pin from HKLM, never from the
// command line).
func rolesHelperCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "roles " + longarmd.MSIDataMarker + "client=1|worker=1|admin=|sid=S-...",
		Short:              "Change this computer's roles (the tray's Account tab, elevated)",
		DisableFlagParsing: true,
		Args:               cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env := msiEnv{}.fill()
			out, done := msiLog(env.programData())
			defer done()
			warn := &warnings{w: out}
			err := runRolesStep(cmd.Context(), rawCommandLine(), env, warn)
			msg := "ok"
			if len(warn.lines) > 0 {
				msg += "; " + strings.Join(warn.lines, "; ")
			}
			if err != nil {
				msg = "error: " + flatErr(err)
				fmt.Fprintf(out, "%s install-helper roles: %s\n", time.Now().Format(time.RFC3339), msg)
			} else {
				fmt.Fprintf(out, "%s install-helper roles: ok\n", time.Now().Format(time.RFC3339))
			}
			env.settings.Set(longarmd.RegRolesResult, msg+" ("+time.Now().UTC().Format(time.RFC3339)+")")
			return err
		},
	}
}

// warnings passes output on and keeps its "warning:" lines (for the
// result the tray shows).
type warnings struct {
	w     io.Writer
	lines []string
}

func (w *warnings) Write(b []byte) (int, error) {
	for _, l := range strings.Split(string(b), "\n") {
		if i := strings.Index(l, "warning:"); i >= 0 && len(w.lines) < 5 {
			w.lines = append(w.lines, strings.TrimSpace(l[i+len("warning:"):]))
		}
	}
	return w.w.Write(b)
}

func flatErr(err error) string {
	return strings.Join(strings.Fields(err.Error()), " ")
}

// roleOps are the system changes of a roles change (tests: fakes).
type roleOps struct {
	elevated    func() bool
	selfSID     func() (string, error)
	adminSetup  func(ctx context.Context, w io.Writer, o adminSetupOptions) error
	adminRemove func(ctx context.Context, w io.Writer, base string, keep bool) error
	leave       func(ctx context.Context, dataDir string, w io.Writer) error
	fallbackHub func() (hub, fp, from string)
	// userDir: a user's longarmd data dir (~\.longarm of the profile)
	userDir func(sid string) (string, error)
}

func (o roleOps) fill() roleOps {
	if o.elevated == nil {
		o.elevated = platform.Elevated
	}
	if o.selfSID == nil {
		o.selfSID = longarmd.CurrentUserSID
	}
	if o.adminSetup == nil {
		o.adminSetup = adminSetup
	}
	if o.adminRemove == nil {
		o.adminRemove = adminRemove
	}
	if o.leave == nil {
		o.leave = func(ctx context.Context, dir string, w io.Writer) error {
			// the user's own dir, read as an administrator: a reparse point
			// there is never followed (as the MSI's session-remove)
			if fi, err := os.Lstat(dir); err != nil || fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 || !fi.IsDir() {
				return nil // never enrolled here
			}
			lctx, cancel := context.WithTimeout(ctx, time.Minute)
			defer cancel()
			_, err := longarmd.Leave(lctx, dir, longarmd.LeaveOptions{Reason: "worker role removed", Force: true, KillJobs: true, Out: w})
			return err
		}
	}
	if o.fallbackHub == nil {
		o.fallbackHub = signin.FallbackHub
	}
	if o.userDir == nil {
		o.userDir = func(sid string) (string, error) {
			profile, err := longarmd.ProfileDirOfSID(sid)
			if err != nil {
				return "", err
			}
			return longarmd.ProfileDataDir(profile), nil
		}
	}
	return o
}

var testRoleOps roleOps // tests

func runRolesStep(ctx context.Context, raw string, env msiEnv, out io.Writer) error {
	env = env.fill()
	ops := testRoleOps.fill()
	d, err := longarmd.MSIData(raw, longarmd.RolesKeys...)
	if err != nil {
		return err
	}
	if !ops.elevated() {
		return errors.New("changing this computer's roles needs an administrator: run it elevated")
	}
	me, err := ops.selfSID()
	if err != nil {
		return err
	}
	if err := (longarmd.MSIRequester{SID: me, IsAdmin: env.isAdmin}).RequireAdmin("changing this computer's Longarm roles"); err != nil {
		return err
	}
	want := longarmd.RoleChange{Client: d["client"] == "1", Worker: d["worker"] == "1", Admin: d["admin"] == "1", SID: d["sid"]}
	if err := longarmd.ValidUserSID(want.SID); err != nil {
		return err
	}
	if err := env.userSID(want.SID); err != nil {
		return err
	}
	cur := longarmd.LoadMachineRoles(env.settings)
	plan, err := longarmd.PlanRoleChange(cur, want)
	if err != nil {
		return err
	}
	if plan.Empty(cur) {
		fmt.Fprintln(out, "roles: nothing to change")
		return nil
	}
	if plan.AddAdmin && !strings.EqualFold(want.SID, me) {
		// the hand-off dir goes to that user (as the MSI's admin-setup)
		if err := (longarmd.MSIRequester{SID: want.SID, IsAdmin: env.isAdmin}).RequireAdmin("signing the admin worker in"); err != nil {
			return err
		}
	}
	base := env.programData()
	var own string
	if plan.AddWorker || plan.AddAdmin {
		if own, err = env.ownDir(); err != nil {
			return err
		}
	}
	record := func(name, v string) error {
		if err := env.settings.Set(name, v); err != nil {
			return fmt.Errorf("record %s: %w", name, err)
		}
		return nil
	}
	userDir := ops.userDir

	if plan.RemoveAdmin {
		if err := ops.adminRemove(ctx, out, base, false); err != nil {
			fmt.Fprintln(out, "warning: removing the admin worker:", err)
		}
		if err := record(longarmd.RegAdminWorker, "0"); err != nil {
			return err
		}
	}
	if plan.RemoveWorker {
		dir, derr := userDir(want.SID)
		if derr != nil {
			fmt.Fprintln(out, "warning:", derr)
		}
		if err := longarmd.RemoveSessionTask(env.sched, want.SID, dir, out); err != nil {
			fmt.Fprintln(out, "warning:", err)
		}
		if dir != "" {
			// a hub that can't be reached: left locally, the hub keeps the
			// node until it is revoked there (Leave says how)
			if err := ops.leave(ctx, dir, out); err != nil {
				fmt.Fprintln(out, "warning: leaving the hub:", err)
			}
		}
		if err := record(longarmd.RegRoles, plan.Roles); err != nil {
			return err
		}
	}
	if plan.AddWorker {
		dir, err := userDir(plan.SessionSID)
		if err != nil {
			return err
		}
		host, args := sessionHost(own)
		if err := longarmd.InstallSessionTask(longarmd.SessionInstallOptions{UserSID: plan.SessionSID, HostExe: host, HostArgs: args,
			Start: true, DataDir: dir, Sched: env.sched, Out: out}); err != nil {
			return err
		}
		if old := plan.DropTrayTaskOf; old != "" && env.sched.Exists(longarmd.TrayTaskName(old)) {
			env.sched.Delete(longarmd.TrayTaskName(old))
		}
		if err := record(longarmd.RegSessionUserSID, plan.SessionSID); err != nil {
			return err
		}
		if cur.RunMode == "" {
			if err := record(longarmd.RegRunMode, "session"); err != nil {
				return err
			}
		}
	}
	// the roles before the admin worker: an upgrade must see the worker
	// the admin worker comes with
	if err := record(longarmd.RegRoles, plan.Roles); err != nil {
		return err
	}
	if plan.AddAdmin {
		// the hub the installer recorded (HKLM: administrators only), else
		// this build's: never one from the command line
		hub, fp, _ := ops.fallbackHub()
		if hub == "" {
			return errors.New("the admin worker signs in with Microsoft at a hub, and none is recorded on this computer")
		}
		if err := ops.adminSetup(ctx, out, adminSetupOptions{base: base, sid: want.SID, exe: filepath.Join(own, "longarmd.exe"), hub: hub, fp: fp}); err != nil {
			return err
		}
		if err := record(longarmd.RegAdminWorker, "1"); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "roles: %s%s\n", plan.Roles, map[bool]string{true: " + admin worker", false: ""}[want.Admin])
	return nil
}
