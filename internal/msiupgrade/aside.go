package msiupgrade

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Binaries of the install dir that running processes may hold.
var Binaries = []string{"longarm.exe", "longarmd.exe", "longarm-tray.exe"}

// AsideDir is where binaries still in use go (below the install dir: the
// same volume, the same protection).
const AsideDir = "old"

// Aside is a binary moved aside.
type Aside struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Files is the file system as moving aside needs it (tests replace InUse).
type Files struct {
	// InUse reports whether a running process holds path (its image).
	InUse func(path string) bool
}

func (f Files) inUse(p string) bool {
	if f.InUse == nil {
		return InUse(p)
	}
	return f.InUse(p)
}

// MoveAside moves the binaries of dir that are still in use (job shims,
// console hosts, `longarm` clients keep running) to dir\old\<name>-<n>.exe:
// Windows lets a running image be renamed, and the installer then writes
// the new file without a reboot.
func (f Files) MoveAside(dir string, now time.Time, log io.Writer) ([]Aside, error) {
	var out []Aside
	for _, name := range Binaries {
		p := filepath.Join(dir, name)
		if _, err := os.Lstat(p); err != nil || !f.inUse(p) {
			continue
		}
		od := filepath.Join(dir, AsideDir)
		if err := os.MkdirAll(od, 0o755); err != nil {
			return out, err
		}
		to := filepath.Join(od, strings.TrimSuffix(name, ".exe")+"-"+strconv.FormatInt(now.UnixNano(), 10)+".exe")
		if err := os.Rename(p, to); err != nil {
			return out, fmt.Errorf("move %s aside: %w", p, err)
		}
		fmt.Fprintf(log, "moved %s (in use) to %s\n", p, to)
		out = append(out, Aside{From: p, To: to})
	}
	return out, nil
}

// RestoreAside moves binaries back where nothing replaced them (a rolled
// back upgrade). Only the names and dirs MoveAside uses are accepted.
func RestoreAside(dir string, as []Aside, log io.Writer) error {
	var errs []string
	for _, a := range as {
		if !validAside(dir, a) {
			errs = append(errs, fmt.Sprintf("%v: not a binary of %s", a, dir))
			continue
		}
		if _, err := os.Lstat(a.From); err == nil {
			continue // a file is there (the old product's, restored)
		}
		if err := os.Rename(a.To, a.From); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		fmt.Fprintf(log, "moved %s back\n", a.From)
	}
	if len(errs) > 0 {
		return fmt.Errorf("restore: %s", strings.Join(errs, "; "))
	}
	return nil
}

func validAside(dir string, a Aside) bool {
	ok := false
	for _, n := range Binaries {
		if strings.EqualFold(filepath.Clean(a.From), filepath.Join(dir, n)) {
			ok = true
		}
	}
	to := filepath.Clean(a.To)
	return ok && !strings.Contains(a.To, "..") && strings.EqualFold(filepath.Dir(to), filepath.Join(dir, AsideDir)) &&
		strings.EqualFold(filepath.Ext(to), ".exe")
}

// CleanAside removes the moved binaries nothing holds any more (and the
// dir once empty); the others stay for the next time.
func CleanAside(dir string, log io.Writer) {
	od := filepath.Join(dir, AsideDir)
	fi, err := os.Lstat(od)
	if err != nil || !fi.IsDir() || fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return
	}
	ents, err := os.ReadDir(od)
	if err != nil {
		return
	}
	left := 0
	for _, e := range ents {
		p := filepath.Join(od, e.Name())
		if err := os.Remove(p); err != nil {
			left++
			continue
		}
		fmt.Fprintf(log, "removed %s\n", p)
	}
	if left == 0 {
		os.Remove(od)
	}
}
