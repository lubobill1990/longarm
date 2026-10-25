// Package ptypes holds the plain data types shared by the platform facade
// (internal/platform) and its per-OS implementations
// (internal/platform/{windows,linux,darwin}). It has no dependencies so the
// per-OS packages can use it without import cycles.
package ptypes

import "time"

// SysSample is a point-in-time reading of machine-wide counters. CPU counters
// are in arbitrary but consistent units; only deltas between two samples of
// the same machine are meaningful.
type SysSample struct {
	IdleTicks  uint64 // cumulative idle CPU time
	TotalTicks uint64 // cumulative total CPU time (including idle)
	NumCPU     int
	MemTotal   uint64 // bytes
	MemAvail   uint64 // bytes
	Uptime     time.Duration
}

// ProcSample is one process in a process listing.
type ProcSample struct {
	PID       int
	PPID      int
	Name      string        // executable name without extension (".exe")
	CPUTime   time.Duration // cumulative user+kernel CPU time; 0 if unknown
	RSS       uint64        // working set / resident set in bytes; 0 if unknown
	StartTime uint64        // opaque creation-time token; 0 if unknown
}

// ContainerOptions configures a job process-tree container.
type ContainerOptions struct {
	// Name is used for diagnostics (and, on some platforms, object names).
	Name string
	// MemLimitBytes caps the memory of the whole tree; 0 = no limit.
	// Best effort: Windows enforces it via the Job Object, Linux/macOS P0a do
	// not (documented gap).
	MemLimitBytes uint64
	// KillOnOwnerDeath: kill the tree when the creating process dies. Always
	// true on Windows (KILL_ON_JOB_CLOSE). On Linux it sets Pdeathsig on the
	// direct child, which pins the calling goroutine to its OS thread for good,
	// so only the long-lived shim main goroutine should ask for it.
	KillOnOwnerDeath bool
}

// ServiceConfig describes the longarmd system service.
type ServiceConfig struct {
	Name        string   // service / unit / label name
	DisplayName string   // Windows display name
	Description string   // human description
	Exe         string   // absolute path of the binary
	Args        []string // arguments (e.g. "run", "--data-dir", "...")
	// User to run as. Windows: "DOMAIN\user" or ".\user" (empty = LocalSystem,
	// not recommended). Linux: User= of the unit. macOS: UserName of the plist.
	User string
	// Password for the Windows account (ignored elsewhere).
	Password string
}

// ServiceStatus is the coarse state of the installed service.
type ServiceStatus string

const (
	ServiceNotInstalled ServiceStatus = "not-installed"
	ServiceStopped      ServiceStatus = "stopped"
	ServiceRunning      ServiceStatus = "running"
	ServicePending      ServiceStatus = "pending"
	ServiceUnknown      ServiceStatus = "unknown"
)

// PTYOptions configures a process started on a pseudo terminal.
type PTYOptions struct {
	Argv       []string
	Dir        string
	Env        []string // full environment ("K=V"); nil = inherit
	Cols, Rows uint16   // 0 = 80x25
	// CmdLine (Windows only) is the raw command line passed to the process
	// instead of one composed from Argv (Argv[0] still names the program).
	CmdLine string
}
