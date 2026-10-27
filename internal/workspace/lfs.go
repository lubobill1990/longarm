package workspace

import (
	"archive/tar"
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
	"strings"
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

// LFSLimits bound what one ImportLFS accepts (zero fields: the defaults).
// Every object's size is known from its tar header before anything is
// written, so oversized uploads fail before they cost disk or CPU.
type LFSLimits struct {
	MaxObjectBytes int64 // one object
	MaxTotalBytes  int64 // all objects of one upload
	MaxObjects     int   // objects of one upload
}

// DefaultLFSLimits: 4 GiB per object, 16 GiB and 50000 objects per upload.
var DefaultLFSLimits = LFSLimits{MaxObjectBytes: 4 << 30, MaxTotalBytes: 16 << 30, MaxObjects: 50000}

func (s *Store) lfsLimits() LFSLimits {
	l := s.LFSLimits
	if l.MaxObjectBytes <= 0 {
		l.MaxObjectBytes = DefaultLFSLimits.MaxObjectBytes
	}
	if l.MaxTotalBytes <= 0 {
		l.MaxTotalBytes = DefaultLFSLimits.MaxTotalBytes
	}
	if l.MaxObjects <= 0 {
		l.MaxObjects = DefaultLFSLimits.MaxObjects
	}
	return l
}

// MaxLFSUploadBytes caps the uploaded LFS tar (Workspace.Sync): the
// objects' limit plus room for tar headers and padding.
func (s *Store) MaxLFSUploadBytes() int64 {
	l := s.lfsLimits()
	return l.MaxTotalBytes + int64(l.MaxObjects)*4096 + 1<<20
}

// ctxReader stops reading once ctx ends.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// ImportLFS stores the objects of a tar (regular entries named by their
// oid) in the mirror's LFS store; every object's content must hash to its
// name. Sparse and other non-regular entries are refused, sizes and counts
// are bounded (LFSLimits), ctx cancels. Objects are verified in a staging
// dir without the mirror's lock, which is held only to move them in. It
// returns how many objects were added.
func (s *Store) ImportLFS(ctx context.Context, repo, tarPath string) (int, error) {
	m, err := s.ensureMirror(ctx, repo)
	if err != nil {
		return 0, err
	}
	lim := s.lfsLimits()
	f, err := os.Open(tarPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if err := os.MkdirAll(filepath.Join(m, "lfs", "tmp"), 0o755); err != nil {
		return 0, err
	}
	stage, err := os.MkdirTemp(filepath.Join(m, "lfs", "tmp"), "import-")
	if err != nil {
		return 0, err
	}
	defer removeAll(stage)
	var verified []string
	var total int64
	count := 0
	tr := tar.NewReader(f)
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("lfs tar: %w", err)
		}
		if err := checkLFSEntry(h); err != nil {
			return 0, err
		}
		if count++; count > lim.MaxObjects {
			return 0, fmt.Errorf("lfs tar: more than %d objects (the worker's limit)", lim.MaxObjects)
		}
		if h.Size > lim.MaxObjectBytes {
			return 0, fmt.Errorf("lfs object %s: %d bytes, more than the worker's limit of %d", h.Name, h.Size, lim.MaxObjectBytes)
		}
		if total += h.Size; total > lim.MaxTotalBytes {
			return 0, fmt.Errorf("lfs objects: more than the worker's limit of %d bytes per upload", lim.MaxTotalBytes)
		}
		if _, err := os.Stat(s.lfsObject(repo, h.Name)); err == nil {
			continue
		}
		if err := copyLFSObject(ctx, filepath.Join(stage, h.Name), h, tr); err != nil {
			return 0, err
		}
		verified = append(verified, h.Name)
	}
	unlock := s.lock(repo)
	defer unlock()
	n := 0
	for _, oid := range verified {
		dst := s.lfsObject(repo, oid)
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return n, err
		}
		if err := os.Rename(filepath.Join(stage, oid), dst); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// checkLFSEntry accepts plain regular files named by an oid only: no
// sparse files (a small upload expanding to a huge object), links, dirs.
func checkLFSEntry(h *tar.Header) error {
	if h.Typeflag != tar.TypeReg || !ValidLFSOid(h.Name) || h.Size < 0 {
		return fmt.Errorf("lfs tar: unexpected entry %q (type %q)", h.Name, h.Typeflag)
	}
	for k := range h.PAXRecords {
		if strings.HasPrefix(k, "GNU.sparse.") {
			return fmt.Errorf("lfs tar: sparse entry %q refused", h.Name)
		}
	}
	return nil
}

// copyLFSObject writes exactly h.Size bytes of r to dst and checks that
// they hash to h.Name.
func copyLFSObject(ctx context.Context, dst string, h *tar.Header, r io.Reader) error {
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, hash), ctxReader{ctx, io.LimitReader(r, h.Size)})
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != h.Size {
		err = fmt.Errorf("lfs object %s: truncated", h.Name)
	}
	if err == nil && hex.EncodeToString(hash.Sum(nil)) != h.Name {
		err = fmt.Errorf("lfs object %s: content does not match its oid", h.Name)
	}
	return err
}

// lfsInstalled reports whether git-lfs is installed here.
func (s *Store) lfsInstalled(ctx context.Context) bool {
	s.lfsOnce.Do(func() {
		s.lfsOK = exec.CommandContext(ctx, s.git(), "lfs", "version").Run() == nil
	})
	return s.lfsOK
}

// usesLFS reports whether any tracked .gitattributes of the worktree (the
// root's or a nested one) routes files through the LFS filter.
func (s *Store) usesLFS(ctx context.Context, dir string) bool {
	out, _ := s.run(ctx, dir, nil, nil, "grep", "-l", "-I", "-F", "-e", "filter=lfs", "--", lfsAttrPathspec)
	return strings.TrimSpace(out) != ""
}

// lfsAttrPathspec matches every .gitattributes, the root's included.
const lfsAttrPathspec = ":(glob)**/.gitattributes"

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
	shipped := s.hasLFSStore(repo)
	if !shipped && !s.usesLFS(ctx, dst) {
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
	if !shipped {
		note("this repository uses Git LFS: LFS files are pointer files (their content was not shipped; longarm run --lfs ship sends it)")
		return nil
	}
	if _, err := s.run(ctx, dst, nil, log, "lfs", "checkout"); err != nil {
		return fmt.Errorf("git lfs checkout: %w", err)
	}
	note("Git LFS: files whose content was shipped are checked out; any other LFS file is a pointer file")
	return nil
}
