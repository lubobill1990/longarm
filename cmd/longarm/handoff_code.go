package main

// How longarm handoff moves the code (docs/usage.md#hand-off-an-agent-session):
//
//   --via relay   the snapshot's commits travel over longarm's own end-to-end
//                 channels: local -> worker as `sync` does; worker -> worker
//                 directly between the workers (Handoff service, a one-time
//                 offer only the target may fetch), else relayed by this
//                 client; worker -> local as a bundle.
//   --via remote  the snapshot is pushed to a temporary branch
//                 longarm/handoff/<user>/<id>-<time> of the repository's
//                 https remote and the target fetches it from there; the
//                 branch is deleted once the target has it (--keep-branch).
//                 Refused (back to relay, with a note) without an https
//                 remote or a credential, when the secret check finds
//                 something, a file is over 50 MB, or the remote refuses
//                 the push.
//   --via auto    relay below --via-threshold of objects the target lacks,
//                 remote above (the default: defaultHandoffVia).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/gitauth"
	"github.com/lubobill1990/longarm/internal/jobs"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// The defaults of --via and --via-threshold (overridden by [handoff] via /
// via_threshold in workers.toml, then by the flags).
const (
	defaultHandoffVia   = "auto"
	defaultViaThreshold = "20M"
	// remoteMaxFile: a changed or untracked file larger than this keeps the
	// snapshot off the remote (relay instead).
	remoteMaxFile = 50 << 20
	// directOfferTTL: how long the source worker keeps a direct offer.
	directOfferTTL = 10 * time.Minute
)

// codeResult is where the code went and how.
type codeResult struct {
	top, branch string
	remoteFirst bool
	transfer    string // "upload", "relay", "direct", "remote", "bundle"
	relayed     int64  // commit bytes this client relayed between workers
	temp        *tempBranch
}

// coder moves one snapshot.
type coder struct {
	s                       *session
	o                       handoffOpts
	src, dst                *hoEndpoint
	srcRepo                 *hoRepo
	ref, hid, short         string
	snap, base              string
	tmp                     string
	user                    string
	localRepo               *hoRepo // --to local: the user's checkout
	notes                   []string
	refsCache               []string
	refsLoaded, refsMissing bool
}

func (c *coder) note(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	c.notes = append(c.notes, msg)
	notef("%s", msg)
}

// moveCode makes the target worktree from the snapshot (see the top of
// this file for the ways).
func moveCode(ctx context.Context, s *session, o handoffOpts, src, dst *hoEndpoint, srcRepo *hoRepo, ref, hid, short, snap, base string) (*codeResult, error) {
	tmp, err := os.MkdirTemp("", "longarm-handoff-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	c := &coder{s: s, o: o, src: src, dst: dst, srcRepo: srcRepo, ref: ref, hid: hid, short: short, snap: snap, base: base, tmp: tmp,
		user: handoffUser(src, dst)}
	if dst.local() && !src.local() {
		if c.localRepo, err = c.openLocalRepo(ctx); err != nil {
			return nil, err
		}
	}
	via := o.via
	if via == "auto" {
		delta := c.delta(ctx)
		if delta >= o.viaThreshold {
			via = "remote"
			notef("%s of commits to move (--via-threshold %s): through the remote", humanSize(delta), humanSize(o.viaThreshold))
		} else {
			via = "relay"
		}
	}
	if via == "remote" {
		res, why, err := c.remote(ctx)
		if err != nil {
			return nil, err
		}
		if res != nil {
			return c.finish(ctx, res)
		}
		c.note("not through the remote (%s): relaying instead", why)
	}
	res, err := c.relay(ctx)
	if err != nil {
		return nil, err
	}
	return c.finish(ctx, res)
}

// finish turns the target checkout of the snapshot into base + the changes,
// uncommitted, on a new branch (a local target did that already).
func (c *coder) finish(ctx context.Context, res *codeResult) (*codeResult, error) {
	if c.dst.local() {
		return res, nil
	}
	wt := &hoRepo{ep: c.dst, top: c.dst.native(res.top)}
	res.branch = wt.freeBranch(ctx, c.short)
	if err := wt.restoreUncommitted(ctx, res.branch, c.base); err != nil {
		return nil, err
	}
	res.top = wt.top
	return res, nil
}

// handoffUser names the user in temporary branch names: the device's
// certificate owner, else what a worker sees.
func handoffUser(eps ...*hoEndpoint) string {
	u := ""
	if b, err := deviceBundle(); err == nil {
		u = b.Identity.Owner()
	}
	for _, e := range eps {
		if u == "" && e.callerUser != "" {
			u = e.callerUser
		}
	}
	return sanitizeRefPart(u)
}

var refPartRE = regexp.MustCompile(`[^a-z0-9._-]+`)

func sanitizeRefPart(s string) string {
	s = strings.Trim(refPartRE.ReplaceAllString(strings.ToLower(s), "-"), ".-")
	for strings.Contains(s, "..") {
		s = strings.ReplaceAll(s, "..", ".")
	}
	if len(s) > 40 {
		s = s[:40]
	}
	if s == "" {
		s = "user"
	}
	return s
}

// tempBranchName is longarm/handoff/<user>/<short-id>-<yyyymmddhhmm>-<nonce>:
// the nonce keeps two handoffs of a session within a minute (one of them
// --keep-branch) from pushing to the same branch.
func tempBranchName(user, short string, t time.Time, nonce string) string {
	return "longarm/handoff/" + sanitizeRefPart(user) + "/" + sanitizeRefPart(short) + "-" + t.UTC().Format("200601021504") + "-" + sanitizeRefPart(nonce)
}

// openLocalRepo is the user's checkout for --to local from a worker.
func (c *coder) openLocalRepo(ctx context.Context) (*hoRepo, error) {
	dir := c.o.localRepo
	if dir == "" {
		dir, _ = os.Getwd()
	}
	lr, err := openHoRepo(ctx, c.dst, dir)
	if err != nil {
		return nil, usagef("--to local needs your local checkout of %s (run handoff in it or give --local-repo): %v", c.srcRepo.key, err)
	}
	if lr.key != c.srcRepo.key {
		return nil, usagef("%s is a checkout of %s, the session's code is %s: run handoff in your checkout of it or give --local-repo", lr.top, lr.key, c.srcRepo.key)
	}
	return lr, nil
}

// targetHave is what the target has of the repository (commit ids).
func (c *coder) targetHave(ctx context.Context) []string {
	if c.refsLoaded {
		return c.refsCache
	}
	c.refsLoaded = true
	switch {
	case c.dst.local() && c.localRepo != nil:
		c.refsCache = c.localRepo.tips(ctx)
	case !c.dst.local():
		rctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		if refs, err := c.dst.conn.Workspace().Refs(rctx, &pb.RefsRequest{Repo: c.srcRepo.key}); err == nil {
			c.refsCache = refs.Commits
		}
	}
	return c.refsCache
}

// delta estimates the bytes of objects the target lacks (git's on-disk
// size of what a bundle would carry); 0 when git can't tell.
func (c *coder) delta(ctx context.Context) int64 {
	args := []string{"rev-list", "--objects", "--disk-usage", c.snap}
	not := c.srcRepo.existing(ctx, c.targetHave(ctx))
	if len(not) > 200 {
		not = not[:200]
	}
	if len(not) > 0 || c.srcRepo.remote != "" {
		args = append(args, "--not")
		args = append(args, not...)
	}
	if c.srcRepo.remote != "" && c.o.gitAuth == gitAuthForward && !c.dst.local() {
		args = append(args, "--remotes="+c.srcRepo.remote) // the target fetches those from the remote first
	}
	out, err := c.srcRepo.git(ctx, nil, nil, args...)
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	return n
}

// ---------------------------------------------------------------- remote

// tempBranch is a pushed temporary branch, deleted once the target has it.
type tempBranch struct {
	url, branch string
	local       *hoRepo     // a local checkout to delete it with your own git (nil: the source worker)
	src         *hoEndpoint // the worker that pushed it
	dir         string
	g           *gitForward
	keep        bool
	done        bool
}

// cleanup deletes the branch (unless --keep-branch); a failure is a note
// with the command that does it.
func (tb *tempBranch) cleanup(ctx context.Context) {
	if tb == nil || tb.done {
		return
	}
	tb.done = true
	defer tb.g.clear()
	if tb.keep {
		notef("kept the temporary branch %s on %s (--keep-branch); delete it with: git push %s --delete %s", tb.branch, remoteHost(tb.url), tb.url, tb.branch)
		return
	}
	var err error
	switch {
	case tb.local != nil:
		_, err = tb.local.pushExact(ctx, tb.url, ":refs/heads/"+tb.branch)
	case tb.g != nil:
		_, err = tb.src.conn.Workspace().PushHandoff(ctx, &pb.PushHandoffRequest{Dir: tb.dir, Url: tb.url, Branch: tb.branch, GitAuth: tb.g.pb()})
	default:
		err = fmt.Errorf("no credential left to delete it with")
	}
	if err != nil {
		notef("note: could not delete the temporary branch %s (%s); delete it with: git push %s --delete %s", tb.branch, oneLine(err), tb.url, tb.branch)
		return
	}
	notef("deleted the temporary branch %s", tb.branch)
}

// remote pushes the snapshot to a temporary branch and has the target fetch
// it. why says why not (then res is nil and the caller relays).
func (c *coder) remote(ctx context.Context) (res *codeResult, why string, err error) {
	raw := c.srcRepo.url
	var cwdRepo *hoRepo
	if cwd, err := os.Getwd(); err == nil && !c.src.local() {
		if r, err := openHoRepo(ctx, &hoEndpoint{name: localMachine, goos: goosLocal()}, cwd); err == nil && r.key == c.srcRepo.key && r.remote != "" {
			cwdRepo = r
			if raw == "" {
				raw = r.url
			}
		}
	}
	if raw == "" {
		return nil, "the checkout has no remote", nil
	}
	u, ok := gitauth.HTTPSURL(raw)
	if !ok {
		return nil, "the remote is not an https remote of a known host", nil
	}
	var g *gitForward
	if !c.src.local() || !c.dst.local() {
		if c.o.gitAuth != gitAuthForward {
			return nil, "a worker pushes or fetches it with your credential: give --git-auth forward", nil
		}
		if g = forwardFor(ctx, u); g == nil {
			return nil, "no credential for " + remoteHost(u), nil
		}
	}
	keepG := false
	defer func() {
		if !keepG {
			g.clear()
		}
	}()
	if !c.src.local() && !slices.Contains(c.src.features, jobs.FeatureHandoffPush) {
		return nil, c.src.name + " cannot push with a forwarded credential (update longarmd)", nil
	}
	if !c.dst.local() && !slices.Contains(c.dst.features, jobs.FeatureGitAuth) {
		return nil, c.dst.name + " cannot fetch with a forwarded credential (update longarmd)", nil
	}
	// nothing secret and nothing huge leaves our channels
	hits, big, serr := scanSnapshot(ctx, c.srcRepo, c.base, c.snap, remoteMaxFile)
	switch {
	case serr != nil:
		return nil, "the secret check failed: " + oneLine(serr), nil
	case len(big) > 0:
		return nil, fmt.Sprintf("%d file(s) over %s for the remote (%s)", len(big), humanSize(remoteMaxFile), strings.Join(firstN(big, 3), ", ")), nil
	case len(hits) > 0 && !c.o.allowSecrets:
		return nil, "the secret check found " + describeHits(hits) + " (--allow-secrets pushes anyway)", nil
	case len(hits) > 0:
		c.note("warning: pushing despite the secret check (--allow-secrets): %s", describeHits(hits))
	}
	nonce := make([]byte, 3)
	rand.Read(nonce)
	branch := tempBranchName(c.user, c.short, time.Now(), hex.EncodeToString(nonce))
	tb := &tempBranch{url: u, branch: branch, keep: c.o.keepBranch}
	// push
	if c.src.local() {
		if _, err := c.srcRepo.pushExact(ctx, u, c.snap+":refs/heads/"+branch); err != nil {
			return nil, "the remote refused the push: " + oneLine(err), nil
		}
		tb.local = c.srcRepo
	} else {
		pctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		_, err := c.src.conn.Workspace().PushHandoff(pctx, &pb.PushHandoffRequest{Dir: c.srcRepo.top, Url: u, Commit: c.snap, Branch: branch, GitAuth: g.pb()})
		cancel()
		if err != nil {
			return nil, "the remote refused the push: " + grpcMessage(err), nil
		}
		switch {
		case c.localRepo != nil:
			tb.local = c.localRepo
		case cwdRepo != nil:
			tb.local = cwdRepo
		default:
			tb.src, tb.dir, tb.g = c.src, c.srcRepo.top, g
			keepG = true // until the branch is deleted
		}
	}
	notef("pushed the snapshot to %s on %s", branch, remoteHost(u))
	// the target fetches it
	r := &codeResult{transfer: "remote", temp: tb, remoteFirst: true}
	if c.dst.local() {
		lr := c.localRepo
		if lr == nil {
			lr = c.srcRepo
		}
		from := lr.remote
		if from == "" {
			from = u
		}
		if _, err := lr.git(ctx, nil, nil, "fetch", "-q", "--no-tags", from, "+refs/heads/"+branch+":"+c.ref); err != nil {
			tb.cleanup(ctx)
			return nil, "fetching the branch here failed: " + oneLine(err), nil
		}
		dir := localWorktreeDir(lr.top, c.o.worktreeDir, c.short)
		r.branch = lr.freeBranch(ctx, c.short)
		if err := lr.addLocalWorktree(ctx, dir, r.branch, c.snap, c.base); err != nil {
			tb.cleanup(ctx)
			return nil, "", err
		}
		r.top = dir
		return r, "", nil
	}
	fg := g
	if fg == nil {
		return nil, "no credential for the target's fetch", nil
	}
	fctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	fres, err := c.dst.conn.Workspace().Fetch(fctx, &pb.FetchRequest{Repo: c.srcRepo.key, Url: u, Commits: []string{c.snap}, GitAuth: fg.pb()})
	cancel()
	if err != nil || !slices.Contains(fres.GetPresent(), c.snap) {
		tb.cleanup(ctx)
		msg := "the snapshot is not there"
		if err != nil {
			msg = grpcMessage(err)
		}
		return nil, c.dst.name + " could not fetch the branch: " + msg, nil
	}
	dstPath, err := freeWorkerDir(ctx, c.dst, c.srcRepo.key, c.short)
	if err != nil {
		tb.cleanup(ctx)
		return nil, "", err
	}
	sres, err := sendBundle(ctx, c.dst.conn, c.srcRepo.key, c.snap, "handoff-"+c.hid, dstPath, "")
	if err != nil {
		tb.cleanup(ctx)
		return nil, "", err
	}
	r.top = sres.Path
	return r, "", nil
}

// pushArgs are the arguments of every handoff push: exactly refspec to
// target and nothing else - no tags that follow the commit
// (push.followTags), no push options, no remote's push / mirror / tagOpt
// settings (target is a URL, not a remote name), no forced update (a
// refspec without "+": a non-fast-forward is refused).
func pushArgs(target, refspec string) []string {
	return []string{"-c", "push.followTags=false", "-c", "push.pushOption=", "-c", "push.recurseSubmodules=no",
		"push", "--no-follow-tags", "--no-recurse-submodules", "--porcelain", "--", target, refspec}
}

// pushExact pushes refspec (one ref) to the push URL of the checkout's
// remote with the user's own git (fallback: url).
func (r *hoRepo) pushExact(ctx context.Context, url, refspec string) (string, error) {
	target := url
	if r.remote != "" {
		if pu, err := r.git(ctx, nil, nil, "remote", "get-url", "--push", r.remote); err == nil && pu != "" {
			target = pu
		}
	}
	if target == "" {
		return "", fmt.Errorf("no remote to push to")
	}
	return r.git(ctx, nil, nil, pushArgs(target, refspec)...)
}

func firstN(s []string, n int) []string {
	if len(s) > n {
		return append(s[:n:n], "...")
	}
	return s
}

// ---------------------------------------------------------------- relay

func (c *coder) relay(ctx context.Context) (*codeResult, error) {
	switch {
	case c.dst.local():
		return c.relayToLocal(ctx)
	case c.src.local():
		return c.upload(ctx)
	}
	return c.workerToWorker(ctx)
}

// upload ships a local snapshot to a worker like sync (remote first with
// --git-auth forward).
func (c *coder) upload(ctx context.Context) (*codeResult, error) {
	lr := &localRepo{top: c.srcRepo.top, key: c.srcRepo.key}
	g, err := prepareGitForward(ctx, c.o.gitAuth, c.srcRepo.top, "HEAD")
	if err != nil {
		return nil, err
	}
	defer g.clear()
	lfs := lfsSkip
	if lr.usesLFS(c.snap) {
		lfs = lfsShip
	}
	dstPath, err := freeWorkerDir(ctx, c.dst, c.srcRepo.key, c.short)
	if err != nil {
		return nil, err
	}
	res, err := lr.upload(ctx, c.dst.conn, c.snap, lfs, false, &pb.SyncHeader{Path: dstPath}, g)
	if err != nil {
		return nil, err
	}
	return &codeResult{top: res.Path, remoteFirst: g != nil, transfer: "upload"}, nil
}

// relayToLocal brings a worker's snapshot here as a bundle.
func (c *coder) relayToLocal(ctx context.Context) (*codeResult, error) {
	lr := c.localRepo
	// the base from the remote with your own git, when it is there (best effort)
	if len(lr.existing(ctx, []string{c.base})) == 0 && lr.remote != "" {
		lr.git(ctx, nil, nil, "fetch", "-q", "--no-tags", lr.remote, c.base)
		c.refsLoaded = false
	}
	b, err := c.srcRepo.bundle(ctx, c.ref, c.hid, append(lr.tips(ctx), lr.existing(ctx, []string{c.base})...))
	if err != nil {
		return nil, err
	}
	if b != "" {
		local := filepath.Join(c.tmp, "handoff.bundle")
		_, err := c.src.download(ctx, b, local, maxRelayBundle)
		c.src.removeFile(context.WithoutCancel(ctx), b)
		if err != nil {
			return nil, err
		}
		if err := lr.fetchBundle(ctx, local, c.ref); err != nil {
			return nil, err
		}
	}
	dir := localWorktreeDir(lr.top, c.o.worktreeDir, c.short)
	branch := lr.freeBranch(ctx, c.short)
	if err := lr.addLocalWorktree(ctx, dir, branch, c.snap, c.base); err != nil {
		return nil, err
	}
	return &codeResult{top: dir, branch: branch, transfer: "bundle"}, nil
}

// workerToWorker moves a worker's snapshot to another worker: directly
// between them when both can, else relayed through this client.
func (c *coder) workerToWorker(ctx context.Context) (*codeResult, error) {
	remoteFirst := false
	if c.o.gitAuth == gitAuthForward {
		url := c.srcRepo.url
		if url == "" {
			if cwd, err := os.Getwd(); err == nil {
				if lr, err := openLocalRepo(cwd, ""); err == nil && lr.key == c.srcRepo.key {
					_, url = lr.remoteFor("HEAD")
				}
			}
		}
		if url == "" {
			c.note("note: --git-auth forward: the source checkout has no remote; shipping all commits the target lacks")
		} else if g := forwardFor(ctx, url); g != nil {
			if needFeature(ctx, c.dst.conn, jobs.FeatureGitAuth, "--git-auth forward") == nil {
				remoteFirst = fetchFirst(ctx, c.dst.conn, g, c.srcRepo.key, c.srcRepo.remoteBases(ctx, c.snap, c.base))
				c.refsLoaded = false
			}
			g.clear()
		}
	}
	b, err := c.srcRepo.bundle(ctx, c.ref, c.hid, c.targetHave(ctx))
	if err != nil {
		return nil, err
	}
	dstPath, err := freeWorkerDir(ctx, c.dst, c.srcRepo.key, c.short)
	if err != nil {
		return nil, err
	}
	if b == "" { // the target has the commits already
		res, err := sendBundle(ctx, c.dst.conn, c.srcRepo.key, c.snap, "handoff-"+c.hid, dstPath, "")
		if err != nil {
			return nil, err
		}
		return &codeResult{top: res.Path, remoteFirst: remoteFirst, transfer: "direct"}, nil
	}
	defer c.src.removeFile(context.WithoutCancel(ctx), b)
	if res, why := c.direct(ctx, b, dstPath); res != nil {
		res.remoteFirst = remoteFirst
		return res, nil
	} else if why != "" {
		c.note("note: no direct transfer %s -> %s (%s): relaying through this machine - keep it online until the handoff is done", c.src.name, c.dst.name, why)
	}
	local := filepath.Join(c.tmp, "handoff.bundle")
	n, err := c.src.download(ctx, b, local, maxRelayBundle)
	if err != nil {
		return nil, err
	}
	notef("relaying %s of commits from %s to %s", humanSize(n), c.src.name, c.dst.name)
	res, err := sendBundle(ctx, c.dst.conn, c.srcRepo.key, c.snap, "handoff-"+c.hid, dstPath, local)
	if err != nil {
		return nil, err
	}
	return &codeResult{top: res.Path, remoteFirst: remoteFirst, transfer: "relay", relayed: n}, nil
}

// direct has the target pull the bundle from the source worker: the
// source offers it once, to the target's certificate, with a random token
// this client gives only to the two of them. why "" with res nil: not
// attempted quietly.
func (c *coder) direct(ctx context.Context, bundle, dstPath string) (*codeResult, string) {
	if !slices.Contains(c.src.features, jobs.FeatureHandoffDirect) || !slices.Contains(c.dst.features, jobs.FeatureHandoffDirect) {
		return nil, "a worker without the handoff-direct feature (update longarmd)"
	}
	endpoints := c.endpointsOf(c.src.name)
	if len(endpoints) == 0 {
		return nil, "no endpoints of " + c.src.name
	}
	defer c.relayPair(ctx)()
	token := make([]byte, 32)
	rand.Read(token)
	defer clear(token)
	tid := "h" + strings.ReplaceAll(c.hid, ".", "-") + "-" + hex.EncodeToString(token[:4])
	octx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	off, err := c.src.conn.Handoff().Offer(octx, &pb.HandoffOfferRequest{Path: bundle, Peer: c.dst.cert, TransferId: tid, Token: token,
		Ttl: durationpb.New(directOfferTTL), MaxBytes: maxRelayBundle})
	if err != nil {
		return nil, c.src.name + ": " + grpcMessage(err)
	}
	pull := &pb.HandoffPullRequest{From: c.src.cert, FromEndpoints: endpoints, TransferId: tid, Token: token,
		Size: off.Size, Sha256: off.Sha256, Header: &pb.SyncHeader{Repo: c.srcRepo.key, Commit: c.snap, Branch: "handoff-" + c.hid, Path: dstPath}}
	res, err := c.dst.conn.Handoff().Pull(octx, pull)
	if status.Code(err) == codes.Unavailable { // once more: a relay may learn the pair a moment after the hub listed it
		select {
		case <-octx.Done():
		case <-time.After(2 * time.Second):
			res, err = c.dst.conn.Handoff().Pull(octx, pull)
		}
	}
	if err != nil {
		return nil, c.dst.name + ": " + grpcMessage(err)
	}
	notef("%s fetched %s of commits from %s directly", c.dst.name, humanSize(off.Size), c.src.name)
	return &codeResult{top: res.Path, transfer: "direct"}, ""
}

// relayPair asks the hub to let its relays carry the target's connection
// to the source for this transfer (Fleet.HandoffPair: relays carry worker
// <-> worker traffic only for listed pairs); the returned func releases
// it. Without a hub, or with a hub that predates it, the transfer needs a
// direct path between the workers (else: relayed through this client).
func (c *coder) relayPair(ctx context.Context) func() {
	h := c.s.optionalHub(ctx)
	if h == nil {
		return func() {}
	}
	req := &pb.HandoffPairRequest{A: c.src.cert, B: c.dst.cert, Ttl: durationpb.New(directOfferTTL)}
	hctx, cancel := context.WithTimeout(ctx, hubTimeout)
	defer cancel()
	if _, err := h.Fleet().HandoffPair(hctx, req); err != nil {
		if status.Code(err) != codes.Unimplemented {
			c.note("note: the hub's relays won't carry %s -> %s (%s): a direct transfer needs a direct path", c.dst.name, c.src.name, grpcMessage(err))
		}
		return func() {}
	}
	return func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), hubTimeout)
		defer cancel()
		h.Fleet().HandoffPair(rctx, &pb.HandoffPairRequest{A: c.src.cert, B: c.dst.cert, Release: true})
	}
}

// endpointsOf are a worker's endpoints as this client knows them.
func (c *coder) endpointsOf(name string) []string {
	if v := os.Getenv("LONGARM_TEST_HANDOFF_ENDPOINTS"); v != "" { // tests: an unreachable source
		return strings.Split(v, ",")
	}
	w, err := c.s.worker(name)
	if err != nil {
		return nil
	}
	var out []string
	if w.Address != "" {
		t := w.Transport
		if t == "" {
			t = "tcp"
		}
		out = append(out, client.Endpoint{Transport: t, Address: w.Address}.Key())
	}
	for _, e := range w.Alt {
		out = append(out, e.Key())
	}
	return out
}

// resolveVia settles --via and --via-threshold: the flag, else [handoff] in
// workers.toml, else the defaults above.
func resolveVia(o *handoffOpts, viaSet bool, threshold string, thresholdSet bool) error {
	if cfg, err := client.LoadConfig(configDir); err == nil {
		if !viaSet && cfg.Handoff.Via != "" {
			o.via = cfg.Handoff.Via
		}
		if !thresholdSet && cfg.Handoff.ViaThreshold != "" {
			threshold = cfg.Handoff.ViaThreshold
		}
	}
	switch o.via {
	case "remote", "relay", "auto":
	default:
		return usagef("--via %q: want remote, relay or auto", o.via)
	}
	n, err := parseSize(strings.NewReplacer("iB", "", "IB", "", "ib", "").Replace(threshold))
	if err != nil || n <= 0 {
		return usagef("--via-threshold %q: want a size such as 20M", threshold)
	}
	o.viaThreshold = n
	return nil
}

// runHandoffCleanup lists the caller's temporary branches on the current
// checkout's remote and deletes those older than an hour (newer ones may
// belong to a handoff in progress).
func runHandoffCleanup(ctx context.Context, o handoffOpts) error {
	dir := o.localRepo
	if dir == "" {
		dir, _ = os.Getwd()
	}
	ep := &hoEndpoint{name: localMachine, goos: goosLocal()}
	r, err := openHoRepo(ctx, ep, dir)
	if err != nil {
		return err
	}
	if r.remote == "" {
		return usagef("%s has no remote", r.top)
	}
	user := handoffUser()
	prefix := "refs/heads/longarm/handoff/" + user + "/"
	out, err := r.git(ctx, nil, nil, "ls-remote", "--heads", "--", r.remote, prefix+"*")
	if err != nil {
		return err
	}
	type branch struct {
		Name    string `json:"branch"`
		Deleted bool   `json:"deleted"`
		Error   string `json:"error,omitempty"`
	}
	var list []branch
	stampRE := regexp.MustCompile(`-(\d{12})(?:-[0-9a-f]{1,16})?$`)
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) != 2 || !strings.HasPrefix(f[1], prefix) {
			continue
		}
		b := branch{Name: strings.TrimPrefix(f[1], "refs/heads/")}
		if m := stampRE.FindStringSubmatch(b.Name); m != nil {
			if t, err := time.Parse("200601021504", m[1]); err == nil && time.Since(t) < time.Hour {
				b.Error = "less than an hour old (a handoff in progress?): kept"
				list = append(list, b)
				continue
			}
		}
		if _, err := r.pushExact(ctx, "", ":"+f[1]); err != nil {
			b.Error = oneLine(err)
		} else {
			b.Deleted = true
		}
		list = append(list, b)
	}
	if o.asJSON {
		if list == nil {
			list = []branch{}
		}
		b, _ := json.MarshalIndent(map[string]any{"remote": r.remote, "branches": list}, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	if len(list) == 0 {
		fmt.Printf("no temporary handoff branches of %s on %s\n", user, r.remote)
	}
	for _, b := range list {
		state := "deleted"
		if !b.Deleted {
			state = b.Error
		}
		fmt.Printf("%s\t%s\n", b.Name, state)
	}
	return nil
}
