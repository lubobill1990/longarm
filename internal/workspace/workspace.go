// Package workspace is longarmd's code-sync module (DESIGN §3.1 "workspace",
// §5.3): one bare mirror per repository and one git worktree per sync
// target or job.
//
//	<data>/mirrors/<repo>.git     bare mirror; refs/longarm/* keep synced commits
//	<data>/worktrees/<job-id>     worktree of a workspace job
//	<data>/staging/<id>/          uncommitted changes uploaded for a job
//	                              (patch, untracked.tar) until it is prepared
//
// Objects arrive either as a git bundle from `longarm sync` / `longarm run --repo .`
// (only what the mirror lacks; the client learns the mirror's commits from
// Refs) or by fetching a URL (GitHub), authenticated with the job's GH_TOKEN
// secret passed through the environment - never on disk or the command line.
package workspace

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Layout under the data dir.
const (
	MirrorsDir   = "mirrors"
	WorktreesDir = "worktrees"
	StagingDir   = "staging"
	lastUsedFile = "longarm-last-used"
	patchFile    = "patch"
	untrackedTar = "untracked.tar"
)

var repoRE = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._-]{0,127}$`)

// ValidRepo reports whether s can be used as a mirror key.
func ValidRepo(s string) bool {
	return repoRE.MatchString(s) && !strings.Contains(s, "..") && !strings.HasSuffix(strings.ToLower(s), ".git")
}

var keyClean = regexp.MustCompile(`[^0-9A-Za-z._-]+`)

// RepoKey derives a mirror key from a remote URL ("https://github.com/o/r.git"
// or "git@github.com:o/r.git" -> "github.com_o_r") or a directory name.
func RepoKey(s string) string {
	s = strings.TrimSpace(s)
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		s = u.Host + "/" + strings.TrimPrefix(u.Path, "/")
	} else if at := strings.Index(s, "@"); at >= 0 && strings.Contains(s[at:], ":") { // scp-like
		s = strings.Replace(s[at+1:], ":", "/", 1)
	}
	s = strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
	s = strings.Trim(keyClean.ReplaceAllString(strings.ReplaceAll(s, "/", "_"), "_"), "_.-")
	for strings.Contains(s, "__") {
		s = strings.ReplaceAll(s, "__", "_")
	}
	for strings.Contains(s, "..") {
		s = strings.ReplaceAll(s, "..", ".")
	}
	if len(s) > 128 {
		s = s[len(s)-128:]
		s = strings.TrimLeft(s, "_.-")
	}
	if s == "" {
		s = "repo"
	}
	return s
}

// Store manages mirrors, worktrees and staging data under Root.
type Store struct {
	Root string // longarmd data dir
	Git  string // git executable (default "git")

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (s *Store) git() string {
	if s.Git != "" {
		return s.Git
	}
	return "git"
}

// lock serializes git operations on one mirror.
func (s *Store) lock(repo string) func() {
	s.mu.Lock()
	if s.locks == nil {
		s.locks = map[string]*sync.Mutex{}
	}
	l := s.locks[repo]
	if l == nil {
		l = &sync.Mutex{}
		s.locks[repo] = l
	}
	s.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// Mirror returns the bare mirror path of repo.
func (s *Store) Mirror(repo string) string {
	return filepath.Join(s.Root, MirrorsDir, repo+".git")
}

// JobWorktree returns the worktree path of a workspace job.
func (s *Store) JobWorktree(jobID string) string {
	return filepath.Join(s.Root, WorktreesDir, jobID)
}

// run runs git; output is appended to log (if any) and returned.
// core.autocrlf is pinned off (here and in the mirror's config): a worktree
// holds the commit's bytes (plus the client's patch) whatever the worker's
// git config says - Git for Windows installs with autocrlf=true system-wide,
// which turned every synced file into CRLF. .gitattributes eol rules still
// apply.
func (s *Store) run(ctx context.Context, dir string, env []string, log io.Writer, args ...string) (string, error) {
	args = append([]string{"-c", "core.autocrlf=false"}, args...)
	cmd := exec.CommandContext(ctx, s.git(), args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "LC_ALL=C"), env...)
	var out bytes.Buffer
	if log != nil {
		cmd.Stdout = io.MultiWriter(&out, log)
		cmd.Stderr = io.MultiWriter(&out, log)
	} else {
		cmd.Stdout, cmd.Stderr = &out, &out
	}
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(out.String())
		if len(msg) > 2000 {
			msg = msg[len(msg)-2000:]
		}
		return out.String(), fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, msg)
	}
	return out.String(), nil
}

func (s *Store) ensureMirror(ctx context.Context, repo string) (string, error) {
	if !ValidRepo(repo) {
		return "", fmt.Errorf("bad repo key %q", repo)
	}
	m := s.Mirror(repo)
	if _, err := os.Stat(filepath.Join(m, "HEAD")); err == nil {
		// Mirrors from before autocrlf was pinned get it too.
		if b, err := os.ReadFile(filepath.Join(m, "config")); err == nil && !bytes.Contains(b, []byte("autocrlf")) {
			s.run(ctx, m, nil, nil, "config", "core.autocrlf", "false")
		}
		touch(filepath.Join(m, lastUsedFile))
		return m, nil
	}
	if err := os.MkdirAll(m, 0o755); err != nil {
		return "", err
	}
	if _, err := s.run(ctx, m, nil, nil, "init", "--bare", "-q"); err != nil {
		return "", err
	}
	// Worktrees of jobs must not lose objects to auto-gc races, and the
	// mirror serves many worktrees: keep gc conservative.
	s.run(ctx, m, nil, nil, "config", "gc.pruneExpire", "2.weeks.ago")
	s.run(ctx, m, nil, nil, "config", "core.longpaths", "true")
	// For the job's own git in its worktrees too (they share this config).
	s.run(ctx, m, nil, nil, "config", "core.autocrlf", "false")
	touch(filepath.Join(m, lastUsedFile))
	return m, nil
}

func touch(p string) {
	now := time.Now()
	if err := os.Chtimes(p, now, now); err != nil {
		os.WriteFile(p, nil, 0o644)
	}
}

// Refs lists the commits at the mirror's ref tips (newest first, at most
// 256) so a client can bundle only what is missing.
func (s *Store) Refs(ctx context.Context, repo string) (bool, []string, error) {
	if !ValidRepo(repo) {
		return false, nil, fmt.Errorf("bad repo key %q", repo)
	}
	m := s.Mirror(repo)
	if _, err := os.Stat(filepath.Join(m, "HEAD")); err != nil {
		return false, nil, nil
	}
	unlock := s.lock(repo)
	defer unlock()
	out, err := s.run(ctx, m, nil, nil, "for-each-ref", "--sort=-committerdate", "--count=256",
		"--format=%(objectname) %(objecttype)", "refs/")
	if err != nil {
		return true, nil, err
	}
	seen := map[string]bool{}
	var commits []string
	for _, l := range strings.Split(out, "\n") {
		sha, typ, _ := strings.Cut(strings.TrimSpace(l), " ")
		if typ == "commit" && !seen[sha] {
			seen[sha] = true
			commits = append(commits, sha)
		}
	}
	return true, commits, nil
}

var shaRE = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func refSuffix(s string) string {
	s = strings.Trim(keyClean.ReplaceAllString(s, "_"), "_.-")
	if s == "" {
		return "head"
	}
	return s
}

// ImportBundle fetches the (single) ref of a bundle into the mirror under
// refs/longarm/sync/<name> and checks that commit is present afterwards. An empty
// bundle path only checks for commit.
func (s *Store) ImportBundle(ctx context.Context, repo, bundle, name, commit string) error {
	if !shaRE.MatchString(commit) {
		return fmt.Errorf("bad commit %q", commit)
	}
	m, err := s.ensureMirror(ctx, repo)
	if err != nil {
		return err
	}
	unlock := s.lock(repo)
	defer unlock()
	if bundle != "" {
		heads, err := s.run(ctx, m, nil, nil, "bundle", "list-heads", bundle)
		if err != nil {
			return err
		}
		var src string
		for _, l := range strings.Split(heads, "\n") {
			if _, ref, ok := strings.Cut(strings.TrimSpace(l), " "); ok {
				src = ref
				break
			}
		}
		if src == "" {
			return errors.New("bundle has no refs")
		}
		if _, err := s.run(ctx, m, nil, nil, "fetch", "-q", "--no-tags", "--force", bundle,
			"+"+src+":refs/longarm/sync/"+refSuffix(name)); err != nil {
			return err
		}
	}
	if _, err := s.run(ctx, m, nil, nil, "cat-file", "-e", commit+"^{commit}"); err != nil {
		return fmt.Errorf("commit %s is not in the mirror (send a bundle)", commit)
	}
	// Keep the exact commit reachable even when the branch moves on.
	_, err = s.run(ctx, m, nil, nil, "update-ref", "refs/longarm/commits/"+commit, commit)
	return err
}

// FetchURL fetches ref (default HEAD) from u into the mirror and returns the
// commit. For http(s) URLs a non-empty token is sent as basic auth
// ("x-access-token", GitHub style) to that host only, via GIT_CONFIG_*
// environment variables.
func (s *Store) FetchURL(ctx context.Context, repo, u, ref, token string, log io.Writer) (string, error) {
	m, err := s.ensureMirror(ctx, repo)
	if err != nil {
		return "", err
	}
	if ref == "" {
		ref = "HEAD"
	}
	if strings.HasPrefix(ref, "-") || strings.ContainsAny(ref, " \t\n:") {
		return "", fmt.Errorf("bad ref %q", ref)
	}
	var env []string
	if token != "" {
		pu, err := url.Parse(u)
		if err != nil || (pu.Scheme != "https" && pu.Scheme != "http") || pu.Host == "" {
			return "", fmt.Errorf("GH_TOKEN given but %q is not an http(s) url", u)
		}
		cred := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		env = []string{
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http." + pu.Scheme + "://" + pu.Host + "/.extraheader",
			"GIT_CONFIG_VALUE_0=AUTHORIZATION: basic " + cred,
		}
	}
	unlock := s.lock(repo)
	defer unlock()
	dst := "refs/longarm/remote/" + refSuffix(ref)
	if _, err := s.run(ctx, m, env, log, "fetch", "--no-tags", "--force", "--", u, "+"+ref+":"+dst); err != nil {
		return "", err
	}
	out, err := s.run(ctx, m, nil, nil, "rev-parse", "--verify", dst+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Checkout makes path a worktree of repo's mirror at commit (detached),
// then applies patch (git diff --binary) and extracts untracked (a tar),
// either of which may be "". An existing worktree of the same mirror is
// reset (tracked changes and untracked files are discarded; ignored files
// such as node_modules are kept); any other existing non-empty directory is
// refused.
func (s *Store) Checkout(ctx context.Context, repo, commit, dst, patch, untracked string, log io.Writer) error {
	m, err := s.ensureMirror(ctx, repo)
	if err != nil {
		return err
	}
	unlock := s.lock(repo)
	defer unlock()
	dst, err = filepath.Abs(dst)
	if err != nil {
		return err
	}
	if empty, err := isEmptyDir(dst); err != nil {
		return err
	} else if empty {
		os.Remove(dst)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		s.run(ctx, m, nil, nil, "worktree", "prune")
		if _, err := s.run(ctx, m, nil, log, "worktree", "add", "-q", "--force", "--detach", dst, commit); err != nil {
			return err
		}
	} else {
		common, err := s.run(ctx, dst, nil, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil || !samePath(strings.TrimSpace(common), m) {
			return fmt.Errorf("%s exists and is not a worktree of mirror %s", dst, repo)
		}
		if _, err := s.run(ctx, dst, nil, log, "checkout", "-q", "-f", "--detach", commit); err != nil {
			return err
		}
		if _, err := s.run(ctx, dst, nil, log, "clean", "-q", "-fd"); err != nil {
			return err
		}
	}
	if patch != "" {
		if fi, err := os.Stat(patch); err == nil && fi.Size() > 0 {
			if _, err := s.run(ctx, dst, nil, log, "apply", "--binary", "--whitespace=nowarn", patch); err != nil {
				return err
			}
		}
	}
	if untracked != "" {
		if err := ExtractTar(untracked, dst); err != nil {
			return fmt.Errorf("untracked files: %w", err)
		}
	}
	return nil
}

func samePath(a, b string) bool {
	fa, err1 := os.Stat(a)
	fb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(fa, fb)
}

func isEmptyDir(p string) (bool, error) {
	ents, err := os.ReadDir(p)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return len(ents) == 0, nil
}

// RemoveWorktree deletes a worktree directory and prunes its registration
// from every mirror.
func (s *Store) RemoveWorktree(ctx context.Context, dir string) error {
	if err := removeAll(dir); err != nil {
		return err
	}
	s.PruneAll(ctx)
	return nil
}

// PruneAll runs `git worktree prune` in every mirror.
func (s *Store) PruneAll(ctx context.Context) {
	ents, _ := os.ReadDir(filepath.Join(s.Root, MirrorsDir))
	for _, e := range ents {
		if e.IsDir() && strings.HasSuffix(e.Name(), ".git") {
			repo := strings.TrimSuffix(e.Name(), ".git")
			unlock := s.lock(repo)
			s.run(ctx, filepath.Join(s.Root, MirrorsDir, e.Name()), nil, nil, "worktree", "prune")
			unlock()
		}
	}
}

func removeAll(p string) error {
	var err error
	for i := 0; i < 20; i++ {
		if err = os.RemoveAll(p); err == nil {
			return nil
		}
		makeWritable(p) // git marks pack files read-only on Windows
		time.Sleep(50 * time.Millisecond)
	}
	return err
}

func makeWritable(root string) {
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			os.Chmod(p, 0o644)
		}
		return nil
	})
}

// MirrorLastUsed returns when a mirror was last synced/fetched.
func (s *Store) MirrorLastUsed(repo string) time.Time {
	if fi, err := os.Stat(filepath.Join(s.Mirror(repo), lastUsedFile)); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}

// RemoveMirror deletes a mirror (GC).
func (s *Store) RemoveMirror(repo string) error {
	unlock := s.lock(repo)
	defer unlock()
	return removeAll(s.Mirror(repo))
}

// ---------------------------------------------------------------- staging

// StagingPath returns the staging directory of id.
func (s *Store) StagingPath(id string) string { return filepath.Join(s.Root, StagingDir, id) }

// StagedFiles returns the patch and tar paths of a staging id ("" if absent).
func (s *Store) StagedFiles(id string) (patch, untracked string) {
	d := s.StagingPath(id)
	if _, err := os.Stat(filepath.Join(d, patchFile)); err == nil {
		patch = filepath.Join(d, patchFile)
	}
	if _, err := os.Stat(filepath.Join(d, untrackedTar)); err == nil {
		untracked = filepath.Join(d, untrackedTar)
	}
	return patch, untracked
}

// Stage moves the uploaded patch / tar (either may be "") into staging/<id>.
func (s *Store) Stage(id, patch, untracked string) error {
	d := s.StagingPath(id)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}
	for src, name := range map[string]string{patch: patchFile, untracked: untrackedTar} {
		if src == "" {
			continue
		}
		if err := os.Rename(src, filepath.Join(d, name)); err != nil {
			return err
		}
	}
	return nil
}

// RemoveStaging deletes staging/<id>.
func (s *Store) RemoveStaging(id string) error { return removeAll(s.StagingPath(id)) }

// ---------------------------------------------------------------- tar

// ExtractTar extracts regular files and directories from the tar at src into
// dst. Entries must be relative and stay inside dst; links are refused.
func ExtractTar(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(strings.ReplaceAll(h.Name, `\`, "/"))
		if name == "." || path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") ||
			strings.HasPrefix(name, ".git/") || name == ".git" {
			return fmt.Errorf("unsafe path %q in tar", h.Name)
		}
		target := filepath.Join(dst, filepath.FromSlash(name))
		if rel, err := filepath.Rel(dst, target); err != nil || filepath.VolumeName(name) != "" || (runtime.GOOS == "windows" && strings.Contains(name, ":")) || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			return fmt.Errorf("unsafe path %q in tar", h.Name)
		}
		if err := noSymlinks(dst, name); err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(h.Mode).Perm() | 0o600
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, tr)
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
			os.Chtimes(target, h.ModTime, h.ModTime)
		default:
			return fmt.Errorf("unsupported tar entry %q (type %c)", h.Name, h.Typeflag)
		}
	}
}

// MirrorInUse reports whether a registered worktree of repo still exists
// (e.g. an `longarm sync` target outside the data dir); GC must keep such
// mirrors, the worktrees' .git files point into them.
func (s *Store) MirrorInUse(repo string) bool {
	dir := filepath.Join(s.Mirror(repo), "worktrees")
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(dir, e.Name(), "gitdir"))
		if err != nil {
			continue
		}
		if _, err := os.Stat(strings.TrimSpace(string(b))); err == nil {
			return true
		}
	}
	return false
}

// noSymlinks fails if any existing component of name below dst (including
// the leaf) is a symlink, so extraction cannot be redirected outside dst.
func noSymlinks(dst, name string) error {
	cur := dst
	for _, part := range strings.Split(name, "/") {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to extract %q through symlink %s", name, cur)
		}
	}
	return nil
}
