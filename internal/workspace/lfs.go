package workspace

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
)

// Git LFS (docs/usage.md "Git LFS"). A shipped repository's LFS content is
// not in its git objects and the mirror has no LFS server to fetch it from:
// the smudge filter would fail the checkout ("batch request: missing
// protocol"). So worktrees are always checked out with the smudge skipped -
// LFS files stay pointer files - and, when the client shipped LFS objects
// (longarm run / sync --lfs ship) into the mirror's LFS store
// (<mirror>/lfs/objects/<aa>/<bb>/<oid>, the store git-lfs uses for every
// worktree of the mirror), `git lfs checkout` then replaces the pointers it
// has the content of.

// skipSmudge is the environment of worktree checkouts.
var skipSmudge = []string{"GIT_LFS_SKIP_SMUDGE=1"}

var oidRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidLFSOid reports whether s is a Git LFS object id (sha256 hex).
func ValidLFSOid(s string) bool { return oidRE.MatchString(s) }

func (s *Store) lfsObject(repo, oid string) string {
	return filepath.Join(s.Mirror(repo), "lfs", "objects", oid[0:2], oid[2:4], oid)
}

// maxLFSOids caps what one Refs request may ask about.
const maxLFSOids = 100000

// LFSPresent returns the oids whose content the mirror's LFS store holds.
func (s *Store) LFSPresent(repo string, oids []string) []string {
	if !ValidRepo(repo) || len(oids) > maxLFSOids {
		return nil
	}
	var have []string
	for _, oid := range oids {
		if !ValidLFSOid(oid) {
			continue
		}
		if fi, err := os.Stat(s.lfsObject(repo, oid)); err == nil && fi.Mode().IsRegular() {
			have = append(have, oid)
		}
	}
	return have
}

// ImportLFS stores the objects of a tar (entries named by their oid) in
// the mirror's LFS store; every object's content must hash to its name.
// It returns how many objects were added.
func (s *Store) ImportLFS(ctx context.Context, repo, tarPath string) (int, error) {
	m, err := s.ensureMirror(ctx, repo)
	if err != nil {
		return 0, err
	}
	unlock := s.lock(repo)
	defer unlock()
	f, err := os.Open(tarPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	tmpDir := filepath.Join(m, "lfs", "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return 0, err
	}
	n := 0
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		if h.Typeflag != tar.TypeReg || !ValidLFSOid(h.Name) {
			return n, fmt.Errorf("lfs tar: unexpected entry %q", h.Name)
		}
		dst := s.lfsObject(repo, h.Name)
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		tmp, err := os.CreateTemp(tmpDir, "obj-")
		if err != nil {
			return n, err
		}
		hash := sha256.New()
		_, err = io.Copy(io.MultiWriter(tmp, hash), tr)
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err == nil && hex.EncodeToString(hash.Sum(nil)) != h.Name {
			err = fmt.Errorf("lfs object %s: content does not match its oid", h.Name)
		}
		if err == nil {
			if err = os.MkdirAll(filepath.Dir(dst), 0o755); err == nil {
				err = os.Rename(tmp.Name(), dst)
			}
		}
		if err != nil {
			os.Remove(tmp.Name())
			return n, err
		}
		n++
	}
}

// lfsInstalled reports whether git-lfs is installed here.
func (s *Store) lfsInstalled(ctx context.Context) bool {
	s.lfsOnce.Do(func() {
		s.lfsOK = exec.CommandContext(ctx, s.git(), "lfs", "version").Run() == nil
	})
	return s.lfsOK
}

// usesLFS reports whether the worktree's root .gitattributes routes files
// through the LFS filter.
func usesLFS(dir string) bool {
	b, err := os.ReadFile(filepath.Join(dir, ".gitattributes"))
	return err == nil && bytes.Contains(b, []byte("filter=lfs"))
}

// hasLFSStore reports whether the mirror holds any LFS objects (git-lfs
// may have made empty directories there).
func (s *Store) hasLFSStore(repo string) bool {
	found := errors.New("found")
	err := filepath.WalkDir(filepath.Join(s.Mirror(repo), "lfs", "objects"), func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() && ValidLFSOid(d.Name()) {
			return found
		}
		return err
	})
	return err == found
}

// lfsFilterConfig makes the mirror's (and so every worktree's) LFS filter
// skip the smudge for the job's own git too - a later checkout in the
// worktree must not fail on content the worker lacks - while the clean
// filter keeps `git status` right for files `git lfs checkout` filled in.
var lfsFilterConfig = [][2]string{
	{"filter.lfs.clean", "git-lfs clean -- %f"},
	{"filter.lfs.smudge", "git-lfs smudge --skip -- %f"},
	{"filter.lfs.process", "git-lfs filter-process --skip"},
	{"filter.lfs.required", "true"},
}

// checkoutLFS runs after a worktree checkout (called with the mirror lock
// held): with git-lfs installed it configures the mirror's LFS filter and
// fills in the LFS files whose content the mirror holds; it notes on log
// what stays a pointer file.
func (s *Store) checkoutLFS(ctx context.Context, repo, mirror, dst string, log io.Writer) error {
	if !usesLFS(dst) {
		return nil
	}
	note := func(format string, a ...any) {
		if log != nil {
			fmt.Fprintf(log, "longarmd: "+format+"\n", a...)
		}
	}
	if !s.lfsInstalled(ctx) {
		note("this repository uses Git LFS; git-lfs is not installed on this worker: LFS files are pointer files")
		return nil
	}
	if out, _ := s.run(ctx, mirror, nil, nil, "config", "--local", "--get", "filter.lfs.process"); out == "" {
		for _, kv := range lfsFilterConfig {
			if _, err := s.run(ctx, mirror, nil, nil, "config", "--local", kv[0], kv[1]); err != nil {
				return err
			}
		}
	}
	if !s.hasLFSStore(repo) {
		note("this repository uses Git LFS: LFS files are pointer files (their content was not shipped; longarm run --lfs ship sends it)")
		return nil
	}
	if _, err := s.run(ctx, dst, nil, log, "lfs", "checkout"); err != nil {
		return fmt.Errorf("git lfs checkout: %w", err)
	}
	note("Git LFS: files whose content was shipped are checked out; any other LFS file is a pointer file")
	return nil
}
