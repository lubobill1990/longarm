package tray

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/telemetry"
)

// PrefsFile holds the tray's own settings in longarm's config dir.
const PrefsFile = "tray.json"

// Prefs are the tray's settings (Settings tab). The zero value is the
// default: every notification on.
type Prefs struct {
	// MuteWorkers: no notifications about your workers (offline / back
	// online, memory low, link relayed).
	MuteWorkers bool `json:"mute_workers,omitempty"`
	// MuteJobs: no notifications about your jobs ending.
	MuteJobs bool `json:"mute_jobs,omitempty"`
}

var prefsCache struct {
	sync.Mutex
	path string
	mod  time.Time
	p    Prefs
}

// LoadPrefs reads dir's tray.json (missing or unreadable: defaults),
// cached until the file changes.
func LoadPrefs(dir string) Prefs {
	path := filepath.Join(dir, PrefsFile)
	st, err := os.Stat(path)
	if err != nil {
		return Prefs{}
	}
	prefsCache.Lock()
	defer prefsCache.Unlock()
	if prefsCache.path == path && prefsCache.mod.Equal(st.ModTime()) {
		return prefsCache.p
	}
	var p Prefs
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &p)
	}
	prefsCache.path, prefsCache.mod, prefsCache.p = path, st.ModTime(), p
	return p
}

// SavePrefs writes dir's tray.json.
func SavePrefs(dir string, p Prefs) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(p, "", "  ")
	path := filepath.Join(dir, PrefsFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	prefsCache.Lock()
	prefsCache.path, prefsCache.mod, prefsCache.p = path, time.Time{}, Prefs{} // reread
	prefsCache.Unlock()
	return nil
}

// ---------------------------------------------------------------- /trayapi (settings, logs, worker start, diagnostics)

func (s *Server) settingsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /trayapi/settings", s.getSettings)
	mux.HandleFunc("POST /trayapi/settings", s.setSettings)
	mux.HandleFunc("POST /trayapi/folder", s.openFolder)
	mux.HandleFunc("POST /trayapi/worker/start", s.startWorker)
	mux.HandleFunc("POST /trayapi/diag", s.collectDiag)
}

func (s *Server) telemetryDir() string {
	return filepath.Join(s.opts.LongarmConfigDir, client.TelemetryDir)
}

type traySettings struct {
	Prefs
	Version           string `json:"version"`
	TelemetryDisabled bool   `json:"telemetry_disabled"`
	TelemetryEnvOff   bool   `json:"telemetry_env_off,omitempty"` // LONGARM_TELEMETRY=0: the switch can't turn it on
	ClientLogs        string `json:"client_logs,omitempty"`
	WorkerLogs        string `json:"worker_logs,omitempty"`
}

func (s *Server) currentSettings() traySettings {
	t, _ := telemetry.LoadSettings(s.telemetryDir())
	out := traySettings{Prefs: LoadPrefs(s.opts.LongarmConfigDir), Version: s.opts.Version, TelemetryDisabled: t.Disabled,
		TelemetryEnvOff: telemetry.EnvDisabled()}
	client, worker := s.Roles()
	if client {
		out.ClientLogs = s.opts.LongarmConfigDir
	}
	if worker {
		out.WorkerLogs = s.opts.LongarmdDataDir
	}
	return out
}

func (s *Server) getSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.currentSettings())
}

// setSettings changes the given settings only.
func (s *Server) setSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MuteWorkers       *bool `json:"mute_workers"`
		MuteJobs          *bool `json:"mute_jobs"`
		TelemetryDisabled *bool `json:"telemetry_disabled"`
	}
	if !readBody(w, r, &body) {
		return
	}
	if body.MuteWorkers != nil || body.MuteJobs != nil {
		p := LoadPrefs(s.opts.LongarmConfigDir)
		if body.MuteWorkers != nil {
			p.MuteWorkers = *body.MuteWorkers
		}
		if body.MuteJobs != nil {
			p.MuteJobs = *body.MuteJobs
		}
		if err := SavePrefs(s.opts.LongarmConfigDir, p); err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if body.TelemetryDisabled != nil {
		// as `longarm telemetry off|on`: off also drops the queue
		if err := telemetry.SaveSettings(s.telemetryDir(), telemetry.Settings{Disabled: *body.TelemetryDisabled}); err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, s.currentSettings())
}

// openFolder opens a known folder in the file manager: "client-logs"
// (longarm's config dir: daemon.log, tray.log), "worker-logs" (longarmd's
// data dir: longarmd.log), "diag" (collected diagnostics). Never a path
// from the request.
func (s *Server) openFolder(w http.ResponseWriter, r *http.Request) {
	var body struct{ Which string }
	if !readBody(w, r, &body) {
		return
	}
	var dir string
	switch body.Which {
	case "client-logs":
		dir = s.opts.LongarmConfigDir
	case "worker-logs":
		dir = s.opts.LongarmdDataDir
	case "diag":
		dir = s.diagDir()
	default:
		httpError(w, http.StatusBadRequest, "unknown folder")
		return
	}
	if dir == "" {
		httpError(w, http.StatusNotFound, "not configured here")
		return
	}
	if _, err := os.Stat(dir); err != nil {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	if err := s.openURL(dir); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"folder": dir})
}

// workerStartArgs is how this machine's worker starts: the user-session
// worker's scheduled task, else the service.
func workerStartArgs(runMode string) []string {
	if runMode == "session" {
		return []string{"session-mode", "start"}
	}
	return []string{"service", "start"}
}

// startWorker starts the local worker for its run mode; a service that
// needs an administrator to start is started elevated (UAC prompt).
func (s *Server) startWorker(w http.ResponseWriter, _ *http.Request) {
	mode, _ := s.installedWorker()
	args := workerStartArgs(mode)
	out, err := s.runCmd(s.opts.LongarmdExe, args...)
	if err != nil && mode != "session" && isAccessDenied(string(out)+" "+err.Error()) {
		if err := s.runElevated(s.opts.LongarmdExe, args...); err != nil {
			httpError(w, http.StatusInternalServerError, "start the worker as an administrator: "+err.Error())
			return
		}
		writeJSON(w, map[string]string{"output": "starting the worker service (administrator)", "mode": "service"})
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, strings.TrimSpace(string(out)+" "+err.Error()))
		return
	}
	if mode == "" {
		mode = "service"
	}
	writeJSON(w, map[string]string{"output": strings.TrimSpace(string(out)), "mode": mode})
}

func isAccessDenied(s string) bool {
	s = strings.ToLower(s)
	return strings.Contains(s, "access is denied") || strings.Contains(s, "access denied") || strings.Contains(s, "permission denied")
}

func (s *Server) diagDir() string {
	if s.opts.LongarmConfigDir == "" {
		return ""
	}
	return filepath.Join(s.opts.LongarmConfigDir, "diag")
}

// collectDiag runs `longarm diag` into the diag folder (nothing is
// uploaded) and opens the folder.
func (s *Server) collectDiag(w http.ResponseWriter, _ *http.Request) {
	dir := s.diagDir()
	if dir == "" {
		httpError(w, http.StatusNotFound, "no longarm config dir")
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	zip := filepath.Join(dir, "longarm-diag-"+time.Now().Format("20060102-150405")+".zip")
	out, err := s.runCmd(s.opts.LongarmExe, s.longarmArgs("diag", "--out", zip)[1:]...)
	if err != nil {
		httpError(w, http.StatusInternalServerError, strings.TrimSpace(string(out)+" "+err.Error()))
		return
	}
	if _, err := os.Stat(zip); errors.Is(err, os.ErrNotExist) {
		httpError(w, http.StatusInternalServerError, "longarm diag wrote no bundle: "+strings.TrimSpace(string(out)))
		return
	}
	s.openURL(dir)
	writeJSON(w, map[string]string{"zip": zip, "output": strings.TrimSpace(string(out))})
}
