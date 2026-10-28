package msiupgrade

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RecordFile, in ProgramData\Longarm (SYSTEM and Administrators only):
// what the stop step stopped, for the restart (or the rollback).
const RecordFile = "msi-upgrade.json"

// recordMaxAge: an older record is from an upgrade that never finished
// (machine off, msiexec killed): ignored.
const recordMaxAge = 2 * time.Hour

// Record is what the upgrade stopped.
type Record struct {
	Created time.Time `json:"created"`
	// Services that ran (longarmd-admin, a RUN=service longarmd).
	Services []string `json:"services,omitempty"`
	// Workers, Trays: users (SIDs) whose user-session worker host / tray
	// ran.
	Workers []string `json:"workers,omitempty"`
	Trays   []string `json:"trays,omitempty"`
	// Aside: binaries moved aside (still in use).
	Aside []Aside `json:"aside,omitempty"`
}

// Save writes the record to dir\RecordFile (replacing it).
func (r *Record) Save(dir string) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(dir, RecordFile)
	tmp := p + ".tmp"
	os.Remove(tmp)
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// LoadRecord reads dir\RecordFile: nil when there is none or it is stale
// (removed then). Every SID and service name is checked; paths are checked
// by RestoreAside.
func LoadRecord(dir string, now time.Time) (*Record, error) {
	p := filepath.Join(dir, RecordFile)
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, 1<<20))
	f.Close()
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	if now.Sub(r.Created) > recordMaxAge || r.Created.After(now.Add(time.Minute)) {
		os.Remove(p)
		return nil, nil
	}
	for _, s := range append(append([]string(nil), r.Workers...), r.Trays...) {
		if !UserSID(s) {
			return nil, fmt.Errorf("%s: %q is not a user", p, s)
		}
	}
	for _, s := range r.Services {
		if !ServiceName(s) {
			return nil, fmt.Errorf("%s: %q is not a service name", p, s)
		}
	}
	return &r, nil
}

// RemoveRecord deletes dir\RecordFile.
func RemoveRecord(dir string) { os.Remove(filepath.Join(dir, RecordFile)) }

// ServiceName accepts the worker services' names (longarmd-admin, the
// MSI's SERVICE_NAME).
func ServiceName(s string) bool {
	if s == "" || len(s) > 80 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._-", c)) {
			return false
		}
	}
	return true
}
