package telemetry

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// EnvOptOut is the client's environment opt-out: LONGARM_TELEMETRY=0 (or
// off, false, no).
const EnvOptOut = "LONGARM_TELEMETRY"

// EnvDisabled reports whether the environment turns telemetry off.
func EnvDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvOptOut))) {
	case "0", "off", "false", "no", "disabled":
		return true
	}
	return false
}

// SettingsFile is the persistent local choice in a telemetry dir
// (`longarm telemetry off|on`).
const SettingsFile = "settings.json"

// Settings is SettingsFile.
type Settings struct {
	// Disabled: the owner turned telemetry off on this machine.
	Disabled bool `json:"disabled"`
}

// LoadSettings reads dir's settings (missing = defaults).
func LoadSettings(dir string) (Settings, error) {
	var s Settings
	b, err := os.ReadFile(filepath.Join(dir, SettingsFile))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

// SaveSettings writes dir's settings; turning telemetry off also drops
// the queue.
func SaveSettings(dir string, s Settings) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	if err := writeAtomic(filepath.Join(dir, SettingsFile), append(b, '\n')); err != nil {
		return err
	}
	if s.Disabled {
		q := &Queue{Dir: dir}
		return q.Clear()
	}
	return nil
}
