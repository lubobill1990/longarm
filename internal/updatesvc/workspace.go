package updatesvc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/lubobill1990/longarm/internal/update"
)

// UpdatesDirName: below ProgramData\Longarm, SYSTEM only.
const UpdatesDirName = "updates"

// dirInfo is what Inspect found at a path.
type dirInfo struct {
	Exists  bool
	Dir     bool
	Reparse bool // a symbolic link, junction or other reparse point
	// Secured: owned by SYSTEM with the protected SYSTEM-only DACL.
	Secured bool
}

// dirOps is the file system as the workspace needs it (windows.go; tests
// fake it).
type dirOps interface {
	Inspect(path string) (dirInfo, error)
	// Create makes the directory with the SYSTEM-only DACL from the start.
	Create(path string) error
	// Remove deletes path: a reparse point itself (never its target), else
	// the tree.
	Remove(path string) error
	// Trusted checks a parent directory: no reparse point, owned and
	// writable by SYSTEM / Administrators only.
	Trusted(path string) error
	// Lock opens a file so nobody can change, rename or delete it while
	// it is open (and refuses a reparse point).
	Lock(path string) (io.ReadCloser, error)
}

// ensureSecureDir returns parent\name, a SYSTEM-only directory: parent
// must be trusted; whatever else is at parent\name - a reparse point, a
// file, a directory with other owners or ACEs - is removed first and the
// directory made anew.
func ensureSecureDir(ops dirOps, parent, name string) (string, error) {
	if err := ops.Trusted(parent); err != nil {
		return "", fmt.Errorf("%s: %w", parent, err)
	}
	p := filepath.Join(parent, name)
	info, err := ops.Inspect(p)
	if err != nil {
		return "", err
	}
	if info.Exists && info.Dir && !info.Reparse && info.Secured {
		return p, nil
	}
	if info.Exists {
		if err := ops.Remove(p); err != nil {
			return "", fmt.Errorf("removing %s (not a SYSTEM-only directory): %w", p, err)
		}
	}
	if err := ops.Create(p); err != nil {
		return "", err
	}
	info, err = ops.Inspect(p)
	if err != nil {
		return "", err
	}
	if !info.Exists || !info.Dir || info.Reparse || !info.Secured {
		return "", fmt.Errorf("%s is not a SYSTEM-only directory after creating it", p)
	}
	return p, nil
}

// FSWorkspace is the Workspace below Root (ProgramData\Longarm):
// Root\updates (SYSTEM only), a fresh Root\updates\<version> per install,
// the installer's log Root\updates\install-<version>.log.
type FSWorkspace struct {
	Root string
	ops  dirOps
}

func (w *FSWorkspace) updatesDir() (string, error) {
	return ensureSecureDir(w.ops, w.Root, UpdatesDirName)
}

// Prepare implements Workspace.
func (w *FSWorkspace) Prepare(version string) (string, string, error) {
	if !update.ValidVersion(version) {
		return "", "", fmt.Errorf("invalid version %q", version)
	}
	up, err := w.updatesDir()
	if err != nil {
		return "", "", err
	}
	dir := filepath.Join(up, version)
	if info, err := w.ops.Inspect(dir); err != nil {
		return "", "", err
	} else if info.Exists {
		if err := w.ops.Remove(dir); err != nil {
			return "", "", err
		}
	}
	if err := w.ops.Create(dir); err != nil {
		return "", "", err
	}
	log := filepath.Join(up, "install-"+version+".log")
	if info, err := w.ops.Inspect(log); err == nil && info.Exists {
		// an old log must not pass for this install's outcome
		if err := w.ops.Remove(log); err != nil {
			return "", "", err
		}
	}
	return dir, log, nil
}

// Lock implements Workspace.
func (w *FSWorkspace) Lock(path string) (io.ReadCloser, error) { return w.ops.Lock(path) }

// Remove implements Workspace: only directories below Root\updates.
func (w *FSWorkspace) Remove(dir string) {
	up := filepath.Join(w.Root, UpdatesDirName)
	rel, err := filepath.Rel(up, dir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || strings.ContainsAny(rel, `\/`) {
		return
	}
	w.ops.Remove(dir)
}

// FileStore keeps State in Root\updates\state.json.
type FileStore struct{ Path string }

func (s FileStore) Load() (*State, error) {
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	if p := st.Pending; p != nil && (!update.ValidVersion(p.To) || !update.ValidVersion(p.From)) {
		st.Pending = nil
	}
	return &st, nil
}

func (s FileStore) Save(st *State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.Path)
}

var engineReturnRE = regexp.MustCompile(`MainEngineThread is returning (\d+)`)

// msiLogOutcome reads msiexec's verbose log (/l*v; UTF-16 or ANSI): the
// engine's return code once the install ended.
func msiLogOutcome(b []byte) (code int, done bool) {
	text := string(b)
	if len(b) >= 2 && (b[0] == 0xff && b[1] == 0xfe || bytes.Count(b[:min(len(b), 512)], []byte{0}) > 64) {
		u := make([]uint16, 0, len(b)/2)
		for i := 0; i+1 < len(b); i += 2 {
			u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
		}
		text = string(utf16.Decode(u))
	}
	m := engineReturnRE.FindAllStringSubmatch(text, -1)
	if len(m) == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(m[len(m)-1][1])
	if err != nil {
		return 0, false
	}
	return n, true
}
