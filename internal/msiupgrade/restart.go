package msiupgrade

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

// Services is the service manager as the upgrade needs it.
type Services interface {
	// Running: false for a service that is stopped or not installed.
	Running(name string) (bool, error)
	Stop(name string) error
	Start(name string) error
}

// Tasks is Task Scheduler as the restart needs it.
type Tasks interface {
	Exists(name string) bool
	Run(name string) error
}

// StopServices stops the services of names that run and returns those.
func StopServices(svc Services, names []string, log io.Writer) []string {
	if log == nil {
		log = io.Discard
	}
	var stopped []string
	for _, n := range names {
		run, err := svc.Running(n)
		if err != nil || !run {
			continue
		}
		if err := svc.Stop(n); err != nil {
			fmt.Fprintf(log, "stop service %s: %v\n", n, err)
		} else {
			fmt.Fprintf(log, "stopped service %s\n", n)
		}
		stopped = append(stopped, n) // started again even if the stop failed half-way
	}
	return stopped
}

// RestartOptions for Restart.
type RestartOptions struct {
	Services Services
	Tasks    Tasks
	// LaunchTray starts the tray in user sid's session (longarmd: the
	// launch-tray task, registered anew); nil: the user's existing tray
	// task is run, if there is one.
	LaunchTray func(sid string) error
	// SkipTray: the user whose tray the installer starts anyway
	// (LongarmTrayLaunch).
	SkipTray string
	// OnlySID: a requester who is no administrator: only their own worker
	// and tray, no service.
	OnlySID string
	Log     io.Writer
}

// Restart starts what r recorded: the services that are not running, the
// users' worker tasks (the task's "do not start a new instance" makes it a
// no-op for a worker the installer started already) and the trays.
func Restart(r *Record, o RestartOptions) error {
	if r == nil {
		return nil
	}
	if o.Log == nil {
		o.Log = io.Discard
	}
	var errs []error
	if o.OnlySID == "" {
		for _, n := range r.Services {
			if run, err := o.Services.Running(n); err == nil && run {
				continue
			}
			if err := o.Services.Start(n); err != nil {
				errs = append(errs, fmt.Errorf("start service %s: %w", n, err))
				continue
			}
			fmt.Fprintf(o.Log, "started service %s\n", n)
		}
	}
	mine := func(sid string) bool { return o.OnlySID == "" || strings.EqualFold(sid, o.OnlySID) }
	for _, sid := range r.Workers {
		name := WorkerTaskName(sid)
		if !mine(sid) || !o.Tasks.Exists(name) {
			continue
		}
		if err := o.Tasks.Run(name); err != nil {
			errs = append(errs, fmt.Errorf("start %s: %w", name, err))
			continue
		}
		fmt.Fprintf(o.Log, "started %s\n", name)
	}
	for _, sid := range r.Trays {
		if !mine(sid) || strings.EqualFold(sid, o.SkipTray) {
			continue
		}
		var err error
		if o.LaunchTray != nil {
			err = o.LaunchTray(sid)
		} else if name := TrayTaskName(sid); o.Tasks.Exists(name) {
			err = o.Tasks.Run(name)
		} else {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("start the tray of %s: %w", sid, err))
			continue
		}
		fmt.Fprintf(o.Log, "started the tray of %s\n", sid)
	}
	return errors.Join(errs...)
}
