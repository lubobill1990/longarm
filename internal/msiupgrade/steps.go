package msiupgrade

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// Env is what the helper's steps need (WindowsEnv; tests fake it).
type Env struct {
	// InstallDir: the install dir recorded by the MSI (HKLM\Software\
	// Longarm InstallDir), checked to be writable by administrators only.
	InstallDir func() (string, error)
	// CurrentSID: this process's user.
	CurrentSID func() (string, error)
	IsAdmin    func(sid string) (bool, error)
	// ServiceNames: the worker services an upgrade stops and starts again
	// (longarmd-admin, the RUN=service longarmd).
	ServiceNames func() []string
	// StateDir: ProgramData\Longarm, secured (SYSTEM and Administrators).
	StateDir func() (string, error)
	System   System
	Services Services
	Tasks    Tasks
	Files    Files
	Timeout  time.Duration
	Now      func() time.Time
	Log      io.Writer
}

// RunStep runs a step of longarm-msihelper: raw is its command line (the
// data after DataMarker). The steps never fail the install: they log, and
// what they could not do the installer still does (with a reboot at worst).
func RunStep(step, raw string, env Env) error {
	keys, ok := Steps[step]
	if !ok {
		return fmt.Errorf("unknown step %q", step)
	}
	if env.Log == nil {
		env.Log = io.Discard
	}
	if env.Now == nil {
		env.Now = time.Now
	}
	d, err := ParseData(raw, keys...)
	if err != nil {
		return err
	}
	dir, err := env.InstallDir()
	if err != nil {
		return fmt.Errorf("install dir: %w", err)
	}
	asker := d["usersid"]
	admin := asker == "S-1-5-18"
	if !admin && asker != "" {
		admin, _ = env.IsAdmin(asker)
	}
	switch step {
	case "stop-tray":
		// as the installing user, before the installer looks for files in
		// use: their own trays only (the installer starts the tray again)
		me, err := env.CurrentSID()
		if err != nil {
			return err
		}
		if !UserSID(me) || !strings.EqualFold(me, asker) {
			fmt.Fprintf(env.Log, "stop-tray: not the installing user (%s): nothing to do\n", me)
			return nil
		}
		_, err = StopProcesses(env.System, StopOptions{Dir: dir, SID: me, Kinds: []Kind{KindTray}, Timeout: timeoutOr(env.Timeout, 10*time.Second), Log: env.Log})
		return err

	case "stop", "stop-remove":
		state, err := env.StateDir()
		if err != nil {
			return err
		}
		o := StopOptions{Dir: dir, Timeout: timeoutOr(env.Timeout, 20*time.Second), Log: env.Log}
		rec := &Record{Created: env.Now().UTC()}
		if !admin {
			// a non-administrator's repair (refused later by the
			// installer): never anybody else's processes
			if !UserSID(asker) {
				return nil
			}
			o.SID = asker
		} else if step == "stop" {
			rec.Services = StopServices(env.Services, env.ServiceNames(), env.Log)
		}
		st, err := StopProcesses(env.System, o)
		rec.Trays, rec.Workers = st.Trays, st.Workers
		if step == "stop-remove" {
			return err
		}
		if admin {
			CleanAside(dir, env.Log)
			as, aerr := env.Files.MoveAside(dir, env.Now(), env.Log)
			rec.Aside = as
			if aerr != nil {
				fmt.Fprintln(env.Log, "warning:", aerr)
			}
		}
		if serr := rec.Save(state); serr != nil {
			return fmt.Errorf("record: %w", serr)
		}
		return err

	case "rollback":
		// the upgrade failed: the old binaries back, then what ran
		state, err := env.StateDir()
		if err != nil {
			return err
		}
		rec, err := LoadRecord(state, env.Now())
		if err != nil || rec == nil {
			return err
		}
		defer RemoveRecord(state)
		var rerr error
		if admin {
			rerr = RestoreAside(dir, rec.Aside, env.Log)
		}
		o := RestartOptions{Services: env.Services, Tasks: env.Tasks, Log: env.Log}
		if !admin {
			o.OnlySID = asker
		}
		if err := Restart(rec, o); err != nil {
			return err
		}
		return rerr
	}
	return fmt.Errorf("unknown step %q", step)
}

func timeoutOr(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}
