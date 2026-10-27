package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/gitauth"
	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/workspace"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// --git-auth: how the worker authenticates to the repository's remote.
const (
	gitAuthNone     = "none"    // the worker's own credentials (default)
	gitAuthForward  = "forward" // a short-lived credential of yours, per job
	gitAuthFlagHelp = "forward: get a credential for the repository's https remote here (Azure DevOps: az, GitHub: gh, " +
		"else git credential fill) and send it with the job for the worker's fetch only (memory-only, never shown to the " +
		"job); with --repo <local checkout> the worker first fetches what the remote has and only the unpushed delta is shipped"
)

// gitForward is a credential for the https remote url, held in memory
// from before placement until it was sent (then cleared).
type gitForward struct {
	url    string
	remote string // local checkout: the remote's name
	cred   *gitauth.Cred
}

func (g *gitForward) clear() {
	if g != nil {
		g.cred.Clear()
	}
}

func (g *gitForward) pb() *pb.GitAuth {
	if g == nil || g.cred == nil {
		return nil
	}
	return &pb.GitAuth{Scheme: g.cred.Scheme, Username: g.cred.Username, Secret: g.cred.Secret}
}

// checkGitAuthFlag validates --git-auth.
func checkGitAuthFlag(mode, repo string) error {
	switch mode {
	case "", gitAuthNone:
		return nil
	case gitAuthForward:
		if repo == "" {
			return usagef("--git-auth forward goes with --repo")
		}
		return nil
	}
	return usagef("--git-auth %q: want forward or none", mode)
}

var acquirer = &gitauth.Acquirer{Log: os.Stderr}

// prepareGitForward gets the credential of --git-auth forward for --repo:
// for a URL (which must be https) failing is an error (exit 77); for a
// local checkout it is for the remote of its upstream (or origin) and any
// problem only means shipping everything, with a note. nil: no forwarding.
func prepareGitForward(ctx context.Context, mode, repo, ref string) (*gitForward, error) {
	if mode != gitAuthForward {
		return nil, nil
	}
	if isURL(repo) {
		u, ok := gitauth.HTTPSURL(repo)
		if !ok || !strings.HasPrefix(repo, "https://") {
			hint := ""
			if ok {
				hint = " (" + u + ")"
			}
			return nil, usagef("--git-auth forward: --repo must be an https url%s", hint)
		}
		cred, err := acquirer.Acquire(ctx, repo)
		if err != nil {
			return nil, &codedError{code: exitNoPerm, err: err}
		}
		fmt.Fprintf(os.Stderr, "longarm: forwarding a credential for %s from %s\n", remoteHost(repo), cred.Source)
		return &gitForward{url: repo, cred: cred}, nil
	}
	lr, err := openLocalRepo(repo, "")
	if err != nil {
		return nil, err
	}
	remote, raw := lr.remoteFor(ref)
	if remote == "" {
		fmt.Fprintf(os.Stderr, "longarm: note: --git-auth forward: %s has no remote; shipping all commits the worker lacks\n", lr.top)
		return nil, nil
	}
	u, ok := gitauth.HTTPSURL(raw)
	if !ok {
		fmt.Fprintf(os.Stderr, "longarm: note: --git-auth forward: remote %s (%s) is not an https remote of a known host; "+
			"shipping all commits the worker lacks\n", remote, workspace.RedactURLs(raw))
		return nil, nil
	}
	cred, err := acquirer.Acquire(ctx, u)
	if err != nil {
		fmt.Fprintf(os.Stderr, "longarm: note: %v; shipping all commits the worker lacks\n", err)
		return nil, nil
	}
	fmt.Fprintf(os.Stderr, "longarm: forwarding a credential for %s from %s\n", remoteHost(u), cred.Source)
	return &gitForward{url: u, remote: remote, cred: cred}, nil
}

func remoteHost(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		h := u[i+3:]
		h, _, _ = strings.Cut(h, "/")
		if at := strings.LastIndex(h, "@"); at >= 0 {
			h = h[at+1:]
		}
		return h
	}
	return u
}

// remoteFor names the remote whose commits the worker can fetch for ref:
// the upstream remote of the branch (HEAD's for HEAD), else origin; and
// its URL.
func (lr *localRepo) remoteFor(ref string) (name, url string) {
	branch := ref
	if ref == "" || ref == "HEAD" {
		if b, err := gitOut(lr.top, nil, "symbolic-ref", "-q", "--short", "HEAD"); err == nil {
			branch = strings.TrimSpace(string(b))
		}
	}
	if branch != "" && !fullSHA(branch) {
		if r, err := gitOut(lr.top, nil, "config", "--get", "branch."+branch+".remote"); err == nil {
			name = strings.TrimSpace(string(r))
		}
	}
	if name == "" || name == "." {
		name = "origin"
	}
	out, err := gitOut(lr.top, nil, "remote", "get-url", name)
	if err != nil {
		return "", ""
	}
	return name, strings.TrimSpace(string(out))
}

// maxBases caps the remote commits the worker is asked to fetch.
const maxBases = 16

// remoteBases are the commits of the remote that commit builds on: the
// boundary of what is not on any of the remote's tracking branches (commit
// itself when it is on one). These the worker fetches from the remote;
// the bundle then holds only the unpushed rest.
func (lr *localRepo) remoteBases(commit, remote string) []string {
	out, err := gitOut(lr.top, nil, "rev-list", "--boundary", commit, "--not", "--remotes="+remote)
	if err != nil {
		return nil
	}
	var bases []string
	unpushed := 0
	for _, l := range strings.Split(string(out), "\n") {
		l = strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(l, "-"):
			if b := l[1:]; fullSHA(b) && len(bases) < maxBases {
				bases = append(bases, b)
			}
		case l != "":
			unpushed++
		}
	}
	if unpushed == 0 {
		return []string{commit} // on the remote already
	}
	return bases
}

// fetchRemoteFirst has the worker fetch the remote commits that commit
// builds on (Workspace.Fetch with the forwarded credential), so that the
// bundle that follows carries only the delta. Failures are notes: the
// shipping then sends what the worker lacks, as without forwarding.
func (lr *localRepo) fetchRemoteFirst(ctx context.Context, conn *client.Conn, g *gitForward, commit string) {
	defer g.clear()
	remote := g.remote
	bases := lr.remoteBases(commit, remote)
	if len(bases) == 0 {
		fmt.Fprintf(os.Stderr, "longarm: note: %s shares no commits with remote %s; shipping all commits the worker lacks\n", commit[:12], remote)
		return
	}
	if err := needFeature(ctx, conn, jobs.FeatureGitAuth, "--git-auth forward"); err != nil {
		fmt.Fprintf(os.Stderr, "longarm: note: %v; shipping all commits the worker lacks\n", err)
		return
	}
	fctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	start := time.Now()
	res, err := conn.Workspace().Fetch(fctx, &pb.FetchRequest{Repo: lr.key, Url: g.url, Commits: bases, GitAuth: g.pb()})
	if err != nil {
		fmt.Fprintf(os.Stderr, "longarm: note: the worker could not fetch from %s (%s); shipping all commits it lacks\n",
			remoteHost(g.url), grpcMessage(err))
		return
	}
	fmt.Fprintf(os.Stderr, "longarm: the worker fetched %d of %d base commit(s) from %s in %s; shipping the delta\n",
		len(res.Present), len(bases), remoteHost(g.url), time.Since(start).Round(time.Millisecond))
}

func grpcMessage(err error) string {
	if s, ok := status.FromError(err); ok {
		return s.Message()
	}
	return err.Error()
}
