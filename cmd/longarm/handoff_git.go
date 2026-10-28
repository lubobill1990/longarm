package main

// The code side of longarm handoff: a snapshot commit of the source
// worktree (HEAD + uncommitted + untracked, .gitignore respected) made
// with a temporary index - the user's branch, index and working tree are
// never touched - and the target worktree it becomes (with the changes
// uncommitted again on a handoff/<id> branch).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/gitauth"
	"github.com/lubobill1990/longarm/internal/workspace"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// hoRepo is a git checkout on an endpoint.
type hoRepo struct {
	ep     *hoEndpoint
	top    string // the worktree's top directory (native separators)
	prefix string // of the directory it was opened at, '/'-separated, "" or ending in "/"
	key    string // longarm's repo key (the worker's mirror name)
	remote string // name of the remote of HEAD's branch (or origin); "" none
	url    string // its URL
}

const gitTimeout = 15 * time.Minute

// hoGitEnv keeps git from prompting and names the snapshot's author.
var hoGitEnv = map[string]string{"GIT_TERMINAL_PROMPT": "0", "GCM_INTERACTIVE": "never",
	"GIT_AUTHOR_NAME": "longarm handoff", "GIT_AUTHOR_EMAIL": "longarm-handoff@localhost",
	"GIT_COMMITTER_NAME": "longarm handoff", "GIT_COMMITTER_EMAIL": "longarm-handoff@localhost"}

func (r *hoRepo) git(ctx context.Context, env map[string]string, stdin []byte, args ...string) (string, error) {
	e := map[string]string{}
	for k, v := range hoGitEnv {
		e[k] = v
	}
	for k, v := range env {
		e[k] = v
	}
	out, err := r.ep.run(ctx, r.top, e, stdin, gitTimeout, append([]string{"git"}, args...)...)
	return strings.TrimSpace(string(out)), err
}

// openHoRepo opens the checkout that contains dir on ep.
func openHoRepo(ctx context.Context, ep *hoEndpoint, dir string) (*hoRepo, error) {
	r := &hoRepo{ep: ep, top: dir}
	out, err := r.git(ctx, nil, nil, "rev-parse", "--show-toplevel", "--show-prefix")
	if errors.Is(err, errNoProgram) {
		return nil, usagef("git is not installed on %s", ep.name)
	}
	if err != nil {
		return nil, usagef("%s on %s is not inside a git checkout (handoff moves the code with git): %v", dir, ep.name, err)
	}
	ls := strings.Split(out, "\n")
	r.top = ep.native(strings.TrimSpace(ls[0]))
	if len(ls) > 1 {
		r.prefix = strings.TrimSpace(ls[1])
	}
	if b, err := r.git(ctx, nil, nil, "symbolic-ref", "-q", "--short", "HEAD"); err == nil && b != "" {
		if rm, err := r.git(ctx, nil, nil, "config", "--get", "branch."+b+".remote"); err == nil && rm != "" && rm != "." {
			r.remote = rm
		}
	}
	if r.remote == "" {
		r.remote = "origin"
	}
	if u, err := r.git(ctx, nil, nil, "remote", "get-url", r.remote); err == nil && u != "" {
		r.url = u
	} else {
		r.remote = ""
	}
	// a worktree of a longarm mirror (<data>/mirrors/<key>.git) keeps the mirror's name
	if common, err := r.git(ctx, nil, nil, "rev-parse", "--path-format=absolute", "--git-common-dir"); err == nil {
		c := strings.ReplaceAll(common, `\`, "/")
		if parent, base := pathSplit(c); strings.HasSuffix(parent, "/mirrors") && strings.HasSuffix(base, ".git") && workspace.ValidRepo(strings.TrimSuffix(base, ".git")) {
			r.key = strings.TrimSuffix(base, ".git")
		}
	}
	if r.key == "" {
		if o, err := r.git(ctx, nil, nil, "remote", "get-url", "origin"); err == nil && o != "" {
			r.key = workspace.RepoKey(o)
		} else {
			_, base := pathSplit(strings.ReplaceAll(r.top, `\`, "/"))
			r.key = workspace.RepoKey(base)
		}
	}
	return r, nil
}

func pathSplit(p string) (dir, base string) {
	p = strings.TrimRight(p, "/")
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return "", p
	}
	return p[:i], p[i+1:]
}

// gitDir is the checkout's (worktree's) own git directory.
func (r *hoRepo) gitDir(ctx context.Context) (string, error) {
	d, err := r.git(ctx, nil, nil, "rev-parse", "--absolute-git-dir")
	return r.ep.native(d), err
}

// snapshot commits HEAD + uncommitted + untracked (not ignored) files of
// the worktree, through a temporary index, as a child of HEAD under
// ref (refs/longarm/handoff/<hid>). The user's index, branch and files
// stay as they are.
func (r *hoRepo) snapshot(ctx context.Context, ref string) (snap, base string, err error) {
	base, err = r.git(ctx, nil, nil, "rev-parse", "--verify", "-q", "HEAD^{commit}")
	if err != nil || base == "" {
		return "", "", usagef("%s on %s has no commit yet: commit once before handing off", r.top, r.ep.name)
	}
	gd, err := r.gitDir(ctx)
	if err != nil {
		return "", "", err
	}
	idx := gd + r.ep.sep() + "longarm-handoff-" + strings.ReplaceAll(strings.TrimPrefix(ref, "refs/longarm/handoff/"), "/", "-") + ".index"
	defer r.ep.removeFile(context.WithoutCancel(ctx), idx)
	env := map[string]string{"GIT_INDEX_FILE": idx}
	if _, err := r.git(ctx, env, nil, "read-tree", "HEAD"); err != nil {
		return "", "", err
	}
	if _, err := r.git(ctx, env, nil, "add", "-A"); err != nil {
		return "", "", err
	}
	tree, err := r.git(ctx, env, nil, "write-tree")
	if err != nil {
		return "", "", err
	}
	snap, err = r.git(ctx, nil, nil, "commit-tree", tree, "-p", base, "-m", "longarm handoff snapshot (HEAD + uncommitted + untracked)")
	if err != nil {
		return "", "", err
	}
	if _, err := r.git(ctx, nil, nil, "update-ref", ref, snap); err != nil {
		return "", "", err
	}
	return snap, base, nil
}

// existing filters commits to those the checkout has.
func (r *hoRepo) existing(ctx context.Context, commits []string) []string {
	if len(commits) == 0 {
		return nil
	}
	out, err := r.git(ctx, nil, []byte(strings.Join(commits, "\n")+"\n"), "cat-file", "--batch-check=%(objectname) %(objecttype)")
	if err != nil {
		return nil
	}
	var have []string
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) == 2 && f[1] == "commit" {
			have = append(have, f[0])
		}
	}
	return have
}

// tips are the commits at the checkout's branch, remote-tracking and tag
// tips (newest first), for the --not side of a bundle made elsewhere.
func (r *hoRepo) tips(ctx context.Context) []string {
	out, err := r.git(ctx, nil, nil, "for-each-ref", "--sort=-committerdate", "--count=200", "--format=%(objectname) %(objecttype)",
		"refs/heads", "refs/remotes", "refs/tags", "refs/longarm")
	if err != nil {
		return nil
	}
	var tips []string
	seen := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) == 2 && f[1] == "commit" && !seen[f[0]] {
			seen[f[0]] = true
			tips = append(tips, f[0])
		}
	}
	return tips
}

// remoteBases: the remote commits snap builds on (see localRepo.remoteBases),
// when the checkout has remote-tracking branches; else base itself.
func (r *hoRepo) remoteBases(ctx context.Context, snap, base string) []string {
	if r.remote != "" {
		out, err := r.git(ctx, nil, nil, "rev-list", "--boundary", snap, "--not", "--remotes="+r.remote)
		if err == nil {
			var bases []string
			for _, l := range strings.Split(out, "\n") {
				if b, ok := strings.CutPrefix(strings.TrimSpace(l), "-"); ok && fullSHA(b) && len(bases) < maxBases {
					bases = append(bases, b)
				}
			}
			if len(bases) > 0 {
				return bases
			}
		}
	}
	return []string{base}
}

// bundle writes a bundle of ref minus the commits have (those the source
// lacks are dropped) into the checkout's git directory; "" when the other
// side has everything already.
func (r *hoRepo) bundle(ctx context.Context, ref, hid string, have []string) (string, error) {
	gd, err := r.gitDir(ctx)
	if err != nil {
		return "", err
	}
	p := gd + r.ep.sep() + "longarm-handoff-" + hid + ".bundle"
	args := append([]string{"bundle", "create", "-q", p, ref}, notArgs(r.existing(ctx, have))...)
	if _, err := r.git(ctx, nil, nil, args...); err != nil {
		if strings.Contains(err.Error(), "empty bundle") || strings.Contains(err.Error(), "Refusing to create empty bundle") {
			return "", nil
		}
		return "", err
	}
	return p, nil
}

// fetchBundle brings a bundle's ref into this (local) checkout.
func (r *hoRepo) fetchBundle(ctx context.Context, bundle, ref string) error {
	_, err := r.git(ctx, nil, nil, "fetch", "-q", "--no-tags", bundle, "+"+ref+":"+ref)
	return err
}

// freeBranch returns handoff/<short> or, if taken, handoff/<short>-N.
func (r *hoRepo) freeBranch(ctx context.Context, short string) string {
	for i := 1; ; i++ {
		b := "handoff/" + short
		if i > 1 {
			b = fmt.Sprintf("%s-%d", b, i)
		}
		if _, err := r.git(ctx, nil, nil, "rev-parse", "--verify", "-q", "refs/heads/"+b); err != nil {
			return b
		}
	}
}

// restoreUncommitted turns a checkout of the snapshot into base plus the
// snapshot's changes, uncommitted (untracked files untracked again), on a
// new branch.
func (r *hoRepo) restoreUncommitted(ctx context.Context, branch, base string) error {
	if _, err := r.git(ctx, nil, nil, "checkout", "-q", "-b", branch); err != nil {
		return err
	}
	_, err := r.git(ctx, nil, nil, "reset", "-q", "--mixed", base)
	return err
}

// addLocalWorktree creates dir as a new worktree of the (local) checkout at
// snap on branch, then restores the changes as uncommitted.
func (r *hoRepo) addLocalWorktree(ctx context.Context, dir, branch, snap, base string) error {
	if _, err := r.git(ctx, nil, nil, "worktree", "add", "-q", "-b", branch, dir, snap); err != nil {
		return err
	}
	wt := &hoRepo{ep: r.ep, top: dir}
	_, err := wt.git(ctx, nil, nil, "reset", "-q", "--mixed", base)
	return err
}

// sendBundle ships a bundle (made on another machine) to a worker as a
// Workspace.Sync into path: the worker imports it into its mirror of key
// and makes path a worktree at commit.
func sendBundle(ctx context.Context, conn *client.Conn, key, commit, branch, dst, bundle string) (*pb.SyncResult, error) {
	st, err := conn.Workspace().Sync(ctx)
	if err != nil {
		return nil, err
	}
	h := &pb.SyncHeader{Repo: key, Commit: commit, Branch: branch, Path: dst}
	if err := st.Send(&pb.SyncChunk{Msg: &pb.SyncChunk_Header{Header: h}}); err != nil {
		_, rerr := st.CloseAndRecv()
		return nil, firstErr(rerr, err)
	}
	if bundle != "" {
		if err := sendFile(st, pb.SyncPart_SYNC_PART_BUNDLE, bundle); err != nil {
			_, rerr := st.CloseAndRecv()
			return nil, firstErr(rerr, err)
		}
	}
	return st.CloseAndRecv()
}

// forwardFor gets a credential for url (https only) for the target's
// remote-first fetch; nil (with a note) when there is none.
func forwardFor(ctx context.Context, url string) *gitForward {
	u, ok := gitauth.HTTPSURL(url)
	if !ok {
		fmt.Fprintf(os.Stderr, "longarm: note: --git-auth forward: %s is not an https remote of a known host; shipping all commits the target lacks\n",
			workspace.RedactURLs(url))
		return nil
	}
	cred, err := acquirer.Acquire(ctx, u)
	if err != nil {
		fmt.Fprintf(os.Stderr, "longarm: note: %v; shipping all commits the target lacks\n", err)
		return nil
	}
	fmt.Fprintf(os.Stderr, "longarm: forwarding a credential for %s from %s\n", remoteHost(u), cred.Source)
	return &gitForward{url: u, cred: cred}
}

// fetchFirst has the target worker fetch bases from the remote with the
// forwarded credential (remote first); failures are notes.
func fetchFirst(ctx context.Context, conn *client.Conn, g *gitForward, key string, bases []string) bool {
	defer g.clear()
	fctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	res, err := conn.Workspace().Fetch(fctx, &pb.FetchRequest{Repo: key, Url: g.url, Commits: bases, GitAuth: g.pb()})
	if err != nil {
		fmt.Fprintf(os.Stderr, "longarm: note: the target could not fetch from %s (%s); shipping all commits it lacks\n", remoteHost(g.url), grpcMessage(err))
		return false
	}
	fmt.Fprintf(os.Stderr, "longarm: the target fetched %d of %d base commit(s) from %s; shipping the delta\n", len(res.Present), len(bases), remoteHost(g.url))
	return len(res.Present) > 0
}

// localWorktreeDir picks the new local worktree's directory: <parent>/<repo>-handoff-<short>
// (parent: --worktree-dir, else next to the checkout), a free name.
func localWorktreeDir(top, parent, short string) string {
	if parent == "" {
		parent = filepath.Dir(top)
	}
	base := filepath.Join(parent, filepath.Base(top)+"-handoff-"+short)
	p := base
	for i := 2; ; i++ {
		if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
			return p
		}
		p = fmt.Sprintf("%s-%d", base, i)
	}
}

// trimBytes is a short form of command output for messages.
func trimBytes(b []byte) string { return firstLines(string(bytes.TrimSpace(b)), 2) }
