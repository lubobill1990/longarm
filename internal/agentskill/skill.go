// Package agentskill installs the Longarm agent skill (skills/longarm,
// embedded in the binary) into coding agents' user-global skills
// directories, opt-in, and keeps the copies it installed current.
//
// A copy is a plain copy of the files plus a manifest (.longarm-skill.json)
// with the version and every file's sha256. Longarm only ever writes,
// updates or deletes a copy whose files still match its manifest: a
// directory without the manifest (somebody else's skill of that name) or
// with a file changed, added where a bundled file goes, or deleted is left
// alone and the request refused with the files named. All file access goes
// through an os.Root of the skill directory, which must be a real directory
// (no symlink or junction); entries in it must be regular files and real
// directories.
package agentskill

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lubobill1990/longarm/internal/version"
	"github.com/lubobill1990/longarm/skills"
)

// SkillName is the skill's directory name in a skills directory.
const SkillName = "longarm"

// ManifestName is the manifest Longarm writes into an installed copy.
const ManifestName = ".longarm-skill.json"

// State of the skill for one agent.
type State string

const (
	Absent   State = "absent"   // not installed
	Current  State = "current"  // installed by Longarm, same as the bundled skill
	Outdated State = "outdated" // installed by Longarm, unmodified, older than the bundled skill
	Modified State = "modified" // installed by Longarm, changed since: left alone
	Foreign  State = "foreign"  // a "longarm" skill Longarm did not install: left alone
)

// Status of the skill for one agent.
type Status struct {
	Agent      string   `json:"agent"`
	Name       string   `json:"name"`
	Detected   bool     `json:"detected"`
	DetectedBy string   `json:"detected_by,omitempty"`
	Dir        string   `json:"dir"`
	State      State    `json:"state"`
	Installed  string   `json:"installed_version,omitempty"`
	Bundled    string   `json:"bundled_version"`
	Modified   []string `json:"modified,omitempty"` // files changed (or deleted) since Longarm installed them
	Extra      []string `json:"extra,omitempty"`    // files Longarm did not install (never touched)
	Note       string   `json:"note,omitempty"`
}

// ErrUnknownAgent: the agent ID is not in Agents.
var ErrUnknownAgent = errors.New("unknown agent")

// RefusedError: Longarm will not touch the copy (user files).
type RefusedError struct {
	Dir    string
	Reason string
	Files  []string
}

func (e *RefusedError) Error() string {
	msg := e.Reason + " (" + e.Dir + ")"
	if len(e.Files) > 0 {
		msg += ": " + strings.Join(e.Files, ", ")
	}
	return msg
}

type manifest struct {
	Schema      int               `json:"schema"`
	Skill       string            `json:"skill"`
	Version     string            `json:"version"`
	Digest      string            `json:"digest"`
	InstalledAt time.Time         `json:"installed_at"`
	Files       map[string]string `json:"files"` // slash path -> sha256 hex
}

// bundle is the embedded skill.
type bundle struct {
	files  map[string][]byte
	hashes map[string]string
	digest string
}

// Bundle is the skill to install; tests replace it.
var Bundle fs.FS = mustSub(skills.FS, SkillName)

// BundledVersion is the version stamped into installed copies.
var BundledVersion = func() string { return version.Version }

func mustSub(f fs.FS, dir string) fs.FS {
	s, err := fs.Sub(f, dir)
	if err != nil {
		panic(err)
	}
	return s
}

func loadBundle() (*bundle, error) {
	b := &bundle{files: map[string][]byte{}, hashes: map[string]string{}}
	err := fs.WalkDir(Bundle, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !validName(p) {
			return fmt.Errorf("bundled skill: bad file name %q", p)
		}
		data, err := fs.ReadFile(Bundle, p)
		if err != nil {
			return err
		}
		b.files[p] = data
		b.hashes[p] = sum(data)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if _, ok := b.files["SKILL.md"]; !ok {
		return nil, errors.New("bundled skill: no SKILL.md")
	}
	b.digest = digest(b.hashes)
	return b, nil
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func digest(hashes map[string]string) string {
	names := make([]string, 0, len(hashes))
	for n := range hashes {
		names = append(names, n)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		fmt.Fprintf(h, "%s\x00%s\n", n, hashes[n])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// validName: a clean relative slash path inside the skill directory, never
// the manifest, with nothing a Windows path would read differently.
func validName(n string) bool {
	if !fs.ValidPath(n) || n == "." || n == ManifestName || strings.ContainsAny(n, "\\:\x00") {
		return false
	}
	for _, el := range strings.Split(n, "/") {
		if strings.HasSuffix(el, ".") || strings.HasSuffix(el, " ") {
			return false
		}
	}
	return true
}

// inspection is what is in an agent's skill directory.
type inspection struct {
	dir       string
	exists    bool
	empty     bool
	man       *manifest
	badMan    string   // the manifest is unreadable or invalid: why
	modified  []string // manifest files changed, deleted or not regular files
	conflicts []string // bundled files present but not Longarm's
	extra     []string // files Longarm did not install
}

// realDir: dir must be a directory itself, not a symlink, junction or
// other reparse point.
func realDir(dir string) (exists bool, err error) {
	fi, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if fi.Mode().Type() != fs.ModeDir {
		return true, &RefusedError{Dir: dir, Reason: "not a plain directory (a file, symlink or junction): Longarm leaves it alone"}
	}
	return true, nil
}

// openRoot opens dir as an os.Root, checking that what was opened is the
// plain directory Lstat saw (not a symlink swapped in meanwhile).
func openRoot(dir string) (*os.Root, error) {
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if fi.Mode().Type() != fs.ModeDir {
		return nil, &RefusedError{Dir: dir, Reason: "not a plain directory (a file, symlink or junction): Longarm leaves it alone"}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	if opened, err := root.Stat("."); err != nil || !os.SameFile(fi, opened) {
		root.Close()
		return nil, &RefusedError{Dir: dir, Reason: "the directory changed while Longarm opened it"}
	}
	return root, nil
}

func inspect(dir string, b *bundle) (*inspection, error) {
	in := &inspection{dir: dir}
	exists, err := realDir(dir)
	if err != nil || !exists {
		return in, err
	}
	in.exists = true
	root, err := openRoot(dir)
	if err != nil {
		return in, err
	}
	defer root.Close()
	files := map[string]fs.FileMode{} // every entry under dir (Lstat types)
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != "." && !d.IsDir() {
			files[p] = d.Type()
		}
		return nil
	})
	if err != nil {
		return in, err
	}
	in.empty = len(files) == 0
	if t, ok := files[ManifestName]; ok {
		delete(files, ManifestName)
		if t != 0 {
			in.badMan = "its manifest is not a regular file"
		} else if data, err := root.ReadFile(ManifestName); err != nil {
			in.badMan = "its manifest is unreadable: " + err.Error()
		} else {
			var m manifest
			if err := json.Unmarshal(data, &m); err != nil || m.Skill != SkillName || len(m.Files) == 0 {
				in.badMan = "its manifest is invalid"
			} else {
				for n := range m.Files {
					if !validName(n) {
						in.badMan = "its manifest names a file outside the skill: " + n
						break
					}
				}
				if in.badMan == "" {
					in.man = &m
				}
			}
		}
	}
	if in.man != nil {
		for n, want := range in.man.Files {
			t, ok := files[n]
			delete(files, n)
			if !ok || t != 0 {
				in.modified = append(in.modified, n)
				continue
			}
			data, err := root.ReadFile(n)
			if err != nil || sum(data) != want {
				in.modified = append(in.modified, n)
			}
		}
	}
	for n := range files {
		if _, bundled := b.files[n]; bundled && in.man != nil {
			in.conflicts = append(in.conflicts, n)
		} else {
			in.extra = append(in.extra, n)
		}
	}
	sort.Strings(in.modified)
	sort.Strings(in.conflicts)
	sort.Strings(in.extra)
	return in, nil
}

func (in *inspection) state(b *bundle) State {
	switch {
	case !in.exists || (in.empty && in.man == nil && in.badMan == ""):
		return Absent
	case in.man == nil:
		return Foreign
	case len(in.modified) > 0 || len(in.conflicts) > 0:
		return Modified
	case in.man.Digest != b.digest || in.man.Version != BundledVersion():
		return Outdated
	}
	return Current
}

func (e Env) status(a Agent, b *bundle) (Status, *inspection, error) {
	st := Status{Agent: a.ID, Name: a.Name, Bundled: BundledVersion()}
	st.Detected, st.DetectedBy = e.detect(a)
	skillsDir := e.SkillsDir(a)
	if skillsDir == "" {
		return st, nil, fmt.Errorf("%s: no skills directory (no home directory?)", a.Name)
	}
	st.Dir = filepath.Join(skillsDir, SkillName)
	in, err := inspect(st.Dir, b)
	if err != nil {
		var re *RefusedError
		if errors.As(err, &re) {
			st.State, st.Note = Foreign, re.Reason
			return st, nil, nil
		}
		return st, nil, err
	}
	st.State = in.state(b)
	if in.man != nil {
		st.Installed = in.man.Version
	}
	st.Modified = append(append([]string{}, in.modified...), in.conflicts...)
	sort.Strings(st.Modified)
	st.Extra = in.extra
	switch st.State {
	case Foreign:
		st.Note = "a skill named " + SkillName + " that Longarm did not install"
		if in.badMan != "" {
			st.Note += " (" + in.badMan + ")"
		}
	case Modified:
		st.Note = "changed since Longarm installed it; Longarm leaves it alone"
	}
	return st, in, nil
}

// Status reports the skill for one agent.
func (e Env) Status(id string) (Status, error) {
	a, ok := Lookup(id)
	if !ok {
		return Status{}, fmt.Errorf("%w %q (known: %s)", ErrUnknownAgent, id, strings.Join(AgentIDs(), ", "))
	}
	b, err := loadBundle()
	if err != nil {
		return Status{}, err
	}
	st, _, err := e.status(a, b)
	return st, err
}

// StatusAll reports the skill for every agent.
func (e Env) StatusAll() ([]Status, error) {
	b, err := loadBundle()
	if err != nil {
		return nil, err
	}
	out := make([]Status, 0, len(Agents))
	for _, a := range Agents {
		st, _, err := e.status(a, b)
		if err != nil {
			st.Note = err.Error()
		}
		out = append(out, st)
	}
	return out, nil
}

// Install installs the bundled skill for the agent, or updates a copy
// Longarm installed that nobody changed since. Anything else in the way is
// refused (RefusedError).
func (e Env) Install(id string) (Status, error) {
	a, ok := Lookup(id)
	if !ok {
		return Status{}, fmt.Errorf("%w %q (known: %s)", ErrUnknownAgent, id, strings.Join(AgentIDs(), ", "))
	}
	b, err := loadBundle()
	if err != nil {
		return Status{}, err
	}
	st, in, err := e.status(a, b)
	if err != nil {
		return st, err
	}
	switch st.State {
	case Foreign:
		return st, &RefusedError{Dir: st.Dir, Reason: "a skill named " + SkillName + " that Longarm did not install is there; remove or rename it to install Longarm's"}
	case Modified:
		return st, &RefusedError{Dir: st.Dir, Reason: "the installed skill was changed since Longarm installed it; Longarm does not overwrite your changes - remove or rename the folder to install the current one", Files: st.Modified}
	case Current:
		return st, nil
	}
	if !in.exists {
		if err := os.MkdirAll(filepath.Dir(st.Dir), 0o755); err != nil {
			return st, err
		}
		if err := os.Mkdir(st.Dir, 0o755); err != nil {
			return st, err
		}
		if _, err := realDir(st.Dir); err != nil {
			return st, err
		}
	}
	root, err := openRoot(st.Dir)
	if err != nil {
		return st, err
	}
	defer root.Close()
	names := make([]string, 0, len(b.files))
	for n := range b.files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := writeFile(root, n, b.files[n]); err != nil {
			return st, fmt.Errorf("%s: %w", filepath.Join(st.Dir, filepath.FromSlash(n)), err)
		}
	}
	if in.man != nil { // files of the old copy the new one no longer has (unmodified: checked)
		var dirs []string
		for n := range in.man.Files {
			if _, keep := b.files[n]; !keep {
				if err := root.Remove(n); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return st, err
				}
				dirs = append(dirs, parents(n)...)
			}
		}
		removeEmptyDirs(root, dirs)
	}
	m := manifest{Schema: 1, Skill: SkillName, Version: BundledVersion(), Digest: b.digest, InstalledAt: time.Now().UTC().Truncate(time.Second), Files: b.hashes}
	data, _ := json.MarshalIndent(m, "", "  ")
	if err := writeFile(root, ManifestName, append(data, '\n')); err != nil {
		return st, err
	}
	st, _, err = e.status(a, b)
	return st, err
}

// Remove deletes the copy Longarm installed for the agent: only its own,
// unmodified files; files somebody added stay (and so does the folder).
func (e Env) Remove(id string) (Status, error) {
	a, ok := Lookup(id)
	if !ok {
		return Status{}, fmt.Errorf("%w %q (known: %s)", ErrUnknownAgent, id, strings.Join(AgentIDs(), ", "))
	}
	b, err := loadBundle()
	if err != nil {
		return Status{}, err
	}
	st, in, err := e.status(a, b)
	if err != nil {
		return st, err
	}
	switch st.State {
	case Absent:
		return st, nil
	case Foreign:
		return st, &RefusedError{Dir: st.Dir, Reason: "Longarm did not install this skill and does not remove it"}
	case Modified:
		return st, &RefusedError{Dir: st.Dir, Reason: "the skill was changed since Longarm installed it; Longarm does not delete your changes - remove the folder yourself if you no longer want it", Files: st.Modified}
	}
	root, err := openRoot(st.Dir)
	if err != nil {
		return st, err
	}
	var dirs []string
	for n := range in.man.Files {
		if err := root.Remove(n); err != nil && !errors.Is(err, fs.ErrNotExist) {
			root.Close()
			return st, err
		}
		dirs = append(dirs, parents(n)...)
	}
	if err := root.Remove(ManifestName); err != nil && !errors.Is(err, fs.ErrNotExist) {
		root.Close()
		return st, err
	}
	removeEmptyDirs(root, dirs)
	root.Close()
	if len(in.extra) == 0 {
		os.Remove(st.Dir) // only when empty
	}
	st, _, err = e.status(a, b)
	if err == nil && len(in.extra) > 0 {
		st.Note = "removed Longarm's files; kept the files you added: " + strings.Join(in.extra, ", ")
	}
	return st, err
}

// Refresh updates every copy Longarm installed that is unmodified and
// older than the bundled skill (after an update of Longarm). It never
// installs a new copy or touches a changed or foreign one.
func (e Env) Refresh() ([]Status, error) {
	all, err := e.StatusAll()
	if err != nil {
		return nil, err
	}
	var out []Status
	var errs []error
	for _, st := range all {
		if st.State != Outdated {
			continue
		}
		nst, err := e.Install(st.Agent)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", st.Name, err))
			continue
		}
		out = append(out, nst)
	}
	return out, errors.Join(errs...)
}

// parents lists n's parent directories, deepest first.
func parents(n string) []string {
	var out []string
	for d := path.Dir(n); d != "."; d = path.Dir(d) {
		out = append(out, d)
	}
	return out
}

// removeEmptyDirs removes the directories that are empty, deepest first.
func removeEmptyDirs(root *os.Root, dirs []string) {
	sort.Slice(dirs, func(i, j int) bool { return strings.Count(dirs[i], "/") > strings.Count(dirs[j], "/") })
	for _, d := range dirs {
		if fi, err := root.Lstat(d); err == nil && fi.Mode().Type() == fs.ModeDir {
			root.Remove(d) // fails when not empty
		}
	}
}

// writeFile writes name in root atomically (temporary file, then rename).
// Every directory on the way must be a real directory and the file, if it
// exists, a regular file.
func writeFile(root *os.Root, name string, data []byte) error {
	if dir := path.Dir(name); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		for _, d := range parents(name) {
			fi, err := root.Lstat(d)
			if err != nil {
				return err
			}
			if fi.Mode().Type() != fs.ModeDir {
				return fmt.Errorf("%s is not a plain directory", d)
			}
		}
	}
	if fi, err := root.Lstat(name); err == nil && !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", name)
	}
	rnd := make([]byte, 6)
	rand.Read(rnd)
	tmp := name + ".longarm-" + hex.EncodeToString(rnd)
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = root.Rename(tmp, name)
	}
	if err != nil {
		root.Remove(tmp)
		return err
	}
	return nil
}

// SkillFile returns the bundled SKILL.md (for `longarm skill show`).
func SkillFile() ([]byte, error) {
	b, err := fs.ReadFile(Bundle, "SKILL.md")
	return bytes.Clone(b), err
}
