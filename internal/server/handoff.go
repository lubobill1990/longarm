package server

// Direct worker-to-worker transfer of a handoff's commits (worker feature
// "handoff-direct", docs/usage.md#hand-off-an-agent-session).
//
// Trust: the grant is the owner's own word, given over the owner's
// end-to-end mTLS connections to both workers - the hub never carries it,
// so it can neither forge nor widen it. The source worker (Offer) records:
// this one file, for this one peer worker, for whoever presents this
// random token, until this expiry, once. The target worker (Pull) dials
// the source with its worker certificate over the port-link legs; both
// sides apply the port-link rules for the owner (LinkOwnerDeny: a user of
// the worker, owner / shares, disabled users, pinned key) plus handoff's
// own: a worker with an owner takes part for its owner only - never for a
// user it is shared with, never for an admin of another user
// (handoffDeny) - and, with end-to-end trust, demand that the other worker
// is endorsed by the owner (LinkPeerTrustDeny).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/workspace"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Limits of handoff transfers (variables for tests).
var (
	handoffMaxTTL   = 10 * time.Minute
	handoffMaxBytes = int64(16 << 30)
)

var transferIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// bundleNameRE: the only files Offer serves - the bundles longarm handoff
// creates in a checkout's git directory.
var bundleNameRE = regexp.MustCompile(`^longarm-handoff-[A-Za-z0-9_-]{1,80}\.bundle$`)

// PeerDialer connects to another worker with this worker's certificate
// over the port-link legs; close releases the connection.
type PeerDialer func(worker string, endpoints []string) (conn grpc.ClientConnInterface, close func(), err error)

// handoffDeny says why user may not hand a session off to or from this
// worker ("" = may): the port-link rules (LinkOwnerDeny), and a worker with
// an owner (the hub's WorkerAccess, or [access] owners) serves handoffs of
// its owner only. Sharing a worker lets others run jobs there, not move
// agent sessions and their code in or out; admin certificates are no
// exception (they act as their user).
func (s *Server) handoffDeny(user string) string {
	if why := s.LinkOwnerDeny(user); why != "" {
		return why
	}
	if (s.WorkerAccess.Owner() != "" || (s.Access != nil && len(s.Access.Owners) > 0)) && !s.machineOwner(user) {
		return "this worker is another user's: handoff moves sessions only between your own machines"
	}
	return ""
}

// offer is one registered transfer.
type offer struct {
	id, owner, peer string
	token           []byte
	file            string // in this worker's staging area
	size            int64
	expires         time.Time
	used            bool
}

type handoffServer struct {
	pb.UnimplementedHandoffServer
	s *Server

	mu     sync.Mutex
	offers map[string]*offer
}

func (h *handoffServer) dir() string {
	return filepath.Join(h.s.DataDir, workspace.StagingDir, "handoff")
}

// drop removes an offer and its file.
func (h *handoffServer) drop(id string) {
	h.mu.Lock()
	o := h.offers[id]
	delete(h.offers, id)
	h.mu.Unlock()
	if o != nil {
		os.Remove(o.file)
	}
}

func (h *handoffServer) Offer(ctx context.Context, req *pb.HandoffOfferRequest) (*pb.HandoffOfferResponse, error) {
	id := IdentityFrom(ctx)
	owner := id.Owner()
	fields := map[string]any{"transfer": req.TransferId, "peer": req.Peer, "path": req.Path}
	deny := func(code codes.Code, format string, a ...any) (*pb.HandoffOfferResponse, error) {
		err := status.Errorf(code, format, a...)
		fields["denied"] = status.Convert(err).Message()
		h.s.Audit.Log(id, pb.Handoff_Offer_FullMethodName, fields)
		return nil, err
	}
	ttl := req.Ttl.AsDuration()
	switch {
	case !transferIDRE.MatchString(req.TransferId):
		return deny(codes.InvalidArgument, "bad transfer id")
	case len(req.Token) < 16:
		return deny(codes.InvalidArgument, "token too short")
	case req.Peer == "" || req.Peer == h.s.Name:
		return deny(codes.InvalidArgument, "bad peer %q", req.Peer)
	case ttl <= 0 || ttl > handoffMaxTTL:
		return deny(codes.InvalidArgument, "ttl must be in (0, %s]", handoffMaxTTL)
	case req.MaxBytes <= 0 || req.MaxBytes > handoffMaxBytes:
		return deny(codes.InvalidArgument, "max_bytes must be in (0, %d]", handoffMaxBytes)
	case !bundleNameRE.MatchString(filepath.Base(filepath.FromSlash(req.Path))):
		return deny(codes.InvalidArgument, "only bundles longarm handoff made can be offered")
	}
	if why := h.s.handoffDeny(owner); why != "" {
		return deny(codes.PermissionDenied, "%s", why)
	}
	h.mu.Lock()
	if h.offers == nil {
		h.offers = map[string]*offer{}
	}
	_, taken := h.offers[req.TransferId]
	h.mu.Unlock()
	if taken {
		return deny(codes.AlreadyExists, "transfer %s exists", req.TransferId)
	}
	// a copy in longarmd's own staging area, read with the caller's rights
	if err := os.MkdirAll(h.dir(), 0o700); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	file := filepath.Join(h.dir(), req.TransferId+".bundle")
	out, err := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return deny(codes.AlreadyExists, "transfer %s: %v", req.TransferId, err)
	}
	sum := sha256.New()
	n, err := h.readAsCaller(ctx, req.Path, io.MultiWriter(out, sum), req.MaxBytes)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(file)
		fields["error"] = errString(err)
		h.s.Audit.Log(id, pb.Handoff_Offer_FullMethodName, fields)
		if status.Code(err) != codes.Unknown {
			return nil, err
		}
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	o := &offer{id: req.TransferId, owner: owner, peer: req.Peer, token: bytes.Clone(req.Token), file: file, size: n,
		expires: time.Now().Add(ttl)}
	h.mu.Lock()
	h.offers[o.id] = o
	h.mu.Unlock()
	time.AfterFunc(ttl, func() { h.drop(o.id) }) // single use or expiry, whichever comes first
	fields["bytes"] = n
	h.s.Audit.Log(id, pb.Handoff_Offer_FullMethodName, fields)
	return &pb.HandoffOfferResponse{Size: n, Sha256: sum.Sum(nil)}, nil
}

// readAsCaller copies the file at p, read with the caller's rights (per-user
// isolation: through the user's helper), into w: a regular file, not a
// symlink, at most limit bytes.
func (h *handoffServer) readAsCaller(ctx context.Context, p string, w io.Writer, limit int64) (int64, error) {
	var stat func(context.Context, *pb.FileStatRequest) (*pb.FileInfo, error)
	var read func(context.Context, *pb.FileReadRequest) (grpc.ServerStreamingClient[pb.FileChunk], error)
	if h.s.perUser() {
		hp, err := h.s.helper(ctx)
		if err != nil {
			return 0, err
		}
		defer hp.Close()
		fc := pb.NewFilesClient(hp.Conn)
		stat = func(ctx context.Context, r *pb.FileStatRequest) (*pb.FileInfo, error) { return fc.Stat(ctx, r) }
		read = func(ctx context.Context, r *pb.FileReadRequest) (grpc.ServerStreamingClient[pb.FileChunk], error) {
			return fc.Read(ctx, r)
		}
	} else {
		abs, err := resolvePath(p)
		if err != nil {
			return 0, status.Error(codes.InvalidArgument, err.Error())
		}
		fi, err := os.Lstat(abs)
		if err != nil {
			return 0, fileErr(err)
		}
		if !fi.Mode().IsRegular() {
			return 0, status.Errorf(codes.InvalidArgument, "%s is not a regular file", abs)
		}
		if fi.Size() > limit {
			return 0, status.Errorf(codes.ResourceExhausted, "%s is larger than %d bytes", abs, limit)
		}
		f, err := platform.OpenShared(abs)
		if err != nil {
			return 0, fileErr(err)
		}
		defer f.Close()
		n, err := io.Copy(w, io.LimitReader(f, limit+1))
		if err == nil && n > limit {
			err = status.Errorf(codes.ResourceExhausted, "%s is larger than %d bytes", abs, limit)
		}
		return n, err
	}
	fi, err := stat(ctx, &pb.FileStatRequest{Path: p, NoFollow: true})
	if err != nil {
		return 0, err
	}
	if !fi.Exists || fi.IsDir || fi.IsSymlink {
		return 0, status.Errorf(codes.InvalidArgument, "%s is not a regular file", p)
	}
	if fi.Size > limit {
		return 0, status.Errorf(codes.ResourceExhausted, "%s is larger than %d bytes", p, limit)
	}
	st, err := read(ctx, &pb.FileReadRequest{Path: p})
	if err != nil {
		return 0, err
	}
	var n int64
	for {
		ch, err := st.Recv()
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		if n += int64(len(ch.Data)); n > limit {
			return n, status.Errorf(codes.ResourceExhausted, "%s is larger than %d bytes", p, limit)
		}
		if _, err := w.Write(ch.Data); err != nil {
			return n, err
		}
	}
}

func (h *handoffServer) Fetch(req *pb.HandoffFetchRequest, st pb.Handoff_FetchServer) error {
	ctx := st.Context()
	id := IdentityFrom(ctx)
	fields := map[string]any{"transfer": req.TransferId}
	deny := func(code codes.Code, format string, a ...any) error {
		err := status.Errorf(code, format, a...)
		fields["denied"] = status.Convert(err).Message()
		h.s.Audit.Log(id, pb.Handoff_Fetch_FullMethodName, fields)
		return err
	}
	h.mu.Lock()
	o := h.offers[req.TransferId]
	switch {
	case o == nil:
		h.mu.Unlock()
		return deny(codes.NotFound, "no transfer %s (expired, used or never offered)", req.TransferId)
	case o.peer != id.Name:
		h.mu.Unlock()
		return deny(codes.PermissionDenied, "transfer %s is not for %s", req.TransferId, id.Name)
	case subtle.ConstantTimeCompare(o.token, req.Token) != 1:
		h.mu.Unlock()
		return deny(codes.PermissionDenied, "transfer %s: wrong token", req.TransferId)
	case time.Now().After(o.expires):
		h.mu.Unlock()
		go h.drop(o.id)
		return deny(codes.NotFound, "transfer %s expired", req.TransferId)
	case o.used:
		h.mu.Unlock()
		return deny(codes.NotFound, "transfer %s was used already", req.TransferId)
	}
	o.used = true // single use: whatever happens next
	h.mu.Unlock()
	defer h.drop(o.id)
	fields["owner"], fields["peer"] = o.owner, id.Name
	if why := h.s.handoffDeny(o.owner); why != "" {
		return deny(codes.PermissionDenied, "%s", why)
	}
	if why := LinkPeerTrustDeny(h.s.Trust.Load(), peerCert(ctx), o.owner); why != "" {
		return deny(codes.PermissionDenied, "%s", why)
	}
	f, err := os.Open(o.file)
	if err != nil {
		return deny(codes.NotFound, "transfer %s: %v", req.TransferId, err)
	}
	defer f.Close()
	fields["bytes"] = o.size
	h.s.Audit.Log(id, pb.Handoff_Fetch_FullMethodName, fields)
	buf := make([]byte, fileChunk)
	var off int64
	for {
		n, err := f.Read(buf)
		if n > 0 {
			if err := st.Send(&pb.FileChunk{Offset: off, Data: buf[:n]}); err != nil {
				return err
			}
			off += int64(n)
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return status.Error(codes.Internal, err.Error())
		}
	}
}

func (h *handoffServer) Pull(ctx context.Context, req *pb.HandoffPullRequest) (*pb.SyncResult, error) {
	id := IdentityFrom(ctx)
	owner := id.Owner()
	hdr := req.GetHeader()
	fields := map[string]any{"transfer": req.TransferId, "from": req.From, "repo": hdr.GetRepo(), "commit": hdr.GetCommit(), "path": hdr.GetPath()}
	fail := func(err error) (*pb.SyncResult, error) {
		fields["error"] = errString(err)
		h.s.Audit.Log(id, pb.Handoff_Pull_FullMethodName, fields)
		return nil, err
	}
	switch {
	case hdr == nil || hdr.StageOnly || !workspace.ValidRepo(hdr.Repo) || hdr.Path == "":
		return fail(status.Error(codes.InvalidArgument, "bad header"))
	case !transferIDRE.MatchString(req.TransferId) || req.From == "" || len(req.FromEndpoints) == 0:
		return fail(status.Error(codes.InvalidArgument, "bad transfer"))
	case req.Size <= 0 || req.Size > handoffMaxBytes || len(req.Sha256) != sha256.Size:
		return fail(status.Error(codes.InvalidArgument, "bad size or sha256"))
	}
	if why := h.s.handoffDeny(owner); why != "" {
		return fail(status.Errorf(codes.PermissionDenied, "%s", why))
	}
	if h.s.PeerDial == nil {
		return fail(status.Error(codes.FailedPrecondition, "this worker does not dial other workers"))
	}
	conn, closeConn, err := h.s.PeerDial(req.From, req.FromEndpoints)
	if err != nil {
		return fail(status.Errorf(codes.Unavailable, "connect to %s: %v", req.From, err))
	}
	defer closeConn()
	if err := os.MkdirAll(h.dir(), 0o700); err != nil {
		return fail(status.Error(codes.Internal, err.Error()))
	}
	tmp, err := os.CreateTemp(h.dir(), "pull-*.bundle")
	if err != nil {
		return fail(status.Error(codes.Internal, err.Error()))
	}
	defer os.Remove(tmp.Name())
	var p peer.Peer
	fctx, cancel := context.WithTimeout(ctx, time.Hour)
	defer cancel()
	st, err := pb.NewHandoffClient(conn).Fetch(fctx, &pb.HandoffFetchRequest{TransferId: req.TransferId, Token: req.Token})
	if err != nil {
		tmp.Close()
		return fail(err)
	}
	// with end-to-end trust the source must be endorsed by the owner: checked
	// before a byte is taken
	if _, err := st.Header(); err != nil {
		tmp.Close()
		return fail(err)
	}
	if pp, ok := peer.FromContext(st.Context()); ok {
		p = *pp
	}
	if why := LinkPeerTrustDeny(h.s.Trust.Load(), clientPeerCert(&p), owner); why != "" {
		tmp.Close()
		return fail(status.Error(codes.PermissionDenied, why))
	}
	sum := sha256.New()
	var n int64
	for {
		ch, err := st.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			tmp.Close()
			return fail(err)
		}
		if n += int64(len(ch.Data)); n > req.Size {
			tmp.Close()
			return fail(status.Errorf(codes.DataLoss, "%s sent more than the %d bytes offered", req.From, req.Size))
		}
		tmp.Write(ch.Data)
		sum.Write(ch.Data)
	}
	if err := tmp.Close(); err != nil {
		return fail(status.Error(codes.Internal, err.Error()))
	}
	if n != req.Size || !bytes.Equal(sum.Sum(nil), req.Sha256) {
		return fail(status.Errorf(codes.DataLoss, "the bundle from %s is not the one offered (size or sha256)", req.From))
	}
	fields["bytes"] = n
	res, err := h.importBundle(ctx, hdr, tmp.Name(), n)
	if err != nil {
		return fail(err)
	}
	h.s.Audit.Log(id, pb.Handoff_Pull_FullMethodName, fields)
	return res, nil
}

// importBundle makes hdr.Path a worktree of the bundle like Workspace.Sync
// (per-user isolation: Sync of the caller's helper, streaming the file).
func (h *handoffServer) importBundle(ctx context.Context, hdr *pb.SyncHeader, bundle string, size int64) (*pb.SyncResult, error) {
	if h.s.perUser() {
		hp, err := h.s.helper(ctx)
		if err != nil {
			return nil, err
		}
		defer hp.Close()
		cs, err := pb.NewWorkspaceClient(hp.Conn).Sync(ctx)
		if err != nil {
			return nil, err
		}
		if err := cs.Send(&pb.SyncChunk{Msg: &pb.SyncChunk_Header{Header: hdr}}); err != nil {
			_, rerr := cs.CloseAndRecv()
			return nil, rerr
		}
		f, err := os.Open(bundle)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		defer f.Close()
		buf := make([]byte, fileChunk)
		for {
			n, rerr := f.Read(buf)
			if n > 0 {
				if err := cs.Send(&pb.SyncChunk{Msg: &pb.SyncChunk_Data{Data: &pb.SyncData{Part: pb.SyncPart_SYNC_PART_BUNDLE, Data: buf[:n]}}}); err != nil {
					_, cerr := cs.CloseAndRecv()
					return nil, cerr
				}
			}
			if errors.Is(rerr, io.EOF) {
				break
			}
			if rerr != nil {
				return nil, status.Error(codes.Internal, rerr.Error())
			}
		}
		return cs.CloseAndRecv()
	}
	dst, err := resolvePath(hdr.Path)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	// the bundle's prerequisites must be in the mirror and it must carry
	// the commit (git fetch from the bundle checks both)
	if err := h.s.ws().ImportBundle(ctx, hdr.Repo, bundle, hdr.Branch, hdr.Commit); err != nil {
		return nil, status.Error(codes.Aborted, err.Error())
	}
	if err := h.s.ws().Checkout(ctx, hdr.Repo, hdr.Commit, dst, "", "", nil); err != nil {
		return nil, status.Error(codes.Aborted, err.Error())
	}
	return &pb.SyncResult{Path: dst, Commit: hdr.Commit, BundleBytes: size}, nil
}

// clientPeerCert is the server certificate of a client call.
func clientPeerCert(p *peer.Peer) *x509.Certificate {
	if p == nil {
		return nil
	}
	ti, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(ti.State.PeerCertificates) == 0 {
		return nil
	}
	return ti.State.PeerCertificates[0]
}
