package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/spf13/cobra"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/workspace"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

func syncCmd() *cobra.Command {
	var repoName, ref, lfs string
	var noLocal bool
	c := &cobra.Command{
		Use:   "sync <dir> <worker>:<path>",
		Short: "Mirror a local git checkout (commits + uncommitted + untracked files) into a worktree on the worker",
		Long: `Make <path> on the worker a git worktree of the local repository's state.

Only objects the worker's mirror (<data>/mirrors/<repo>.git) lacks are sent,
as a git bundle (the worker reports which commits it has). Uncommitted changes
to tracked files travel as a binary diff, untracked (not ignored) files as a
tar. On the worker, <path> is created as a worktree of the mirror or, if it is
already one, reset to the new state: tracked changes and untracked files there
are discarded, ignored files (node_modules, build output) are kept.
<path> may be empty ("w1:") for <home>/longarm-sync/<repo>.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openSession()
			if err != nil {
				return err
			}
			defer s.close()
			worker, dst, ok := splitRemote(s.cfg, args[1])
			if !ok {
				return errors.New("second argument must be <worker>:<path>")
			}
			conn, err := s.conn(worker)
			if err != nil {
				return err
			}
			defer conn.Close()
			lr, err := openLocalRepo(args[0], repoName)
			if err != nil {
				return err
			}
			if dst == "" {
				dst = "longarm-sync/" + lr.key
			}
			start := time.Now()
			res, err := lr.upload(cmd.Context(), conn, ref, lfs, !noLocal, &pb.SyncHeader{Path: dst})
			if err != nil {
				return err
			}
			fmt.Printf("%s:%s = %s (%s)\n", worker, res.Path, res.Commit[:12], lr.key)
			lfsSent := ""
			if res.LfsBytes > 0 {
				lfsSent = ", lfs " + humanSize(res.LfsBytes)
			}
			fmt.Fprintf(os.Stderr, "longarm: sent bundle %s, patch %s, untracked %s%s in %s\n", humanSize(res.BundleBytes),
				humanSize(res.PatchBytes), humanSize(res.UntrackedBytes), lfsSent, time.Since(start).Round(time.Millisecond))
			return nil
		},
	}
	c.Flags().StringVar(&repoName, "repo-name", "", "mirror name on the worker (default: from the origin url, else the directory name)")
	c.Flags().StringVar(&ref, "ref", "HEAD", "commit to sync: HEAD, a branch, a tag or a full commit id (uncommitted changes are only included for HEAD)")
	c.Flags().StringVar(&lfs, "lfs", lfsSkip, lfsFlagHelp)
	c.Flags().BoolVar(&noLocal, "committed-only", false, "do not include uncommitted changes and untracked files")
	return c
}

// localRepo is a local git checkout.
type localRepo struct {
	top string
	key string
}

func gitOut(dir string, stdin io.Reader, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdin = stdin
	var errb bytes.Buffer
	cmd.Stderr = &errb
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out, nil
}

func openLocalRepo(dir, name string) (*localRepo, error) {
	out, err := gitOut(dir, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("%s is not inside a git checkout: %w", dir, err)
	}
	lr := &localRepo{top: strings.TrimSpace(string(out))}
	switch {
	case name != "":
		if !workspace.ValidRepo(name) {
			return nil, fmt.Errorf("bad --repo-name %q", name)
		}
		lr.key = name
	default:
		if u, err := gitOut(lr.top, nil, "remote", "get-url", "origin"); err == nil && len(bytes.TrimSpace(u)) > 0 {
			lr.key = workspace.RepoKey(string(bytes.TrimSpace(u)))
		} else {
			lr.key = workspace.RepoKey(filepath.Base(lr.top))
		}
	}
	return lr, nil
}

// upload sends what the worker lacks for ref (plus, for HEAD and
// withLocal, uncommitted and untracked files; with lfs "ship" the Git LFS
// objects of the commit) with Workspace.Sync; h carries the target (path
// or stage_only). ref is HEAD, a branch, a tag or a full commit id.
func (lr *localRepo) upload(ctx context.Context, conn *client.Conn, ref, lfs string, withLocal bool, h *pb.SyncHeader) (*pb.SyncResult, error) {
	if ref == "" {
		ref = "HEAD"
	}
	out, err := gitOut(lr.top, nil, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		if fullSHA(ref) {
			return nil, fmt.Errorf("--ref %s: no such commit in %s (fetch it first)", ref, lr.top)
		}
		return nil, err
	}
	commit := strings.TrimSpace(string(out))
	bundleRef := "HEAD"
	if ref != "HEAD" {
		if fullSHA(ref) {
			// A commit, not a ref: bundled through a temporary ref (a
			// bundle carries refs only), checked out detached.
			bundleRef = "refs/longarm/tmp/" + ulid.Make().String()
			if _, err := gitOut(lr.top, nil, "update-ref", "--no-deref", bundleRef, commit); err != nil {
				return nil, err
			}
			defer gitOut(lr.top, nil, "update-ref", "-d", bundleRef)
		} else {
			full, err := gitOut(lr.top, nil, "rev-parse", "--symbolic-full-name", ref)
			bundleRef = strings.TrimSpace(string(full))
			if err != nil || !strings.HasPrefix(bundleRef, "refs/") {
				if shortSHA(ref) {
					return nil, fmt.Errorf("--ref %q: a short commit id; give the full one (%s), a branch or a tag", ref, commit)
				}
				return nil, fmt.Errorf("--ref %q: use HEAD, a branch, a tag or a full commit id", ref)
			}
		}
		withLocal = false
	}
	branch := "detached"
	if b, err := gitOut(lr.top, nil, "rev-parse", "--abbrev-ref", "HEAD"); err == nil && ref == "HEAD" {
		branch = strings.TrimSpace(string(b))
	} else if ref != "HEAD" {
		branch = ref
	}
	var lfsOids []string
	var lfsDir string
	switch lfs {
	case "", lfsSkip:
		if lr.usesLFS(commit) {
			fmt.Fprintf(os.Stderr, "longarm: note: %s uses Git LFS; on the worker LFS files stay pointer files (--lfs ship sends their content)\n", lr.key)
		}
	case lfsShip:
		if lfsOids, lfsDir, err = lr.lfsObjects(commit); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("--lfs %q: want skip or ship", lfs)
	}

	rctx, cancel := context.WithTimeout(ctx, time.Minute)
	refs, err := conn.Workspace().Refs(rctx, &pb.RefsRequest{Repo: lr.key, LfsOids: lfsOids})
	cancel()
	if err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp("", "longarm-sync-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	var bundle string
	if !contains(refs.Commits, commit) {
		have, err := lr.existing(refs.Commits)
		if err != nil {
			return nil, err
		}
		bundle = filepath.Join(tmp, "sync.bundle")
		args := append([]string{"bundle", "create", "-q", bundle, bundleRef}, notArgs(have)...)
		if _, err := gitOut(lr.top, nil, args...); err != nil {
			if !strings.Contains(err.Error(), "empty bundle") {
				return nil, err
			}
			bundle = ""
		}
	}
	var patch, untracked string
	if withLocal {
		d, err := gitOut(lr.top, nil, "diff", "--binary", "HEAD")
		if err != nil {
			return nil, err
		}
		if len(d) > 0 {
			patch = filepath.Join(tmp, "patch")
			if err := os.WriteFile(patch, d, 0o600); err != nil {
				return nil, err
			}
		}
		if untracked, err = lr.untrackedTar(filepath.Join(tmp, "untracked.tar")); err != nil {
			return nil, err
		}
	}

	var lfsTar string
	if len(lfsOids) > 0 {
		if lfsTar, err = lfsObjectsTar(filepath.Join(tmp, "lfs.tar"), lfsDir, lfsOids, refs.LfsPresent); err != nil {
			return nil, err
		}
	}

	h.Repo, h.Commit, h.Branch = lr.key, commit, branch
	st, err := conn.Workspace().Sync(ctx)
	if err != nil {
		return nil, err
	}
	if err := st.Send(&pb.SyncChunk{Msg: &pb.SyncChunk_Header{Header: h}}); err != nil {
		_, rerr := st.CloseAndRecv()
		return nil, firstErr(rerr, err)
	}
	for _, part := range []struct {
		kind pb.SyncPart
		file string
	}{{pb.SyncPart_SYNC_PART_BUNDLE, bundle}, {pb.SyncPart_SYNC_PART_PATCH, patch}, {pb.SyncPart_SYNC_PART_UNTRACKED_TAR, untracked},
		{pb.SyncPart_SYNC_PART_LFS_TAR, lfsTar}} {
		if part.file == "" {
			continue
		}
		if err := sendFile(st, part.kind, part.file); err != nil {
			_, rerr := st.CloseAndRecv()
			return nil, firstErr(rerr, err)
		}
	}
	res, err := st.CloseAndRecv()
	if err != nil && lfsTar != "" && strings.Contains(err.Error(), "unknown part") {
		return nil, fmt.Errorf("the worker's longarmd is too old for --lfs ship (update it): %w", err)
	}
	return res, err
}

func sendFile(st pb.Workspace_SyncClient, kind pb.SyncPart, p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, transferChunk)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			if err := st.Send(&pb.SyncChunk{Msg: &pb.SyncChunk_Data{Data: &pb.SyncData{Part: kind, Data: buf[:n]}}}); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func notArgs(have []string) []string {
	if len(have) == 0 {
		return nil
	}
	if len(have) > 200 { // keep the command line short; newest tips first
		have = have[:200]
	}
	return append([]string{"--not"}, have...)
}

// existing filters commits to those present locally (bundle prerequisites
// must exist on our side).
func (lr *localRepo) existing(commits []string) ([]string, error) {
	if len(commits) == 0 {
		return nil, nil
	}
	out, err := gitOut(lr.top, strings.NewReader(strings.Join(commits, "\n")+"\n"), "cat-file", "--batch-check=%(objectname) %(objecttype)")
	if err != nil {
		return nil, err
	}
	var have []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && f[1] == "commit" {
			have = append(have, f[0])
		}
	}
	return have, nil
}

// untrackedTar writes the untracked, not ignored files into a tar; "" when
// there are none.
func (lr *localRepo) untrackedTar(dst string) (string, error) {
	out, err := gitOut(lr.top, nil, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", err
	}
	var names []string
	for _, n := range strings.Split(string(out), "\x00") {
		if n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return "", nil
	}
	f, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	for _, n := range names {
		p := filepath.Join(lr.top, filepath.FromSlash(n))
		fi, err := os.Lstat(p)
		if err != nil {
			return "", err
		}
		if !fi.Mode().IsRegular() {
			fmt.Fprintf(os.Stderr, "longarm: sync skips untracked non-regular file %s\n", n)
			continue
		}
		h := &tar.Header{Name: n, Mode: int64(fi.Mode().Perm()), Size: fi.Size(), ModTime: fi.ModTime(), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(h); err != nil {
			return "", err
		}
		src, err := os.Open(p)
		if err != nil {
			return "", err
		}
		_, err = io.CopyN(tw, src, fi.Size())
		src.Close()
		if err != nil {
			return "", err
		}
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	return dst, nil
}

// jobWorkspace turns `longarm run --repo X --ref R [--lfs L]` into a JobWorkspace: a URL is
// fetched by the worker itself; a local checkout is uploaded first (staged
// on the worker until the job's worktree is prepared).
func jobWorkspace(ctx context.Context, conn *client.Conn, repo, ref, lfs string) (*pb.JobWorkspace, error) {
	if repo == "" {
		return nil, nil
	}
	if isURL(repo) {
		if lfs == lfsShip {
			return nil, errors.New("--lfs ship goes with a local checkout (--repo <path>); a url is fetched by the worker")
		}
		return &pb.JobWorkspace{Repo: workspace.RepoKey(repo), Url: repo, Ref: ref}, nil
	}
	lr, err := openLocalRepo(repo, "")
	if err != nil {
		return nil, err
	}
	res, err := lr.upload(ctx, conn, ref, lfs, true, &pb.SyncHeader{StageOnly: true})
	if err != nil {
		return nil, err
	}
	return &pb.JobWorkspace{Repo: lr.key, Commit: res.Commit, StagingId: res.StagingId}, nil
}

func isURL(s string) bool {
	return strings.Contains(s, "://") || (strings.Contains(s, "@") && strings.Contains(s, ":") && !filepath.IsAbs(s))
}

// --lfs: what happens to the Git LFS content of a shipped checkout.
const (
	lfsSkip     = "skip" // LFS files are pointer files on the worker
	lfsShip     = "ship" // the commit's LFS objects travel too
	lfsFlagHelp = "Git LFS files of a local checkout: skip (pointer files on the worker) or ship (send the commit's LFS objects the worker lacks; git-lfs on both sides)"
)

var (
	fullSHARE  = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	shortSHARE = regexp.MustCompile(`^[0-9a-f]{4,63}$`)
)

func fullSHA(s string) bool  { return fullSHARE.MatchString(s) }
func shortSHA(s string) bool { return !fullSHA(s) && shortSHARE.MatchString(s) }

// usesLFS reports (cheaply, without git-lfs) whether commit's root
// .gitattributes routes files through the LFS filter.
func (lr *localRepo) usesLFS(commit string) bool {
	b, err := gitOut(lr.top, nil, "cat-file", "blob", commit+":.gitattributes")
	return err == nil && bytes.Contains(b, []byte("filter=lfs"))
}

// lfsObjects lists the LFS objects (oids) of commit and the local LFS
// store they are in.
func (lr *localRepo) lfsObjects(commit string) ([]string, string, error) {
	env, err := gitOut(lr.top, nil, "lfs", "env")
	if err != nil {
		return nil, "", fmt.Errorf("--lfs ship needs git-lfs here: %w", err)
	}
	var dir string
	for _, l := range strings.Split(string(env), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "LocalMediaDir="); ok {
			dir = v
		}
	}
	if dir == "" {
		return nil, "", errors.New("git lfs env names no LocalMediaDir")
	}
	out, err := gitOut(lr.top, nil, "lfs", "ls-files", "--long", commit)
	if err != nil {
		return nil, "", err
	}
	seen := map[string]bool{}
	var oids []string
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) > 0 && workspace.ValidLFSOid(f[0]) && !seen[f[0]] {
			seen[f[0]] = true
			oids = append(oids, f[0])
		}
	}
	return oids, dir, nil
}

// lfsObjectsTar writes the objects of oids the worker lacks (not in
// present) from the local store dir into a tar at dst, named by oid; ""
// when there is nothing to send. Objects missing here are reported: those
// files stay pointer files on the worker.
func lfsObjectsTar(dst, dir string, oids, present []string) (string, error) {
	have := map[string]bool{}
	for _, o := range present {
		have[o] = true
	}
	var send []string
	missing := 0
	for _, o := range oids {
		if have[o] {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, o[0:2], o[2:4], o)); err != nil {
			missing++
			continue
		}
		send = append(send, o)
	}
	if missing > 0 {
		fmt.Fprintf(os.Stderr, "longarm: %d Git LFS object(s) of the commit are not in the local LFS store (git lfs fetch gets them); "+
			"those files stay pointer files on the worker\n", missing)
	}
	if len(send) == 0 {
		return "", nil
	}
	f, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	for _, o := range send {
		src, err := os.Open(filepath.Join(dir, o[0:2], o[2:4], o))
		if err != nil {
			return "", err
		}
		fi, err := src.Stat()
		if err == nil {
			err = tw.WriteHeader(&tar.Header{Name: o, Mode: 0o644, Size: fi.Size(), ModTime: fi.ModTime(), Typeflag: tar.TypeReg})
		}
		if err == nil {
			_, err = io.CopyN(tw, src, fi.Size())
		}
		src.Close()
		if err != nil {
			return "", err
		}
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	return dst, nil
}
