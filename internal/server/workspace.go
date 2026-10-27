package server

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/oklog/ulid/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/workspace"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

type workspaceServer struct {
	pb.UnimplementedWorkspaceServer
	s *Server
}

func (w *workspaceServer) Refs(ctx context.Context, req *pb.RefsRequest) (*pb.RefsResponse, error) {
	if !workspace.ValidRepo(req.Repo) {
		return nil, status.Errorf(codes.InvalidArgument, "bad repo %q", req.Repo)
	}
	if w.s.perUser() {
		return w.proxyRefs(ctx, req)
	}
	exists, commits, err := w.s.ws().Refs(ctx, req.Repo)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &pb.RefsResponse{Exists: exists, Commits: commits, LfsPresent: w.s.ws().LFSPresent(req.Repo, req.LfsOids)}, nil
}

// GitAuthFromPB is the forwarded credential of a request (nil: none). Its
// secret aliases the request's bytes: Clear zeroes those.
func GitAuthFromPB(a *pb.GitAuth) *jobs.GitAuth {
	if a == nil {
		return nil
	}
	return &jobs.GitAuth{Scheme: a.Scheme, Username: a.Username, Secret: a.Secret}
}

// Fetch fetches commits from a hosted remote into the mirror with a
// forwarded credential (remote-first shipping of a local checkout). The
// credential is used for this fetch only and zeroed; the audit log records
// the url and commits, never the credential.
func (w *workspaceServer) Fetch(ctx context.Context, req *pb.FetchRequest) (*pb.FetchResponse, error) {
	auth := GitAuthFromPB(req.GitAuth)
	defer auth.Clear()
	if !workspace.ValidRepo(req.Repo) {
		return nil, status.Errorf(codes.InvalidArgument, "bad repo %q", req.Repo)
	}
	if !auth.Valid() {
		return nil, status.Error(codes.InvalidArgument, "fetch needs a forwarded credential (git_auth)")
	}
	var res *pb.FetchResponse
	var err error
	start := time.Now()
	if w.s.perUser() {
		res, err = w.proxyFetch(ctx, req)
	} else {
		var present []string
		present, err = w.s.ws().FetchCommits(ctx, req.Repo, req.Url, req.Commits, auth, nil)
		res = &pb.FetchResponse{Present: present, FetchedMs: time.Since(start).Milliseconds()}
		if err != nil {
			err = status.Error(codes.FailedPrecondition, workspace.RedactURLs(err.Error()))
		}
	}
	w.s.Audit.Log(IdentityFrom(ctx), pb.Workspace_Fetch_FullMethodName, map[string]any{
		"repo": req.Repo, "url": workspace.RedactURLs(req.Url), "commits": req.Commits, "present": len(res.GetPresent()),
		"git_auth": true, "error": errString(err),
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (w *workspaceServer) Sync(st pb.Workspace_SyncServer) error {
	ctx := st.Context()
	first, err := st.Recv()
	if err != nil {
		return err
	}
	h := first.GetHeader()
	if h == nil {
		return status.Error(codes.InvalidArgument, "first message must be the header")
	}
	if !workspace.ValidRepo(h.Repo) {
		return status.Errorf(codes.InvalidArgument, "bad repo %q", h.Repo)
	}
	if w.s.perUser() {
		return w.proxySync(st, first)
	}
	var dst string
	if !h.StageOnly {
		if dst, err = resolvePath(h.Path); err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
	}
	tmp, err := os.MkdirTemp(filepath.Join(w.s.DataDir, workspace.StagingDir), "upload-")
	if errors.Is(err, os.ErrNotExist) {
		os.MkdirAll(filepath.Join(w.s.DataDir, workspace.StagingDir), 0o755)
		tmp, err = os.MkdirTemp(filepath.Join(w.s.DataDir, workspace.StagingDir), "upload-")
	}
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	defer os.RemoveAll(tmp)

	names := map[pb.SyncPart]string{
		pb.SyncPart_SYNC_PART_BUNDLE: "bundle", pb.SyncPart_SYNC_PART_PATCH: "patch",
		pb.SyncPart_SYNC_PART_UNTRACKED_TAR: "untracked.tar", pb.SyncPart_SYNC_PART_LFS_TAR: "lfs.tar",
	}
	lfsMax := w.s.ws().MaxLFSUploadBytes()
	files := map[pb.SyncPart]*os.File{}
	sizes := map[pb.SyncPart]int64{}
	closeAll := func() {
		for _, f := range files {
			f.Close()
		}
	}
	for {
		msg, err := st.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			closeAll()
			return err
		}
		d := msg.GetData()
		if d == nil {
			closeAll()
			return status.Error(codes.InvalidArgument, "expected data")
		}
		name, ok := names[d.Part]
		if !ok {
			closeAll()
			return status.Errorf(codes.InvalidArgument, "unknown part %v", d.Part)
		}
		f := files[d.Part]
		if f == nil {
			if f, err = os.Create(filepath.Join(tmp, name)); err != nil {
				closeAll()
				return status.Error(codes.Internal, err.Error())
			}
			files[d.Part] = f
		}
		if d.Part == pb.SyncPart_SYNC_PART_LFS_TAR && sizes[d.Part]+int64(len(d.Data)) > lfsMax {
			closeAll()
			return status.Errorf(codes.ResourceExhausted, "lfs objects: more than the worker's limit of %d bytes per upload", lfsMax)
		}
		if _, err := f.Write(d.Data); err != nil {
			closeAll()
			return status.Error(codes.Internal, err.Error())
		}
		sizes[d.Part] += int64(len(d.Data))
	}
	closeAll()
	path := func(p pb.SyncPart) string {
		if sizes[p] == 0 {
			return ""
		}
		return filepath.Join(tmp, names[p])
	}
	res := &pb.SyncResult{
		Commit: h.Commit, BundleBytes: sizes[pb.SyncPart_SYNC_PART_BUNDLE],
		PatchBytes: sizes[pb.SyncPart_SYNC_PART_PATCH], UntrackedBytes: sizes[pb.SyncPart_SYNC_PART_UNTRACKED_TAR],
		LfsBytes: sizes[pb.SyncPart_SYNC_PART_LFS_TAR],
	}
	err = w.s.ws().ImportBundle(ctx, h.Repo, path(pb.SyncPart_SYNC_PART_BUNDLE), h.Branch, h.Commit)
	if err == nil && res.LfsBytes > 0 {
		_, err = w.s.ws().ImportLFS(ctx, h.Repo, path(pb.SyncPart_SYNC_PART_LFS_TAR))
	}
	if err == nil {
		if h.StageOnly {
			res.StagingId = ulid.Make().String()
			err = w.s.ws().Stage(res.StagingId, path(pb.SyncPart_SYNC_PART_PATCH), path(pb.SyncPart_SYNC_PART_UNTRACKED_TAR))
		} else {
			res.Path = dst
			err = w.s.ws().Checkout(ctx, h.Repo, h.Commit, dst, path(pb.SyncPart_SYNC_PART_PATCH),
				path(pb.SyncPart_SYNC_PART_UNTRACKED_TAR), nil)
		}
	}
	w.s.Audit.Log(IdentityFrom(ctx), pb.Workspace_Sync_FullMethodName, map[string]any{
		"repo": h.Repo, "commit": h.Commit, "path": dst, "stage_only": h.StageOnly,
		"bundle_bytes": res.BundleBytes, "patch_bytes": res.PatchBytes, "untracked_bytes": res.UntrackedBytes,
		"lfs_bytes": res.LfsBytes, "error": errString(err),
	})
	if err != nil {
		// Aborted, not FailedPrecondition: importing / checking out can fail
		// for passing reasons (a git lock, a full disk); clients retry
		return status.Error(codes.Aborted, err.Error())
	}
	return st.SendAndClose(res)
}
