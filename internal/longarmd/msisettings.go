package longarmd

import (
	"fmt"
	"sort"
	"strings"
)

// The sign-in defaults the MSI records in HKLM\Software\Longarm - Hub,
// CaFingerprint, Login, Labels, Mode: where `longarm login`, `longarmd
// login` and the tray sign in without a hub, and which CA they pin. They
// are written by the helper (install-helper msi settings), not by the MSI's
// Registry table: a value only reaches HKLM after the helper validated it
// and - when it changes what an earlier install recorded - checked that an
// administrator asked. (A non-administrator can run a repair of a
// per-machine product elevated with public properties of their choosing,
// e.g. REMOVE=ALL REINSTALL=ALL HUB=<evil>: their sign-ins would go to the
// attacker's hub.)

// MSISettingNames maps the settings step's keys to their registry names.
var MSISettingNames = map[string]string{
	"hub":    "Hub",
	"ca":     "CaFingerprint",
	"login":  "Login",
	"labels": "Labels",
	"mode":   "Mode",
}

// SettingsStore reads and writes HKLM\Software\Longarm values (tests:
// memory).
type SettingsStore interface {
	Get(name string) (string, bool)
	Set(name, value string) error
	Delete(name string) error
}

// ApplyMSISettings writes the sign-in defaults d (keys of
// MSISettingNames) to s. A change of a value an earlier install recorded
// needs an administrator (req.RequireAdmin); a first install records
// freely (it needed an elevation an administrator granted).
func ApplyMSISettings(s SettingsStore, d map[string]string, req MSIRequester) error {
	var changed []string
	for k, name := range MSISettingNames {
		old, had := s.Get(name)
		if had && old != d[k] {
			changed = append(changed, name)
		}
	}
	sort.Strings(changed)
	if len(changed) > 0 {
		if err := req.RequireAdmin("changing the recorded " + strings.Join(changed, ", ")); err != nil {
			return err
		}
	}
	for k, name := range MSISettingNames {
		if err := s.Set(name, d[k]); err != nil {
			return fmt.Errorf("record %s: %w", name, err)
		}
	}
	return nil
}

// RemoveMSISettings deletes the sign-in defaults (uninstall).
func RemoveMSISettings(s SettingsStore) {
	for _, name := range MSISettingNames {
		s.Delete(name)
	}
}

// memSettings keeps settings in memory (other systems, tests).
type memSettings struct{ m map[string]string }

func (s *memSettings) Get(n string) (string, bool) { v, ok := s.m[n]; return v, ok }
func (s *memSettings) Set(n, v string) error {
	if s.m == nil {
		s.m = map[string]string{}
	}
	s.m[n] = v
	return nil
}
func (s *memSettings) Delete(n string) error { delete(s.m, n); return nil }
