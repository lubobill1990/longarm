package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/lubobill1990/longarm/internal/platform"
)

// ErrRestart is returned by longarmd's Run after an update was applied: the
// caller exits so that the service manager (or, in the foreground, longarmd
// itself) starts the new binary.
var ErrRestart = errors.New("restarting for a self-update")

// Files and defaults.
const (
	StateFile    = "update.json"
	skipFile     = "update-skip.json"
	stagingDir   = ".longarm-update"
	backupSuffix = ".old"
	badSuffix    = ".bad"
)

var (
	// DefaultHealthWindow: the new version must stay healthy this long.
	DefaultHealthWindow = 60 * time.Second
	// DefaultStartGrace: how long the watchdog waits for the new version to
	// start at all (service restart delay, AV scans of a new exe).
	DefaultStartGrace = 2 * time.Minute
	watchInterval     = 500 * time.Millisecond
	stateReadTries    = 120 // x watchInterval: unreadable update.json before the watchdog gives up
)

// Binaries that are updated together when present next to longarmd.
var Binaries = []string{"longarmd", "longarm", "longarm-tray"}

// Phases of State.
const (
	PhaseRestarting = "restarting" // old binary swapped the files and exits
	PhaseChecking   = "checking"   // new binary runs its health check
	PhaseOK         = "ok"
	PhaseUnhealthy  = "unhealthy" // the new binary failed its check; watchdog rolls back
	PhaseRolledBack = "rolled_back"
	PhaseFailed     = "failed" // could not even swap
)

// Swap is one replaced file.
type Swap struct {
	Path   string `json:"path"`   // the binary's path (now the new version)
	Backup string `json:"backup"` // where the previous version was moved
}

// State is <data>/update.json, shared by the old binary, its watchdog and
// the new binary.
type State struct {
	From          string    `json:"from"`
	To            string    `json:"to"`
	Phase         string    `json:"phase"`
	Error         string    `json:"error,omitempty"`
	Started       time.Time `json:"started"`
	CheckStarted  time.Time `json:"check_started,omitempty"`
	HealthWindow  Duration  `json:"health_window"`
	StartGrace    Duration  `json:"start_grace"`
	Swaps         []Swap    `json:"swaps"`
	Mode          string    `json:"mode"` // service | exec
	ServiceName   string    `json:"service_name,omitempty"`
	Exe           string    `json:"exe"`
	Args          []string  `json:"args,omitempty"` // exec mode relaunch
	PID           int       `json:"pid,omitempty"`  // the new longarmd
	PIDStart      uint64    `json:"pid_start,omitempty"`
	WatchdogPID   int       `json:"watchdog_pid,omitempty"`
	WatchdogStart uint64    `json:"watchdog_start,omitempty"`
	Reported      bool      `json:"reported,omitempty"` // outcome sent to the hub
	Updated       time.Time `json:"updated"`
}

// Duration marshals as a Go duration string.
type Duration struct{ time.Duration }

func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	d.Duration = v
	return err
}

// LoadState reads update.json (nil if there is none).
func LoadState(dataDir string) (*State, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, StateFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", StateFile, err)
	}
	return &s, nil
}

// Save writes update.json atomically.
func (s *State) Save(dataDir string) error {
	s.Updated = time.Now()
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dataDir, StateFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return platform.ReplaceFile(tmp, filepath.Join(dataDir, StateFile))
}

// Config of the updater in longarmd.
type Config struct {
	DataDir      string
	Exe          string // the running longarmd
	Version      string // the running version
	PublicKey    string // default: the embedded PublicKey
	HealthWindow time.Duration
	StartGrace   time.Duration
	Mode         string // service | exec
	ServiceName  string
	Args         []string // exec mode: longarmd's arguments
	Log          *slog.Logger
}

func (c *Config) defaults() {
	if c.PublicKey == "" {
		c.PublicKey = PublicKey
	}
	if c.HealthWindow <= 0 {
		c.HealthWindow = DefaultHealthWindow
	}
	if c.StartGrace <= 0 {
		c.StartGrace = DefaultStartGrace
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	if c.Mode == "" {
		c.Mode = "exec"
	}
}

// File is one release file offered by the hub.
type File struct {
	Name      string // longarmd-windows-amd64.exe
	SHA256    string
	Size      int64
	Signature []byte
}

// Fetch downloads a release file into w.
type Fetch func(ctx context.Context, version, name string, w io.Writer) error

// ReleaseName is the release file name of a binary for this platform.
func ReleaseName(bin, goos, goarch string) string {
	n := bin + "-" + goos + "-" + goarch
	if goos == "windows" {
		n += ".exe"
	}
	return n
}

func exeName(bin string) string {
	if runtime.GOOS == "windows" {
		return bin + ".exe"
	}
	return bin
}

// Apply downloads, verifies and installs version: every binary of
// Binaries that exists next to longarmd is replaced (longarmd must be offered), the
// previous files are kept as <name>.old, a watchdog is started from the
// old longarmd, and update.json says "restarting". The caller must then restart
// (return ErrRestart from Run).
func Apply(ctx context.Context, cfg Config, version string, files []File, fetch Fetch) error {
	cfg.defaults()
	if cfg.PublicKey == "" {
		return errors.New("self-update disabled: this build has no update public key")
	}
	if !ValidVersion(version) {
		return fmt.Errorf("refusing update to an invalid version %q", version)
	}
	if Skipped(cfg.DataDir, version) {
		return fmt.Errorf("version %s failed here before; not retrying it", version)
	}
	dir := filepath.Dir(cfg.Exe)
	byName := map[string]File{}
	for _, f := range files {
		byName[f.Name] = f
	}
	type item struct {
		path  string
		file  File
		stage string
	}
	var items []item
	for _, bin := range Binaries {
		path := filepath.Join(dir, exeName(bin))
		if bin == "longarmd" {
			path = cfg.Exe
		} else if _, err := os.Stat(path); err != nil {
			continue
		}
		f, ok := byName[ReleaseName(bin, runtime.GOOS, runtime.GOARCH)]
		if !ok {
			if bin == "longarmd" {
				return fmt.Errorf("release %s has no %s", version, ReleaseName(bin, runtime.GOOS, runtime.GOARCH))
			}
			cfg.Log.Warn("update: release has no file for this binary; keeping it", "binary", path)
			continue
		}
		items = append(items, item{path: path, file: f})
	}
	// Stage on the same volume (rename must not cross devices).
	stage := filepath.Join(dir, stagingDir, version)
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return fmt.Errorf("update staging dir: %w (is %s writable by the service account?)", err, dir)
	}
	for i := range items {
		it := &items[i]
		it.stage = filepath.Join(stage, filepath.Base(it.path))
		if err := download(ctx, cfg, version, it.file, it.stage, fetch); err != nil {
			os.RemoveAll(stage)
			return err
		}
	}
	// Swap: current -> .old, staged -> current. Undo on any error.
	st := &State{From: cfg.Version, To: version, Phase: PhaseRestarting, Started: time.Now(),
		HealthWindow: Duration{cfg.HealthWindow}, StartGrace: Duration{cfg.StartGrace},
		Mode: cfg.Mode, ServiceName: cfg.ServiceName, Exe: cfg.Exe, Args: cfg.Args}
	undo := func() {
		for i := len(st.Swaps) - 1; i >= 0; i-- {
			s := st.Swaps[i]
			os.Remove(s.Path)
			os.Rename(s.Backup, s.Path)
		}
	}
	for _, it := range items {
		backup := it.path + backupSuffix
		if err := moveAside(backup); err != nil {
			undo()
			return err
		}
		if err := os.Rename(it.path, backup); err != nil {
			undo()
			return fmt.Errorf("update: move %s aside: %w", it.path, err)
		}
		st.Swaps = append(st.Swaps, Swap{Path: it.path, Backup: backup})
		if err := os.Rename(it.stage, it.path); err != nil {
			undo()
			return fmt.Errorf("update: install %s: %w", it.path, err)
		}
	}
	os.RemoveAll(stage)
	if err := st.Save(cfg.DataDir); err != nil {
		undo()
		return err
	}
	// The watchdog runs from the previous longarmd binary: it must work even if
	// the new one never starts.
	var longarmdBackup string
	for _, s := range st.Swaps {
		if s.Path == cfg.Exe {
			longarmdBackup = s.Backup
		}
	}
	cmd, err := startWatchdog(longarmdBackup, cfg.DataDir)
	if err != nil {
		undo()
		st.Phase, st.Error = PhaseFailed, "watchdog: "+err.Error()
		st.Save(cfg.DataDir)
		return fmt.Errorf("update: start watchdog: %w", err)
	}
	if cmd.Process != nil {
		st.WatchdogPID = cmd.Process.Pid
		st.WatchdogStart, _ = platform.ProcessStartTime(st.WatchdogPID)
		go cmd.Wait() // reap it if it ends while longarmd still runs
	}
	st.Save(cfg.DataDir)
	cfg.Log.Info("update: installed; restarting", "from", cfg.Version, "to", version, "files", len(st.Swaps), "watchdog", st.WatchdogPID)
	return nil
}

// moveAside frees name (an older backup): removed, or renamed if it is
// still in use (Windows: shims of old jobs run from it).
func moveAside(name string) error {
	if _, err := os.Stat(name); err != nil {
		return nil
	}
	if os.Remove(name) == nil {
		return nil
	}
	if err := os.Rename(name, fmt.Sprintf("%s-%d", name, time.Now().UnixNano())); err != nil {
		return fmt.Errorf("update: %s is in use and can't be moved: %w", name, err)
	}
	return nil
}

func download(ctx context.Context, cfg Config, version string, f File, dst string, fetch Fetch) error {
	if len(f.Signature) == 0 {
		return fmt.Errorf("release %s: %s is not signed", version, f.Name)
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	h := sha256.New()
	err = fetch(ctx, version, f.Name, io.MultiWriter(out, h))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
		return fmt.Errorf("download %s: %w", f.Name, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, f.SHA256) {
		os.Remove(dst)
		return fmt.Errorf("%s: sha256 %s, the hub says %s", f.Name, got, f.SHA256)
	}
	data, err := os.ReadFile(dst)
	if err == nil {
		err = Verify(cfg.PublicKey, data, f.Signature, f.Name, version)
	}
	if err != nil {
		os.Remove(dst)
		return err
	}
	return nil
}

// ---------------------------------------------------------------- skip list

// Skipped reports whether version failed on this node before.
func Skipped(dataDir, version string) bool {
	return slices.Contains(skipList(dataDir), version)
}

func skipList(dataDir string) []string {
	var l []string
	b, err := os.ReadFile(filepath.Join(dataDir, skipFile))
	if err == nil {
		json.Unmarshal(b, &l)
	}
	return l
}

func addSkip(dataDir, version string) {
	l := skipList(dataDir)
	if slices.Contains(l, version) {
		return
	}
	l = append(l, version)
	if len(l) > 20 {
		l = l[len(l)-20:]
	}
	b, _ := json.Marshal(l)
	os.WriteFile(filepath.Join(dataDir, skipFile), b, 0o600)
}

// ClearSkip forgets that version failed (an admin pushed it again on purpose).
func ClearSkip(dataDir, version string) {
	l := slices.DeleteFunc(skipList(dataDir), func(s string) bool { return s == version })
	b, _ := json.Marshal(l)
	os.WriteFile(filepath.Join(dataDir, skipFile), b, 0o600)
}

// Cleanup removes leftovers that are no longer in use: staging dirs, .bad
// files and older backups (".old-<n>"); the latest ".old" stays for a
// manual rollback. Files still in use (Windows) are left for next time.
func Cleanup(exe string) {
	dir := filepath.Dir(exe)
	os.RemoveAll(filepath.Join(dir, stagingDir))
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		n := e.Name()
		for _, bin := range Binaries { // only our own leftovers (longarmd lives in shared dirs like /usr/local/bin)
			b := exeName(bin)
			if n == b+badSuffix || strings.HasPrefix(n, b+backupSuffix+"-") {
				os.Remove(filepath.Join(dir, n))
			}
		}
	}
}

// CanApply reports why this installation can't update itself (nil = it
// can): no public key in the build, or longarmd's directory is not writable by
// the account longarmd runs as (package-managed installs: deb/rpm/brew update
// through their package manager).
func CanApply(exe, publicKey string) error {
	if publicKey == "" {
		return errors.New("this build has no update public key")
	}
	f, err := os.CreateTemp(filepath.Dir(exe), ".longarm-update-probe-*")
	if err != nil {
		return fmt.Errorf("%s is not writable for this account (package-managed install?)", filepath.Dir(exe))
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return nil
}
