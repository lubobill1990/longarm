package hub

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The startup marker tells a liveness probe (`longarm-hub healthcheck
// --live`) that the hub is not up yet because it is still starting: the
// container entrypoint restores the database (Litestream: minutes for a long
// WAL), bootstraps, and the hub opens the database, before /healthz answers.
// While a phase runs, the marker's mtime is refreshed every
// StartupHeartbeat; the probe accepts a marker refreshed within
// StartupMarkerStale, so a restore whose process died or hangs past its
// timeout stops refreshing it and the probe fails again. The hub removes the
// marker once its listeners are up.
const (
	// StartupMarkerFile is the marker's name, next to the database.
	StartupMarkerFile = "longarm-hub.starting"
	// EnvStartupMarker is the marker's path, set by `longarm-hub container`
	// for the serve process (only a container start refreshes and removes it).
	EnvStartupMarker = "LONGARM_HUB_STARTUP_MARKER"
	// StartupHeartbeat is how often a running phase refreshes the marker.
	StartupHeartbeat = 10 * time.Second
	// StartupMarkerStale: an older marker no longer counts as starting.
	StartupMarkerStale = 60 * time.Second
)

// StartupMarkerPath is the marker of cfg's database (the database's directory
// is the hub's own, also when the data dir is a shared volume).
func StartupMarkerPath(cfg *Config) string {
	return filepath.Join(filepath.Dir(cfg.DatabasePath()), StartupMarkerFile)
}

// StartupMarker refreshes a marker file while a start phase runs.
type StartupMarker struct {
	path  string
	every time.Duration

	mu    sync.Mutex
	stop  chan struct{}
	done  chan struct{}
	phase string
	since time.Time
}

// NewStartupMarker returns a marker at path refreshed every `every`
// (0: StartupHeartbeat).
func NewStartupMarker(path string, every time.Duration) *StartupMarker {
	if every <= 0 {
		every = StartupHeartbeat
	}
	return &StartupMarker{path: path, every: every}
}

// Path is the marker file.
func (m *StartupMarker) Path() string { return m.path }

// Begin writes the marker for phase and refreshes it until End, Remove or,
// when limit > 0, limit after Begin: a phase that hangs longer stops counting as
// alive (the probe then fails and the container is restarted).
func (m *StartupMarker) Begin(phase string, limit time.Duration) error {
	m.End()
	now := time.Now()
	content := fmt.Sprintf("phase=%s\npid=%d\nsince=%s\n", phase, os.Getpid(), now.UTC().Format(time.RFC3339))
	if err := os.WriteFile(m.path, []byte(content), 0o600); err != nil {
		return err
	}
	stop, done := make(chan struct{}), make(chan struct{})
	m.mu.Lock()
	m.stop, m.done, m.phase, m.since = stop, done, phase, now
	m.mu.Unlock()
	go func() {
		defer close(done)
		t := time.NewTicker(m.every)
		defer t.Stop()
		var expired <-chan time.Time
		if limit > 0 {
			timer := time.NewTimer(limit)
			defer timer.Stop()
			expired = timer.C
		}
		for {
			select {
			case <-stop:
				return
			case <-expired:
				return
			case <-t.C:
				n := time.Now()
				os.Chtimes(m.path, n, n)
			}
		}
	}()
	return nil
}

// End stops refreshing the marker (it stays: the next phase or the hub's
// start takes over) and returns how long the phase ran.
func (m *StartupMarker) End() time.Duration {
	m.mu.Lock()
	stop, done, since := m.stop, m.done, m.since
	m.stop, m.done = nil, nil
	m.mu.Unlock()
	if stop == nil {
		return 0
	}
	close(stop)
	<-done
	return time.Since(since)
}

// Touch refreshes the marker once (a hand-over between processes).
func (m *StartupMarker) Touch() error {
	n := time.Now()
	return os.Chtimes(m.path, n, n)
}

// Remove stops refreshing and deletes the marker.
func (m *StartupMarker) Remove() error {
	m.End()
	if err := os.Remove(m.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// StartupInProgress reports whether the marker at path was refreshed within
// stale before now, and its phase. A missing marker is not an error (not
// starting); an old one is reported as such.
func StartupInProgress(path string, stale time.Duration, now time.Time) (phase string, age time.Duration, ok bool, err error) {
	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", 0, false, nil
		}
		return "", 0, false, err
	}
	age = now.Sub(fi.ModTime())
	if b, rerr := os.ReadFile(path); rerr == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if v, found := strings.CutPrefix(l, "phase="); found {
				phase = strings.TrimSpace(v)
				break
			}
		}
	}
	// a future mtime (clock step) counts as fresh only within stale
	return phase, age, age < stale && age > -stale, nil
}

// SetStartupMarker hands the start's marker to Serve, which removes it once
// the listeners are up (nil: none).
func (h *Hub) SetStartupMarker(m *StartupMarker) { h.startupMarker = m }
