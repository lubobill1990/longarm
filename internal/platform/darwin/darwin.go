//go:build darwin

// Package darwin implements internal/platform on macOS: process-group
// containment (no memory limit - documented stub), sysctl process identity,
// gopsutil metrics and a launchd LaunchDaemon.
package darwin

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/process"
	xunix "golang.org/x/sys/unix"

	"github.com/lubobill1990/longarm/internal/platform/ptypes"
	"github.com/lubobill1990/longarm/internal/platform/unix"
)

// NewContainer returns a process-group container. macOS has no reliable
// memory limit for a process tree (setrlimit RLIMIT_AS breaks many runtimes);
// MemLimitBytes is ignored - the planned approach is monitor-and-kill.
func NewContainer(opts ptypes.ContainerOptions) (*unix.Container, error) {
	c := unix.NewContainer(opts)
	c.ListPIDs = listPIDs
	c.GroupLive = groupLive
	return c, nil
}

// sZomb is SZOMB from <sys/proc.h>.
const sZomb = 5

// groupLive reports whether process group pgid has a non-zombie member
// (kern.proc.pgrp); when unsure, true.
func groupLive(pgid int) bool {
	kps, err := xunix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
	if err != nil {
		return true
	}
	for _, kp := range kps {
		if kp.Proc.P_stat != sZomb {
			return true
		}
	}
	return false
}

// listPIDs returns every pid (kern.proc.all).
func listPIDs() []int {
	kps, err := xunix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil
	}
	pids := make([]int, 0, len(kps))
	for _, kp := range kps {
		pids = append(pids, int(kp.Proc.P_pid))
	}
	return pids
}

// ProcessStartTime returns the start time (µs since epoch) from kern.proc.pid.
func ProcessStartTime(pid int) (uint64, error) {
	kp, err := xunix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, err
	}
	if kp.Proc.P_pid != int32(pid) {
		return 0, fmt.Errorf("no such process %d", pid)
	}
	tv := kp.Proc.P_starttime
	return uint64(tv.Sec)*1e6 + uint64(tv.Usec), nil
}

// ProcessAlive reports whether pid runs (not a zombie) with the given start time.
func ProcessAlive(pid int, start uint64) bool {
	if !unix.Alive(pid) {
		return false
	}
	kp, err := xunix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp.Proc.P_pid != int32(pid) {
		return false
	}
	if kp.Proc.P_stat == sZomb {
		return false
	}
	if start == 0 {
		return true
	}
	tv := kp.Proc.P_starttime
	return uint64(tv.Sec)*1e6+uint64(tv.Usec) == start
}

// SampleSystem uses gopsutil (host_statistics / sysctl underneath).
func SampleSystem() (ptypes.SysSample, error) {
	var s ptypes.SysSample
	ts, err := cpu.Times(false)
	if err != nil || len(ts) == 0 {
		return s, fmt.Errorf("cpu times: %v", err)
	}
	t := ts[0]
	scale := func(f float64) uint64 { return uint64(f * 1e6) }
	s.IdleTicks = scale(t.Idle)
	s.TotalTicks = scale(t.User + t.System + t.Idle + t.Nice + t.Iowait + t.Irq + t.Softirq + t.Steal)
	n, _ := cpu.Counts(true)
	s.NumCPU = n
	vm, err := mem.VirtualMemory()
	if err != nil {
		return s, err
	}
	s.MemTotal, s.MemAvail = vm.Total, vm.Available
	if up, err := host.Uptime(); err == nil {
		s.Uptime = time.Duration(up) * time.Second
	}
	return s, nil
}

// ListProcesses uses gopsutil; per-process failures leave fields at zero.
func ListProcesses() ([]ptypes.ProcSample, error) {
	ps, err := process.Processes()
	if err != nil {
		return nil, err
	}
	out := make([]ptypes.ProcSample, 0, len(ps))
	for _, p := range ps {
		s := ptypes.ProcSample{PID: int(p.Pid)}
		s.Name, _ = p.Name()
		if pp, err := p.Ppid(); err == nil {
			s.PPID = int(pp)
		}
		if t, err := p.Times(); err == nil {
			s.CPUTime = time.Duration((t.User + t.System) * float64(time.Second))
		}
		if mi, err := p.MemoryInfo(); err == nil {
			s.RSS = mi.RSS
		}
		if ct, err := p.CreateTime(); err == nil {
			s.StartTime = uint64(ct)
		}
		out = append(out, s)
	}
	return out, nil
}

// ---- launchd -----------------------------------------------------------------

func label(name string) string { return "com.longarm." + name }

func plistPath(name string) string {
	return filepath.Join("/Library/LaunchDaemons", label(name)+".plist")
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

// Plist renders the LaunchDaemon. AbandonProcessGroup keeps job shims alive
// when longarmd exits or is restarted by launchd.
func Plist(cfg ptypes.ServiceConfig) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
`)
	fmt.Fprintf(&b, "<key>Label</key><string>%s</string>\n", label(cfg.Name))
	b.WriteString("<key>ProgramArguments</key><array>\n")
	for _, a := range append([]string{cfg.Exe}, cfg.Args...) {
		fmt.Fprintf(&b, "  <string>%s</string>\n", xmlEscape(a))
	}
	b.WriteString("</array>\n")
	if cfg.User != "" {
		fmt.Fprintf(&b, "<key>UserName</key><string>%s</string>\n", xmlEscape(cfg.User))
	}
	b.WriteString("<key>RunAtLoad</key><true/>\n<key>KeepAlive</key><true/>\n<key>AbandonProcessGroup</key><true/>\n</dict></plist>\n")
	return b.String()
}

func launchctl(args ...string) error {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// InstallService writes the plist (does not load it; use StartService).
func InstallService(cfg ptypes.ServiceConfig) error {
	if _, err := os.Stat(plistPath(cfg.Name)); err == nil {
		return fmt.Errorf("%s already exists", plistPath(cfg.Name))
	}
	return os.WriteFile(plistPath(cfg.Name), []byte(Plist(cfg)), 0o644)
}

// UninstallService unloads and removes the plist.
func UninstallService(name string) error {
	_ = StopService(name)
	if err := os.Remove(plistPath(name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func StartService(name string) error { return launchctl("bootstrap", "system", plistPath(name)) }
func StopService(name string) error  { return launchctl("bootout", "system/"+label(name)) }

// QueryService checks `launchctl print`.
func QueryService(name string) (ptypes.ServiceStatus, error) {
	if _, err := os.Stat(plistPath(name)); err != nil {
		return ptypes.ServiceNotInstalled, nil
	}
	out, err := exec.Command("launchctl", "print", "system/"+label(name)).Output()
	if err != nil {
		return ptypes.ServiceStopped, nil
	}
	if strings.Contains(string(out), "state = running") {
		return ptypes.ServiceRunning, nil
	}
	return ptypes.ServiceStopped, nil
}

// RunAsService: launchd runs longarmd as a plain foreground process.
func RunAsService(string, func(context.Context) error) (bool, error) { return false, nil }
