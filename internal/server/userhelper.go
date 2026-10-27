package server

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/userspace"
	"github.com/lubobill1990/longarm/internal/workspace"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Per-user isolation: Files and Workspace calls are served by a helper
// process running as the caller's OS account (`longarmd user-helper`,
// ServeUserHelper below), so the file system is only ever touched with the
// user's own rights - never with longarmd's. longarmd authorizes, audits
// and relays.

// helper starts the caller's user helper.
func (s *Server) helper(ctx context.Context) (*userspace.Helper, error) {
	sp, err := s.spaceFor(IdentityFrom(ctx).Owner())
	if err != nil {
		return nil, err
	}
	h, err := sp.StartHelper(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return h, nil
}

func (f *filesServer) proxyStat(ctx context.Context, req *pb.FileStatRequest) (*pb.FileInfo, error) {
	h, err := f.s.helper(ctx)
	if err != nil {
		return nil, err
	}
	defer h.Close()
	return pb.NewFilesClient(h.Conn).Stat(ctx, req)
}

func (f *filesServer) proxyList(ctx context.Context, req *pb.FileListRequest) (*pb.FileListResponse, error) {
	h, err := f.s.helper(ctx)
	if err != nil {
		return nil, err
	}
	defer h.Close()
	return pb.NewFilesClient(h.Conn).List(ctx, req)
}

func (f *filesServer) proxyMkdir(ctx context.Context, req *pb.MkdirRequest) (*pb.FileInfo, error) {
	h, err := f.s.helper(ctx)
	if err != nil {
		return nil, err
	}
	defer h.Close()
	fi, err := pb.NewFilesClient(h.Conn).Mkdir(ctx, req)
	f.s.Audit.Log(IdentityFrom(ctx), pb.Files_Mkdir_FullMethodName, map[string]any{"path": req.Path, "error": errString(err)})
	return fi, err
}

func (f *filesServer) proxyRead(req *pb.FileReadRequest, st pb.Files_ReadServer) error {
	ctx := st.Context()
	h, err := f.s.helper(ctx)
	if err != nil {
		return err
	}
	defer h.Close()
	f.s.Audit.Log(IdentityFrom(ctx), pb.Files_Read_FullMethodName, map[string]any{"path": req.Path, "offset": req.Offset})
	rs, err := pb.NewFilesClient(h.Conn).Read(ctx, req)
	if err != nil {
		return err
	}
	for {
		c, err := rs.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := st.Send(c); err != nil {
			return err
		}
	}
}

func (f *filesServer) proxyWrite(st pb.Files_WriteServer, first *pb.FileWriteRequest) error {
	ctx := st.Context()
	h, err := f.s.helper(ctx)
	if err != nil {
		return err
	}
	defer h.Close()
	hdr := first.GetHeader()
	res, err := relayClientStream[pb.FileWriteRequest, pb.FileWriteResult](ctx, first, st.Recv,
		func(ctx context.Context) (grpc.ClientStreamingClient[pb.FileWriteRequest, pb.FileWriteResult], error) {
			return pb.NewFilesClient(h.Conn).Write(ctx)
		})
	f.s.Audit.Log(IdentityFrom(ctx), pb.Files_Write_FullMethodName, map[string]any{
		"path": hdr.GetPath(), "offset": hdr.GetOffset(), "size": hdr.GetSize(), "error": errString(err),
	})
	if err != nil {
		return err
	}
	return st.SendAndClose(res)
}

func (w *workspaceServer) proxyRefs(ctx context.Context, req *pb.RefsRequest) (*pb.RefsResponse, error) {
	h, err := w.s.helper(ctx)
	if err != nil {
		return nil, err
	}
	defer h.Close()
	return pb.NewWorkspaceClient(h.Conn).Refs(ctx, req)
}

func (w *workspaceServer) proxyFetch(ctx context.Context, req *pb.FetchRequest) (*pb.FetchResponse, error) {
	h, err := w.s.helper(ctx)
	if err != nil {
		return nil, err
	}
	defer h.Close()
	return pb.NewWorkspaceClient(h.Conn).Fetch(ctx, req)
}

func (w *workspaceServer) proxySync(st pb.Workspace_SyncServer, first *pb.SyncChunk) error {
	ctx := st.Context()
	h, err := w.s.helper(ctx)
	if err != nil {
		return err
	}
	defer h.Close()
	hdr := first.GetHeader()
	res, err := relayClientStream[pb.SyncChunk, pb.SyncResult](ctx, first, st.Recv,
		func(ctx context.Context) (grpc.ClientStreamingClient[pb.SyncChunk, pb.SyncResult], error) {
			return pb.NewWorkspaceClient(h.Conn).Sync(ctx)
		})
	w.s.Audit.Log(IdentityFrom(ctx), pb.Workspace_Sync_FullMethodName, map[string]any{
		"repo": hdr.GetRepo(), "commit": hdr.GetCommit(), "path": hdr.GetPath(), "stage_only": hdr.GetStageOnly(),
		"bundle_bytes": res.GetBundleBytes(), "patch_bytes": res.GetPatchBytes(), "untracked_bytes": res.GetUntrackedBytes(),
		"lfs_bytes": res.GetLfsBytes(), "error": errString(err),
	})
	if err != nil {
		return err
	}
	return st.SendAndClose(res)
}

// relayClientStream forwards a client stream (first message, then recv
// until EOF) to the helper and returns its answer.
func relayClientStream[Req, Res any](ctx context.Context, first *Req, recv func() (*Req, error),
	open func(context.Context) (grpc.ClientStreamingClient[Req, Res], error)) (*Res, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cs, err := open(ctx)
	if err != nil {
		return nil, err
	}
	if err := cs.Send(first); err != nil {
		_, rerr := cs.CloseAndRecv()
		return nil, rerr
	}
	for {
		m, err := recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err // the client went away: cancel the helper's call
		}
		if err := cs.Send(m); err != nil {
			_, rerr := cs.CloseAndRecv() // the helper refused: its error
			return nil, rerr
		}
	}
	return cs.CloseAndRecv()
}

// ---------------------------------------------------------------- helper side

// ServeUserHelper is `longarmd user-helper --root <space>`: it runs as a
// user's OS account and serves Files, Workspace and UserHelper for longarmd
// on one connection over in/out (its stdin/stdout), then exits.
func ServeUserHelper(root string, in io.Reader, out io.WriteCloser) error {
	srv := &Server{DataDir: root, helperWS: &workspace.Store{Root: root}}
	g := grpc.NewServer(grpc.MaxRecvMsgSize(64<<20), grpc.MaxSendMsgSize(64<<20))
	pb.RegisterFilesServer(g, &filesServer{s: srv})
	pb.RegisterWorkspaceServer(g, &workspaceServer{s: srv})
	pb.RegisterUserHelperServer(g, &userHelperServer{ws: srv.helperWS})
	ln := newOneConnListener(userspace.NewStdioConn(in, out))
	go func() { <-ln.closed; g.Stop() }()
	err := g.Serve(ln)
	if errors.Is(err, net.ErrClosed) || errors.Is(err, grpc.ErrServerStopped) {
		return nil
	}
	return err
}

type userHelperServer struct {
	pb.UnimplementedUserHelperServer
	ws *workspace.Store
}

// streamWriter turns writes into PrepareJobOutput messages.
type streamWriter struct {
	mu sync.Mutex
	st pb.UserHelper_PrepareJobServer
}

func (w *streamWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.st.Send(&pb.PrepareJobOutput{Log: append([]byte(nil), p...)}); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (u *userHelperServer) PrepareJob(req *pb.PrepareJobRequest, st pb.UserHelper_PrepareJobServer) error {
	w := req.GetWorkspace()
	if w == nil || !jobs.ValidID(req.JobId) || !workspace.ValidRepo(w.Repo) {
		return status.Error(codes.InvalidArgument, "bad prepare request")
	}
	spec := jobs.Spec{ID: req.JobId, Workspace: &jobs.Workspace{Repo: w.Repo, URL: w.Url, Ref: w.Ref, Commit: w.Commit,
		StagingID: w.StagingId, LFSFetch: w.LfsFetch}}
	auth := GitAuthFromPB(req.GitAuth)
	defer auth.Clear()
	dir, err := u.ws.PrepareJob(st.Context(), "", spec, req.SecretEnv, auth, &streamWriter{st: st})
	if err != nil {
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	return st.Send(&pb.PrepareJobOutput{Workdir: dir})
}

func (u *userHelperServer) Prune(ctx context.Context, _ *pb.PruneRequest) (*pb.PruneResponse, error) {
	u.ws.PruneAll(ctx)
	return &pb.PruneResponse{}, nil
}

// oneConnListener hands out one connection, then blocks until it closes.
type oneConnListener struct {
	mu     sync.Mutex
	conn   net.Conn
	closed chan struct{}
	once   sync.Once
}

func newOneConnListener(c net.Conn) *oneConnListener {
	l := &oneConnListener{closed: make(chan struct{})}
	l.conn = &notifyConn{Conn: c, onClose: func() { l.Close() }}
	return l
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	c := l.conn
	l.conn = nil
	l.mu.Unlock()
	if c != nil {
		return c, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}

func (l *oneConnListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *oneConnListener) Addr() net.Addr { return stdioAddr{} }

type stdioAddr struct{}

func (stdioAddr) Network() string { return "stdio" }
func (stdioAddr) String() string  { return "stdio" }

// notifyConn reports the end of the connection (EOF on stdin or Close).
type notifyConn struct {
	net.Conn
	onClose func()
}

func (c *notifyConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if err != nil {
		c.onClose()
	}
	return n, err
}

func (c *notifyConn) Close() error {
	c.onClose()
	return c.Conn.Close()
}
