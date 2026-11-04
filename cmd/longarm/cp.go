package main

// longarm cp: a file or a directory tree from one worker to another,
// directly (the Copy service, worker feature "copy"; docs/usage.md "Copy
// between workers"). This client offers the source path on the source
// worker to the target worker's certificate with a random token, asks the
// hub to let its relays carry the pair (when there is no direct path), and
// has the target pull it; the data goes worker to worker, never through
// this machine. Partial files stay on the target (<file>.lapart): a copy
// that broke off resumes - on its own after a lost connection, else when
// it is run again.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/lubobill1990/longarm/internal/jobs"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Copies (variables for tests).
var (
	// copyOfferTTL: how long the source serves the offer (revoked when the
	// copy ends)
	copyOfferTTL = 24 * time.Hour
	// copyPullTries: Pulls of one copy after lost connections
	copyPullTries = 5
	copyPullPause = 2 * time.Second
)

func cpCmd() *cobra.Command {
	var quiet bool
	c := &cobra.Command{
		Use:   "cp <worker>:<path> <worker>:<path>",
		Short: "Copy files/directories from one worker to another, directly (resumable, sha256-verified)",
		Long: `Copy a file or a directory (recursively) from one worker to another. The data
goes worker to worker - directly, else through the hub's relay - never
through this machine. Like cp -r: if the target is an existing directory (or
ends with /), the source goes inside it. Relative paths are relative to the
worker account's home. Files whose size and sha256 already match are
skipped; a copy that broke off resumes from the verified prefix of
<file>.lapart on the target - automatically after a lost connection, else
when you run it again. Symlinks are not copied. Both workers must be yours
(or fleet workers you may use) and have the "copy" feature. With one side
on this machine it is longarm put / longarm get.`,
		Example: `  longarm cp w1:build/out w2:in/          # w2:in/out
  longarm cp w1:C:/data/model.bin w2:/models/model.bin`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openSession()
			if err != nil {
				return err
			}
			defer s.close()
			sw, sp, sok := splitRemote(s.cfg, args[0])
			dw, dp, dok := splitRemote(s.cfg, args[1])
			switch {
			case !sok && dok:
				return putCmd().RunE(cmd, args)
			case sok && !dok:
				return getCmd().RunE(cmd, args)
			case !sok && !dok:
				return usagef("cp copies between workers: longarm cp <worker>:<path> <worker>:<path> (on this machine: cp)")
			case sw == dw:
				return usagef("both paths are on %s: longarm exec %s -- cp -r (or Copy-Item -Recurse) ...", sw, sw)
			}
			return copyBetween(cmd.Context(), s, sw, sp, dw, dp, quiet)
		},
	}
	c.Flags().BoolVarP(&quiet, "quiet", "q", false, "no progress, only the summary")
	return c
}

// copyBetween copies srcPath of worker sw to dstPath of worker dw.
func copyBetween(ctx context.Context, s *session, sw, srcPath, dw, dstPath string, quiet bool) error {
	src, err := openEndpoint(ctx, s, sw)
	if err != nil {
		return err
	}
	defer src.close()
	dst, err := openEndpoint(ctx, s, dw)
	if err != nil {
		return err
	}
	defer dst.close()
	for _, e := range []*hoEndpoint{src, dst} {
		if !slices.Contains(e.features, jobs.FeatureCopy) {
			return fmt.Errorf("%s: longarmd %s copies to and from other workers only once updated (longarm worker update %s); meanwhile longarm get, then longarm put",
				e.name, e.version, e.name)
		}
	}
	endpoints := workerEndpoints(s, sw)
	if len(endpoints) == 0 {
		return fmt.Errorf("no endpoints of %s known here", sw)
	}
	token := make([]byte, 32)
	rand.Read(token)
	defer clear(token)
	id := make([]byte, 8)
	rand.Read(id)
	grant := &pb.CopyGrant{TransferId: "cp-" + hex.EncodeToString(id), Token: token}
	off, err := src.conn.Copy().Offer(ctx, &pb.CopyOfferRequest{Path: srcPath, Peer: dst.cert, PeerId: dst.id, TransferId: grant.TransferId,
		Token: token, Ttl: durationpb.New(copyOfferTTL)})
	if err != nil {
		return fmt.Errorf("%s:%s: %s", sw, srcPath, grpcMessage(err))
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), hubTimeout)
		defer cancel()
		src.conn.Copy().Revoke(rctx, &pb.CopyRevokeRequest{TransferId: grant.TransferId})
	}()
	target, err := copyTarget(ctx, dst, dstPath, off.Root.AbsPath)
	if err != nil {
		return err
	}
	defer relayPair(ctx, s, src, dst, true, notef)()
	req := &pb.CopyPullRequest{From: src.cert, FromId: src.id, FromEndpoints: endpoints, Grant: grant, Path: target}
	show := !quiet && stderrIsTerminal()
	start := time.Now()
	var last *pb.CopyProgress
	for try := 1; ; try++ {
		p, err := pullCopy(ctx, dst, req, show)
		if p != nil {
			last = p
		}
		if err == nil {
			break
		}
		// a lost connection (to the target, or the target's to the source):
		// the offer is still there, the partial files too - pull again
		if status.Code(err) != codes.Unavailable || try >= copyPullTries || ctx.Err() != nil {
			return fmt.Errorf("copy %s:%s to %s:%s: %s", sw, srcPath, dw, target, grpcMessage(err))
		}
		notef("%s; resuming", grpcMessage(err))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(copyPullPause):
		}
	}
	notef("%s", copySummary(sw, off.Root.AbsPath, dw, last, time.Since(start)))
	return nil
}

// copyTarget is where the copy goes on dst, like cp -r: into an existing
// directory (or a path ending in a separator) by the source's name, else
// at p itself.
func copyTarget(ctx context.Context, dst *hoEndpoint, p, srcAbs string) (string, error) {
	into := strings.HasSuffix(p, "/") || strings.HasSuffix(p, `\`)
	if !into {
		fi, err := dst.conn.Files().Stat(ctx, &pb.FileStatRequest{Path: p})
		if err != nil {
			return "", fmt.Errorf("%s:%s: %s", dst.name, p, grpcMessage(err))
		}
		into = fi.Exists && fi.IsDir
	}
	if !into {
		return p, nil
	}
	return intoTarget(p, srcAbs), nil
}

// intoTarget is the path of srcAbs (a path on the source worker) inside the
// directory p of the target worker.
func intoTarget(p, srcAbs string) string {
	base := path.Base(strings.ReplaceAll(srcAbs, `\`, "/"))
	d := strings.TrimRight(p, `/\`)
	switch {
	case base == "/" || base == "." || strings.HasSuffix(base, ":"): // a root or a drive: no name of its own
		return d + "/"
	case p == "": // the home directory
		return base
	case d == "": // p was "/"
		return "/" + base
	}
	return d + "/" + base
}

// pullCopy runs one Pull to its end; the last progress (nil: none).
func pullCopy(ctx context.Context, dst *hoEndpoint, req *pb.CopyPullRequest, show bool) (*pb.CopyProgress, error) {
	st, err := dst.conn.Copy().Pull(ctx, req)
	if err != nil {
		return nil, err
	}
	var last *pb.CopyProgress
	shown := false
	defer func() {
		if shown {
			fmt.Fprint(os.Stderr, "\r\033[K")
		}
	}()
	for {
		p, err := st.Recv()
		if errors.Is(err, io.EOF) {
			if last == nil || !last.Done {
				return last, status.Error(codes.Unavailable, "the copy ended early")
			}
			return last, nil
		}
		if err != nil {
			return last, err
		}
		last = p
		if show && !p.Done {
			fmt.Fprintf(os.Stderr, "\r\033[K%s", copyLine(p))
			shown = true
		}
	}
}

// copyLine is the progress line: files, bytes and the file being copied.
func copyLine(p *pb.CopyProgress) string {
	line := fmt.Sprintf("copying %d/%d files, %s / %s", p.Files, p.FilesTotal, humanSize(p.Bytes), humanSize(p.BytesTotal))
	if p.File != "" {
		f := p.File
		if len(f) > 50 {
			f = "..." + f[len(f)-47:]
		}
		line += "  " + f
	}
	return line
}

func copySummary(sw, srcAbs, dw string, p *pb.CopyProgress, took time.Duration) string {
	out := fmt.Sprintf("copied %s:%s to %s:%s: %d file(s), %s sent in %s", sw, srcAbs, dw, p.AbsPath, p.Files, humanSize(p.Sent),
		took.Round(time.Millisecond))
	if p.UpToDate > 0 {
		out += fmt.Sprintf(", %d up to date", p.UpToDate)
	}
	if p.Resumed > 0 {
		out += fmt.Sprintf(", %s resumed", humanSize(p.Resumed))
	}
	if p.Links > 0 {
		out += fmt.Sprintf(", %d symlink(s) not copied", p.Links)
	}
	return out
}
