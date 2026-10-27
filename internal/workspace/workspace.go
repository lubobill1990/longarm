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

	"github.com/lubobill1990/longarm/internal/jobs"
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
	// LFSLimits bound LFS uploads (zero: DefaultLFSLimits).
	LFSLimits LFSLimits
	// Prune replaces PruneAll's git runs (per-user isolation: the user's
	// mirrors are pruned as the user).
	Prune func(ctx context.Context)
	// PrivateMirrors: the mirrors belong to one user (per-user isolation).
	// Otherwise (shared isolation) a commit fetched for one user's job is
	// there for every user's: a job that would use such a commit without
	// fetching first proves its own access to the remote (proveAccess).
	PrivateMirrors bool

	mu    sync.Mutex
	locks map[string]*sync.Mutex

	lfsOnce sync.Once // lfsInstalled
	lfsOK   bool
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
	return s.runAuth(ctx, dir, env, nil, log, args...)
}

// runAuth is run with the environment of fa (an authenticated fetch) and
// its credential masked in everything it returns or logs.
func (s *Store) runAuth(ctx context.Context, dir string, env []string, fa *fetchAuth, log io.Writer, args ...string) (string, error) {
	args = append([]string{"-c", "core.autocrlf=false"}, args...)
	cmd := exec.CommandContext(ctx, s.git(), args...)
	cmd.Dir = dir
	// Never wait for a prompt nobody sees: git's own, an askpass helper's,
	// or Git Credential Manager's sign-in window.
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "GCM_INTERACTIVE=never", "LC_ALL=C"), env...)
	cmd.Env = append(cmd.Env, fa.env(cmd.Env)...)
	// One writer for both streams: exec then never calls it concurrently.
	// What goes to log (a job's out.log) has URL credentials masked.
	var out bytes.Buffer
	var rlog *redactWriter
	var w io.Writer = &out
	if log != nil {
		rlog = &redactWriter{w: log, fa: fa}
		w = io.MultiWriter(&out, rlog)
	}
	cmd.Stdout, cmd.Stderr = w, w
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	if rlog != nil {
		rlog.flush()
	}
	if err != nil {
		msg := strings.TrimSpace(out.String())
		if len(msg) > 2000 {
			msg = msg[len(msg)-2000:]
		}
		return fa.redact(out.String()), fmt.Errorf("git %s: %v: %s", RedactURLs(strings.Join(args, " ")), err, RedactURLs(fa.redact(msg)))
	}
	return fa.redact(out.String()), nil
}

// urlUserinfo matches the userinfo of a URL ("https://user:pass@",
// "https://token@").
var urlUserinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^\s/?#@'"]+@`)

// RedactURLs masks the userinfo (user and password: a PAT is often the
// user) of every URL in s, and the value of every Authorization header.
func RedactURLs(s string) string {
	return authHeader.ReplaceAllString(urlUserinfo.ReplaceAllString(s, "${1}xxxxx@"), "${1}xxxxx")
}

// redactWriter passes lines through RedactURLs (and masks the credential
// of fa, if any).
type redactWriter struct {
	w   io.Writer
	fa  *fetchAuth
	buf []byte
}

func (r *redactWriter) Write(p []byte) (int, error) {
	r.buf = append(r.buf, p...)
	for {
		i := bytes.IndexAny(r.buf, "\r\n")
		if i < 0 {
			break
		}
		r.w.Write([]byte(RedactURLs(r.fa.redact(string(r.buf[:i+1])))))
		r.buf = r.buf[i+1:]
	}
	if len(r.buf) > 64<<10 { // no line end in sight
		r.flush()
	}
	return len(p), nil
}

func (r *redactWriter) flush() {
	if len(r.buf) > 0 {
		r.w.Write([]byte(RedactURLs(r.fa.redact(string(r.buf)))))
		r.buf = nil
	}
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
// commit. For http(s) URLs a non-empty token (the job's GH_TOKEN secret) is
// sent as basic auth ("x-access-token", GitHub style) to that host only,
// via GIT_CONFIG_* environment variables.
func (s *Store) FetchURL(ctx context.Context, repo, u, ref, token string, log io.Writer) (string, error) {
	return s.FetchURLAuth(ctx, repo, u, ref, nil, token, log)
}

// FetchURLAuth is FetchURL with a forwarded credential (preferred over
// token; https only, sent to u only - gitauth.go). A full commit id as ref
// is not fetched when the mirror has it already; otherwise it is fetched
// by id (fetchCommits).
func (s *Store) FetchURLAuth(ctx context.Context, repo, u, ref string, auth *jobs.GitAuth, token string, log io.Writer) (string, error) {
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
	fa, err := newFetchAuth(u, auth, token)
	if err != nil {
		return "", err
	}
	unlock := s.lock(repo)
	defer unlock()
	if shaRE.MatchString(ref) {
		if _, err := s.run(ctx, m, nil, nil, "cat-file", "-e", ref+"^{commit}"); err == nil {
			if err := s.proveAccess(ctx, m, u, fa, log); err != nil {
				return "", fetchError(u, fa.kind, err)
			}
			logf(log, "commit %s is in the mirror already: not fetching", ref)
			return ref, nil
		}
		if err := s.fetchCommits(ctx, m, u, []string{ref}, fa, log); err != nil {
			return "", fetchError(u, fa.kind, err)
		}
		return ref, nil
	}
	dst := "refs/longarm/remote/" + refSuffix(ref)
	if _, err := s.runAuth(ctx, m, nil, fa, log, "fetch", "--no-tags", "--force", "--", u, "+"+ref+":"+dst); err != nil {
		return "", fetchError(u, fa.kind, err)
	}
	out, err := s.run(ctx, m, nil, nil, "rev-parse", "--verify", dst+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// proveAccess checks, when the mirror is shared between users, that the
// caller's credential (forwarded, GH_TOKEN, or else the worker's own, as a
// fetch would use) reads the remote u: a commit in the mirror may have come
// with another user's credential, and using it unchecked would skip the
// remote's authorization. One ls-remote of HEAD (protocol v2: the server
// lists that ref only), no objects. Called with the mirror lock held.
func (s *Store) proveAccess(ctx context.Context, m, u string, fa *fetchAuth, log io.Writer) error {
	if s.PrivateMirrors {
		return nil
	}
	if strings.HasPrefix(u, "-") {
		return fmt.Errorf("bad url %q", RedactURLs(u))
	}
	_, err := s.runAuth(ctx, m, nil, fa, log, "ls-remote", u, "HEAD")
	return err
}

// maxFetchCommits caps the commits of one FetchCommits.
const maxFetchCommits = 64

// FetchCommits fetches those of commits (full ids) the mirror lacks from u
// with a forwarded credential and returns the ones the mirror has
// afterwards (also on error, which is then about the fetch).
func (s *Store) FetchCommits(ctx context.Context, repo, u string, commits []string, auth *jobs.GitAuth, log io.Writer) ([]string, error) {
	if len(commits) == 0 || len(commits) > maxFetchCommits {
		return nil, fmt.Errorf("want 1 to %d commits, got %d", maxFetchCommits, len(commits))
	}
	for _, c := range commits {
		if !shaRE.MatchString(c) {
			return nil, fmt.Errorf("bad commit %q", c)
		}
	}
	m, err := s.ensureMirror(ctx, repo)
	if err != nil {
		return nil, err
	}
	fa, err := newFetchAuth(u, auth, "")
	if err != nil {
		return nil, err
	}
	unlock := s.lock(repo)
	defer unlock()
	have := func() (present, missing []string) {
		for _, c := range commits {
			if _, err := s.run(ctx, m, nil, nil, "cat-file", "-e", c+"^{commit}"); err == nil {
				present = append(present, c)
			} else {
				missing = append(missing, c)
			}
		}
		return present, missing
	}
	present, missing := have()
	if len(missing) == 0 {
		if err := s.proveAccess(ctx, m, u, fa, log); err != nil {
			return nil, fetchError(u, fa.kind, err)
		}
		return present, nil
	}
	ferr := s.fetchCommits(ctx, m, u, missing, fa, log)
	present, _ = have()
	if ferr != nil {
		return present, fetchError(u, fa.kind, ferr)
	}
	return present, nil
}

// fetchCommits fetches commits by id into refs/longarm/remote/<id> (with
// protocol v2 GitHub, Azure DevOps and git http-backend serve any commit
// reachable from their refs). When the server refuses that for another
// reason than authentication, it fetches the remote's branches instead,
// which must contain the commits. Called with the mirror lock held.
func (s *Store) fetchCommits(ctx context.Context, m, u string, commits []string, fa *fetchAuth, log io.Writer) error {
	args := []string{"fetch", "--no-tags", "--force", "--", u}
	for _, c := range commits {
		args = append(args, "+"+c+":refs/longarm/remote/"+c)
	}
	_, err := s.runAuth(ctx, m, nil, fa, log, args...)
	if err == nil {
		return nil
	}
	if authFailure.MatchString(err.Error()) {
		return err
	}
	logf(log, "fetching by commit id failed (%v); fetching the remote's branches", firstLine(err.Error()))
	if _, err := s.runAuth(ctx, m, nil, fa, log, "fetch", "--no-tags", "--force", "--", u,
		"+refs/heads/*:refs/longarm/remote/heads/*"); err != nil {
		return err
	}
	for _, c := range commits {
		if _, err := s.run(ctx, m, nil, nil, "cat-file", "-e", c+"^{commit}"); err != nil {
			return fmt.Errorf("commit %s is not on %s (not reachable from its branches)", c, RedactURLs(u))
		}
		s.run(ctx, m, nil, nil, "update-ref", "refs/longarm/remote/"+c, c)
	}
	return nil
}

// FetchLFS fetches the Git LFS objects of commit from u (git lfs fetch,
// authenticated like FetchURLAuth) into the mirror's LFS store, from where
// Checkout fills them in.
func (s *Store) FetchLFS(ctx context.Context, repo, u, commit string, auth *jobs.GitAuth, token string, log io.Writer) error {
	if !shaRE.MatchString(commit) {
		return fmt.Errorf("bad commit %q", commit)
	}
	if !s.lfsInstalled(ctx) {
		logf(log, "--lfs fetch: git-lfs is not installed on this worker: LFS files are pointer files")
		return nil
	}
	m, err := s.ensureMirror(ctx, repo)
	if err != nil {
		return err
	}
	fa, err := newFetchAuth(u, auth, token)
	if err != nil {
		return err
	}
	unlock := s.lock(repo)
	defer unlock()
	if _, err := s.runAuth(ctx, m, nil, fa, log, "lfs", "fetch", u, commit); err != nil {
		return fetchError(u, fa.kind, fmt.Errorf("git lfs fetch: %w", err))
	}
	return nil
}

func logf(log io.Writer, format string, a ...any) {
	if log != nil {
		fmt.Fprintf(log, "longarmd: "+format+"\n", a...)
	}
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

// authFailure matches git's (and its credential helpers') ways of saying
// the remote refused or never got credentials.
var authFailure = regexp.MustCompile(`(?i)(returned error: 40[13]|HTTP 40[13]|\b40[13] (Forbidden|Unauthorized)|Authentication failed|` +
	`could not read (Username|Password)|terminal prompts disabled|Permission denied \(publickey|` +
	`Repository not found|cannot prompt|interactivity is disabled)`)

// fetchError explains a failed fetch of u that looks like an
// authentication problem: the worker fetches as its own account,
// non-interactively - a credential helper that needs a sign-in (Git
// Credential Manager for Azure DevOps) can't ask anyone.
func fetchError(u string, kind authKind, err error) error {
	u = RedactURLs(u)
	m := authFailure.FindString(err.Error())
	if m == "" {
		return errors.New(RedactURLs(err.Error()))
	}
	host := u
	if pu, perr := url.Parse(u); perr == nil && pu.Host != "" {
		host = pu.Host
	}
	how := "a credential that works without a prompt for " + host + " in the worker account's git config " +
		"(a PAT in its credential store, an ssh key; GitHub: the job's GH_TOKEN secret, --secret-env GH_TOKEN)"
	switch kind {
	case authGHToken:
		how = "a GH_TOKEN with access to the repository (the job's secret was refused)"
	case authForwarded:
		return fmt.Errorf("fetching %s: the forwarded credential was refused (%s): it may have expired or lack access "+
			"to the repository (Azure DevOps: a token of the organization's tenant; longarm gets one with az, else git "+
			"credential fill). Ship the commits from your machine instead - longarm run --repo <local checkout> - or "+
			"check the credential with git ls-remote %s: %s", u, m, u, RedactURLs(err.Error()))
	}
	return fmt.Errorf("fetching %s: the worker could not authenticate (%s): it fetches as its own account and "+
		"can't prompt for a sign-in (e.g. Git Credential Manager). Forward your credential for this fetch - "+
		"longarm run --git-auth forward - or ship the commits from your machine - longarm run --repo <local checkout> "+
		"(e.g. --repo .) - or give the worker %s: %s", u, m, how, RedactURLs(err.Error()))
}

// Checkout makes path a worktree of repo's mirror at commit (detached;
// Git LFS files as pointers unless their content was shipped, lfs.go),
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
		if _, err := s.run(ctx, m, skipSmudge, log, "worktree", "add", "-q", "--force", "--detach", dst, commit); err != nil {
			return err
		}
	} else {
		common, err := s.run(ctx, dst, nil, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil || !samePath(strings.TrimSpace(common), m) {
			return fmt.Errorf("%s exists and is not a worktree of mirror %s", dst, repo)
		}
		if _, err := s.run(ctx, dst, skipSmudge, log, "checkout", "-q", "-f", "--detach", commit); err != nil {
			return err
		}
		if _, err := s.run(ctx, dst, nil, log, "clean", "-q", "-fd"); err != nil {
			return err
		}
	}
	if err := s.checkoutLFS(ctx, repo, m, dst, log); err != nil {
		return err
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
	if s.Prune != nil {
		s.Prune(ctx)
		return
	}
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
