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
	// Elevated: this process holds an administrator's elevated token (an
	// immediate step: the installer runs as SYSTEM - the update service -
	// or from an elevated prompt; not an uninstall from Settings, whose
	// immediate steps get the user's filtered token).
	Elevated func() bool
	IsAdmin  func(sid string) (bool, error)
	// ServiceNames: the worker services an upgrade stops and starts again
	// (longarmd-admin, LongarmUpdate, the RUN=service longarmd).
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
	case "stop-early", "stop-early-remove":
		// immediate, before InstallValidate looks for files in use (with
		// Restart Manager off altogether its own check took ~40 s for every
		// process that holds a file it replaces or removes - minutes for an
		// upgrade or uninstall with Longarm running; Restart Manager's
		// detection is fast). With an administrator's token (the update
		// service's install as SYSTEM, an elevated msiexec), everything is
		// stopped and moved aside here, as the deferred stop would, and
		// recorded; the deferred stop then adds what it still finds.
		me, err := env.CurrentSID()
		if err != nil {
			return err
		}
		if env.Elevated != nil && env.Elevated() {
			return stopAll(env, dir, true)
		}
		// otherwise as the installing user: their own processes only, and
		// nothing recorded - the tray (the installer starts it again) and,
		// for an uninstall, their worker too
		if !UserSID(me) || !strings.EqualFold(me, asker) {
			fmt.Fprintf(env.Log, "%s: not the installing user (%s) nor elevated: nothing to do\n", step, me)
			return nil
		}
		kinds := []Kind{KindTray}
		if step == "stop-early-remove" {
			kinds = nil
		}
		_, err = StopProcesses(env.System, StopOptions{Dir: dir, SID: me, Kinds: kinds, Timeout: timeoutOr(env.Timeout, 10*time.Second), Log: env.Log})
		return err

	case "stop":
		if !admin {
			// a non-administrator's repair (refused later by the
			// installer): never anybody else's processes
			if !UserSID(asker) {
				return nil
			}
			return stopOwn(env, dir, asker)
		}
		return stopAll(env, dir, false)

	case "stop-remove":
		if !admin {
			if !UserSID(asker) {
				return nil
			}
			_, err := StopProcesses(env.System, StopOptions{Dir: dir, SID: asker, Timeout: timeoutOr(env.Timeout, 20*time.Second), Log: env.Log})
			return err
		}
		_, err := StopProcesses(env.System, StopOptions{Dir: dir, Timeout: timeoutOr(env.Timeout, 20*time.Second), Log: env.Log})
		// what stop-early moved aside is still running (a `longarm`
		// client, a job): deleted at the next start, as Windows Installer
		// does with files in use; the record is no longer needed
		if state, serr := env.StateDir(); serr == nil {
			RemoveRecord(state)
		}
		env.Files.RemoveAside(dir, env.Log)
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

// stopAll (an administrator): the services, every user's processes, the
// binaries still in use moved aside - recorded for the restart or the
// rollback. early (stop-early) writes a new record; the deferred stop adds
// to the one stop-early wrote for the same install, and leaves the aside
// dir as it is then (what stop-early moved there is in that record).
func stopAll(env Env, dir string, early bool) error {
	state, err := env.StateDir()
	if err != nil {
		return err
	}
	now := env.Now()
	rec := &Record{Created: now.UTC(), Early: early}
	prior := false
	if !early {
		if r, _ := LoadRecord(state, now); r != nil && r.Early {
			rec, prior = r, true
			fmt.Fprintln(env.Log, "stop-early stopped Longarm already: adding to its record")
		}
	}
	services := StopServices(env.Services, env.ServiceNames(), env.Log)
	st, err := StopProcesses(env.System, StopOptions{Dir: dir, Timeout: timeoutOr(env.Timeout, 20*time.Second), Log: env.Log})
	if !prior {
		CleanAside(dir, env.Log)
	}
	as, aerr := env.Files.MoveAside(dir, now, env.Log)
	if aerr != nil {
		fmt.Fprintln(env.Log, "warning:", aerr)
	}
	rec.Add(services, st.Workers, st.Trays, as)
	if serr := rec.Save(state); serr != nil {
		return fmt.Errorf("record: %w", serr)
	}
	return err
}

// stopOwn: a non-administrator's repair (refused later by the installer):
// their own processes, recorded; no service, nothing moved aside.
func stopOwn(env Env, dir, sid string) error {
	state, err := env.StateDir()
	if err != nil {
		return err
	}
	st, err := StopProcesses(env.System, StopOptions{Dir: dir, SID: sid, Timeout: timeoutOr(env.Timeout, 20*time.Second), Log: env.Log})
	rec := &Record{Created: env.Now().UTC(), Trays: st.Trays, Workers: st.Workers}
	if serr := rec.Save(state); serr != nil {
		return fmt.Errorf("record: %w", serr)
	}
	return err
}

func timeoutOr(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}
