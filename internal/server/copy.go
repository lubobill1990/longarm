package server

// Worker-to-worker copies of files and directory trees (longarm cp, worker
// feature "copy", docs/usage.md "Copy between workers").
//
// Trust, as for handoff.go: the grant is the owner's own word, given over
// the owner's end-to-end mTLS connections to both workers - the hub never
// carries it. The source worker (Offer) records: this path and what is
// below it, read as the owner, for this one peer worker (name and node ID),
// for whoever presents this random token, until this expiry or a Revoke.
// Unlike a handoff offer it may be read many times: a copy that broke off
// is pulled again and resumes. The target worker (Pull) dials the source
// with its worker certificate over the port-link legs and writes as the
// owner. Both sides apply the handoff rules for the owner on every call
// (ownMachineDeny: a worker with an owner copies for its owner only) and,
// with end-to-end trust, demand that the other worker is endorsed by the
// owner (LinkPeerTrustDeny).

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"longarm/internal/pki"
	"longarm/internal/platform"
	pb "longarm/proto/longarm/v1"
)

// Limits of copies (variables for tests).
var (
	copyMaxTTL    = 24 * time.Hour
	copyMaxOffers = 64
	// copyRetries: attempts per file after a broken read or write (the
	// next one resumes from the partial file)
	copyRetries = 3
	copyBackoff = time.Second
)

func (s *Server) copyDeny(user string) string {
	return s.ownMachineDeny(user, "this worker is another user's: longarm cp copies only between your own machines")
}

// copyOffer is one registered copy.
type copyOffer struct {
	id, owner, peer string
	peerID          string // the peer's node ID ("" = by name only)
	token           []byte
	root            string // absolute, on this worker
	isDir           bool
	speed           bool // a speed test's: Source and Sink only, no files
	expires         time.Time
	timer           *time.Timer
}

// path is the absolute path of rel (relative to the offered path, '/'
// separated; "" = the offered path).
func (o *copyOffer) path(rel string) (string, error) {
	if rel == "" {
		return o.root, nil
	}
	if !o.isDir || !validCopyRel(rel) {
		return "", status.Errorf(codes.InvalidArgument, "%q is not a path in the offered directory", rel)
	}
	return filepath.Join(o.root, filepath.FromSlash(rel)), nil
}

// validCopyRel: a relative, '/'-separated path without "", "." or ".."
// elements (and, on Windows, without what Windows reads as a separator,
// drive or stream).
func validCopyRel(rel string) bool {
	if rel == "" || strings.HasPrefix(rel, "/") || strings.ContainsRune(rel, 0) {
		return false
	}
	if runtime.GOOS == "windows" && strings.ContainsAny(rel, `\:`) {
		return false
	}
	for _, e := range strings.Split(rel, "/") {
		if e == "" || e == "." || e == ".." {
			return false
		}
	}
	return true
}

type copyServer struct {
	pb.UnimplementedCopyServer
	s *Server

	mu     sync.Mutex
	offers map[string]*copyOffer
}

func (c *copyServer) drop(id string) {
	c.mu.Lock()
	if o := c.offers[id]; o != nil {
		o.timer.Stop()
		delete(c.offers, id)
	}
	c.mu.Unlock()
}

func (c *copyServer) Offer(ctx context.Context, req *pb.CopyOfferRequest) (*pb.CopyOfferResponse, error) {
	id := IdentityFrom(ctx)
	owner := id.Owner()
	fields := map[string]any{"transfer": req.TransferId, "peer": req.Peer, "path": req.Path}
	deny := func(err error) (*pb.CopyOfferResponse, error) {
		fields["denied"] = status.Convert(err).Message()
		c.s.Audit.Log(id, pb.Copy_Offer_FullMethodName, fields)
		return nil, err
	}
	ttl := req.Ttl.AsDuration()
	switch {
	case !transferIDRE.MatchString(req.TransferId):
		return deny(status.Error(codes.InvalidArgument, "bad transfer id"))
	case len(req.Token) < 16:
		return deny(status.Error(codes.InvalidArgument, "token too short"))
	case req.Peer == "" || req.Peer == c.s.Name:
		return deny(status.Errorf(codes.InvalidArgument, "bad peer %q", req.Peer))
	case ttl <= 0 || ttl > copyMaxTTL:
		return deny(status.Errorf(codes.InvalidArgument, "ttl must be in (0, %s]", copyMaxTTL))
	case req.Speed && req.Path != "":
		return deny(status.Error(codes.InvalidArgument, "a speed test's offer has no path"))
	case req.Path == "" && !req.Speed:
		return deny(status.Error(codes.InvalidArgument, "no path"))
	}
	if why := c.s.copyDeny(owner); why != "" {
		return deny(status.Error(codes.PermissionDenied, why))
	}
	fields["speed"] = req.Speed
	c.mu.Lock()
	_, taken := c.offers[req.TransferId]
	n := len(c.offers)
	c.mu.Unlock()
	switch {
	case taken:
		return deny(status.Errorf(codes.AlreadyExists, "transfer %s exists", req.TransferId))
	case n >= copyMaxOffers:
		return deny(status.Errorf(codes.ResourceExhausted, "%d copies are offered already (max %d)", n, copyMaxOffers))
	}
	root := &pb.FileInfo{}
	if !req.Speed {
		fs, err := c.s.filesAs(ctx, owner)
		if err != nil {
			return deny(err)
		}
		defer fs.Close()
		root, err = fs.Stat(ctx, &pb.FileStatRequest{Path: req.Path, NoFollow: true})
		switch {
		case err != nil:
			return deny(err)
		case !root.Exists:
			return deny(status.Errorf(codes.NotFound, "%s does not exist", root.AbsPath))
		case root.IsSymlink:
			return deny(status.Errorf(codes.InvalidArgument, "%s is a symlink", root.AbsPath))
		}
	}
	o := &copyOffer{id: req.TransferId, owner: owner, peer: req.Peer, peerID: strings.ToLower(req.PeerId), token: bytes.Clone(req.Token),
		root: root.AbsPath, isDir: root.IsDir, speed: req.Speed, expires: time.Now().Add(ttl)}
	o.timer = time.AfterFunc(ttl, func() { c.drop(o.id) })
	c.mu.Lock()
	if c.offers == nil {
		c.offers = map[string]*copyOffer{}
	}
	if _, taken := c.offers[o.id]; taken { // offered meanwhile
		c.mu.Unlock()
		o.timer.Stop()
		return deny(status.Errorf(codes.AlreadyExists, "transfer %s exists", req.TransferId))
	}
	c.offers[o.id] = o
	c.mu.Unlock()
	fields["abs_path"], fields["dir"] = root.AbsPath, root.IsDir
	c.s.Audit.Log(id, pb.Copy_Offer_FullMethodName, fields)
	return &pb.CopyOfferResponse{Root: root}, nil
}

func (c *copyServer) Revoke(ctx context.Context, req *pb.CopyRevokeRequest) (*pb.CopyRevokeResponse, error) {
	id := IdentityFrom(ctx)
	c.mu.Lock()
	o := c.offers[req.TransferId]
	c.mu.Unlock()
	if o == nil || o.owner != id.Owner() { // another user's: as unknown
		return nil, status.Errorf(codes.NotFound, "no transfer %s", req.TransferId)
	}
	c.drop(o.id)
	c.s.Audit.Log(id, pb.Copy_Revoke_FullMethodName, map[string]any{"transfer": o.id})
	return &pb.CopyRevokeResponse{}, nil
}

// grant is the offer g names for the calling peer worker, checked as Fetch
// checks a handoff offer - on every call, so a revoked offer, a changed
// owner or trust ends a copy at its next read. speed: the call is a speed
// test's (Source, Sink) - a file offer never serves one, a speed offer never
// a file.
func (c *copyServer) grant(ctx context.Context, g *pb.CopyGrant, method string, fields map[string]any, speed bool) (*copyOffer, error) {
	id := IdentityFrom(ctx)
	fields["transfer"] = g.GetTransferId()
	deny := func(code codes.Code, format string, a ...any) (*copyOffer, error) {
		err := status.Errorf(code, format, a...)
		fields["denied"] = status.Convert(err).Message()
		c.s.Audit.Log(id, method, fields)
		return nil, err
	}
	c.mu.Lock()
	o := c.offers[g.GetTransferId()]
	c.mu.Unlock()
	switch {
	case o == nil || o.speed != speed:
		return deny(codes.NotFound, "no transfer %s (expired, revoked or never offered)", g.GetTransferId())
	case o.peer != id.Name || o.peerID != "" && !strings.EqualFold(o.peerID, id.ID):
		return deny(codes.PermissionDenied, "transfer %s is not for %s", o.id, id.Name)
	case subtle.ConstantTimeCompare(o.token, g.GetToken()) != 1:
		return deny(codes.PermissionDenied, "transfer %s: wrong token", o.id)
	case time.Now().After(o.expires):
		c.drop(o.id)
		return deny(codes.NotFound, "transfer %s expired", o.id)
	}
	fields["owner"] = o.owner
	if why := c.s.copyDeny(o.owner); why != "" {
		return deny(codes.PermissionDenied, "%s", why)
	}
	if why := LinkPeerTrustDeny(c.s.Trust.Load(), peerCert(ctx), o.owner); why != "" {
		return deny(codes.PermissionDenied, "%s", why)
	}
	return o, nil
}

func (c *copyServer) List(ctx context.Context, req *pb.CopyListRequest) (*pb.FileListResponse, error) {
	fields := map[string]any{}
	o, err := c.grant(ctx, req.Grant, pb.Copy_List_FullMethodName, fields, false)
	if err != nil {
		return nil, err
	}
	c.s.Audit.Log(IdentityFrom(ctx), pb.Copy_List_FullMethodName, fields)
	if !o.isDir {
		return &pb.FileListResponse{AbsPath: o.root}, nil
	}
	fs, err := c.s.filesAs(ctx, o.owner)
	if err != nil {
		return nil, err
	}
	defer fs.Close()
	return fs.List(ctx, &pb.FileListRequest{Path: o.root})
}

func (c *copyServer) Stat(ctx context.Context, req *pb.CopyStatRequest) (*pb.FileInfo, error) {
	o, err := c.grant(ctx, req.Grant, pb.Copy_Stat_FullMethodName, map[string]any{"path": req.Path}, false)
	if err != nil {
		return nil, err
	}
	p, err := o.path(req.Path)
	if err != nil {
		return nil, err
	}
	fs, err := c.s.filesAs(ctx, o.owner)
	if err != nil {
		return nil, err
	}
	defer fs.Close()
	fi, err := fs.Stat(ctx, &pb.FileStatRequest{Path: p, NoFollow: true, Hash: req.Hash, HashLimit: req.HashLimit})
	if err != nil {
		return nil, err
	}
	fi.Path = req.Path
	return fi, nil
}

func (c *copyServer) Read(req *pb.CopyReadRequest, st pb.Copy_ReadServer) error {
	ctx := st.Context()
	fields := map[string]any{"path": req.Path, "offset": req.Offset}
	o, err := c.grant(ctx, req.Grant, pb.Copy_Read_FullMethodName, fields, false)
	if err != nil {
		return err
	}
	p, err := o.path(req.Path)
	if err != nil {
		return err
	}
	fs, err := c.s.filesAs(ctx, o.owner)
	if err != nil {
		return err
	}
	defer fs.Close()
	// a regular file: never a link out of the offered tree
	fi, err := fs.Stat(ctx, &pb.FileStatRequest{Path: p, NoFollow: true})
	switch {
	case err != nil:
		return err
	case !fi.Exists:
		return status.Errorf(codes.NotFound, "%s does not exist", p)
	case fi.IsDir || fi.IsSymlink:
		return status.Errorf(codes.InvalidArgument, "%s is not a regular file", p)
	}
	c.s.Audit.Log(IdentityFrom(ctx), pb.Copy_Read_FullMethodName, fields)
	return fs.Read(ctx, &pb.FileReadRequest{Path: p, Offset: req.Offset}, st.Send)
}

func (c *copyServer) Pull(req *pb.CopyPullRequest, st pb.Copy_PullServer) error {
	ctx := st.Context()
	id := IdentityFrom(ctx)
	owner := id.Owner()
	fields := map[string]any{"transfer": req.GetGrant().GetTransferId(), "from": req.From, "path": req.Path}
	fail := func(err error) error {
		fields["error"] = errString(err)
		c.s.Audit.Log(id, pb.Copy_Pull_FullMethodName, fields)
		return err
	}
	switch {
	case !transferIDRE.MatchString(req.GetGrant().GetTransferId()) || len(req.GetGrant().GetToken()) < 16:
		return fail(status.Error(codes.InvalidArgument, "bad transfer"))
	case req.From == "" || req.From == c.s.Name || len(req.FromEndpoints) == 0:
		return fail(status.Error(codes.InvalidArgument, "bad source worker"))
	case req.Path == "":
		return fail(status.Error(codes.InvalidArgument, "no path"))
	}
	if why := c.s.copyDeny(owner); why != "" {
		return fail(status.Error(codes.PermissionDenied, why))
	}
	if c.s.PeerDial == nil {
		return fail(status.Error(codes.FailedPrecondition, "this worker does not dial other workers"))
	}
	conn, closeConn, err := c.s.PeerDial(req.From, req.FromEndpoints)
	if err != nil {
		return fail(status.Errorf(codes.Unavailable, "connect to %s: %v", req.From, err))
	}
	defer closeConn()
	src := pb.NewCopyClient(conn)
	// the source, as this connection verified it, before a byte is taken:
	// endorsed by the owner (end-to-end trust), the node the client named
	var p peer.Peer
	root, err := src.Stat(ctx, &pb.CopyStatRequest{Grant: req.Grant}, grpc.Peer(&p))
	if err != nil {
		return fail(err)
	}
	if why := LinkPeerTrustDeny(c.s.Trust.Load(), clientPeerCert(&p), owner); why != "" {
		return fail(status.Error(codes.PermissionDenied, why))
	}
	if req.FromId != "" {
		if sid, err := pki.IdentityFromCert(clientPeerCert(&p)); err != nil || !strings.EqualFold(sid.ID, req.FromId) {
			return fail(status.Errorf(codes.PermissionDenied, "%s is not the node the copy was offered by", req.From))
		}
	}
	dst, err := c.s.filesAs(ctx, owner)
	if err != nil {
		return fail(err)
	}
	defer dst.Close()
	target, err := dst.Stat(ctx, &pb.FileStatRequest{Path: req.Path, NoFollow: true})
	if err != nil {
		return fail(err)
	}
	cp := &copier{ctx: ctx, src: src, dst: dst, grant: req.Grant, send: st.Send}
	if err := cp.run(root, target.AbsPath); err != nil {
		fields["files"], fields["sent"] = cp.p.Files, cp.p.Sent
		return fail(err)
	}
	fields["files"], fields["sent"], fields["resumed"], fields["up_to_date"] = cp.p.Files, cp.p.Sent, cp.p.Resumed, cp.p.UpToDate
	c.s.Audit.Log(id, pb.Copy_Pull_FullMethodName, fields)
	cp.p.Done, cp.p.AbsPath, cp.p.File = true, target.AbsPath, ""
	return st.Send(&cp.p)
}

// copier copies an offered file or tree (Pull).
type copier struct {
	ctx   context.Context
	src   pb.CopyClient
	dst   userFiles
	grant *pb.CopyGrant
	send  func(*pb.CopyProgress) error
	p     pb.CopyProgress
	last  time.Time
}

// progress sends the counters, at most every 500 ms unless now.
func (c *copier) progress(now bool) error {
	if !now && time.Since(c.last) < 500*time.Millisecond {
		return nil
	}
	c.last = time.Now()
	return c.send(&c.p)
}

func (c *copier) run(root *pb.FileInfo, target string) error {
	if !root.IsDir {
		c.p.FilesTotal, c.p.BytesTotal = 1, root.Size
		return c.file("", target, root)
	}
	list, err := c.src.List(c.ctx, &pb.CopyListRequest{Grant: c.grant})
	if err != nil {
		return err
	}
	for _, e := range list.Entries {
		if !e.IsDir && !e.IsSymlink {
			c.p.FilesTotal++
			c.p.BytesTotal += e.Size
		}
	}
	if _, err := c.dst.Mkdir(c.ctx, &pb.MkdirRequest{Path: target, Mode: root.Mode}); err != nil {
		return err
	}
	if err := c.progress(true); err != nil {
		return err
	}
	for _, e := range list.Entries { // directories before their contents
		if !validCopyRel(e.Path) {
			return status.Errorf(codes.InvalidArgument, "the source listed %q: not a path this worker writes", e.Path)
		}
		p := filepath.Join(target, filepath.FromSlash(e.Path))
		switch {
		case e.IsSymlink:
			c.p.Links++ // not followed, not copied: it may point anywhere
		case e.IsDir:
			if _, err := c.dst.Mkdir(c.ctx, &pb.MkdirRequest{Path: p, Mode: e.Mode}); err != nil {
				return err
			}
		default:
			if err := c.file(e.Path, p, e); err != nil {
				return err
			}
		}
	}
	return nil
}

// file copies rel to target, again after a broken read or write: the next
// attempt resumes from the partial file the last one left.
func (c *copier) file(rel, target string, info *pb.FileInfo) error {
	c.p.File = rel
	for attempt := 0; ; attempt++ {
		err := c.fileOnce(rel, target, info)
		if err == nil {
			c.p.Files++
			return c.progress(false)
		}
		switch status.Code(err) {
		case codes.Unavailable, codes.Aborted, codes.DataLoss, codes.DeadlineExceeded:
		default:
			return fileError(rel, target, err)
		}
		if attempt >= copyRetries || c.ctx.Err() != nil {
			return fileError(rel, target, err)
		}
		select {
		case <-c.ctx.Done():
			return c.ctx.Err()
		case <-time.After(copyBackoff << attempt):
		}
	}
}

func fileError(rel, target string, err error) error {
	name := rel
	if name == "" {
		name = target
	}
	return status.Errorf(status.Code(err), "%s: %s", name, status.Convert(err).Message())
}

func (c *copier) fileOnce(rel, target string, info *pb.FileInfo) error {
	ctx := c.ctx
	whole, err := c.src.Stat(ctx, &pb.CopyStatRequest{Grant: c.grant, Path: rel, Hash: true})
	switch {
	case err != nil:
		return err
	case !whole.Exists || whole.IsDir || whole.IsSymlink:
		return status.Error(codes.FailedPrecondition, "no longer a regular file on the source")
	}
	size := whole.Size
	// the same file there already: done
	if cur, err := c.dst.Stat(ctx, &pb.FileStatRequest{Path: target, NoFollow: true}); err != nil {
		return err
	} else if cur.Exists && !cur.IsDir && !cur.IsSymlink && cur.Size == size {
		h, err := c.dst.Stat(ctx, &pb.FileStatRequest{Path: target, Hash: true})
		if err != nil {
			return err
		}
		if bytes.Equal(h.Sha256, whole.Sha256) {
			c.p.UpToDate++
			c.p.Bytes += size
			return nil
		}
	}
	// a partial file of an earlier attempt: resume when it is a prefix of
	// the source
	var off int64
	part, err := c.dst.Stat(ctx, &pb.FileStatRequest{Path: target, Partial: true, Hash: true})
	if err != nil {
		return err
	}
	if part.Exists && !part.IsDir && part.Size > 0 && part.Size <= size {
		ph, err := c.src.Stat(ctx, &pb.CopyStatRequest{Grant: c.grant, Path: rel, Hash: true, HashLimit: part.Size})
		if err != nil {
			return err
		}
		if bytes.Equal(ph.Sha256, part.Sha256) {
			off = part.Size
		}
	}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	rs, err := c.src.Read(rctx, &pb.CopyReadRequest{Grant: c.grant, Path: rel, Offset: off})
	if err != nil {
		return err
	}
	resumed, sent := off, int64(0)
	c.p.Resumed += resumed
	c.p.Bytes += resumed
	_, err = c.dst.Write(ctx, &pb.FileWriteHeader{Path: target, Offset: off, Size: size, Sha256: whole.Sha256, Mode: info.Mode, Mtime: info.Mtime},
		func() ([]byte, error) {
			ch, err := rs.Recv()
			if err != nil {
				return nil, err
			}
			if ch.Offset != off+sent {
				return nil, status.Errorf(codes.DataLoss, "the source sent offset %d, want %d", ch.Offset, off+sent)
			}
			sent += int64(len(ch.Data))
			c.p.Sent += int64(len(ch.Data))
			c.p.Bytes += int64(len(ch.Data))
			if err := c.progress(false); err != nil {
				return nil, err
			}
			return ch.Data, nil
		})
	if err != nil {
		// what this attempt counted is counted again by the next one
		c.p.Bytes -= resumed + sent
		c.p.Resumed -= resumed
		return err
	}
	return nil
}

// userFiles are the Files operations as a user: through the user's helper
// with per-user isolation, else longarmd's own (it runs as that account).
type userFiles interface {
	Stat(context.Context, *pb.FileStatRequest) (*pb.FileInfo, error)
	List(context.Context, *pb.FileListRequest) (*pb.FileListResponse, error)
	Mkdir(context.Context, *pb.MkdirRequest) (*pb.FileInfo, error)
	Read(ctx context.Context, req *pb.FileReadRequest, send func(*pb.FileChunk) error) error
	Write(ctx context.Context, h *pb.FileWriteHeader, recv func() ([]byte, error)) (*pb.FileWriteResult, error)
	Close()
}

func (s *Server) filesAs(ctx context.Context, user string) (userFiles, error) {
	if !s.perUser() {
		return localFiles{&filesServer{s: s}}, nil
	}
	sp, err := s.spaceFor(user)
	if err != nil {
		return nil, err
	}
	h, err := sp.StartHelper(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return helperFiles{close: func() { h.Close() }, c: pb.NewFilesClient(h.Conn)}, nil
}

type localFiles struct{ f *filesServer }

func (l localFiles) Stat(ctx context.Context, r *pb.FileStatRequest) (*pb.FileInfo, error) {
	return l.f.Stat(ctx, r)
}

func (l localFiles) List(ctx context.Context, r *pb.FileListRequest) (*pb.FileListResponse, error) {
	return l.f.List(ctx, r)
}

func (l localFiles) Mkdir(ctx context.Context, r *pb.MkdirRequest) (*pb.FileInfo, error) {
	return l.f.Mkdir(ctx, r)
}

func (l localFiles) Read(_ context.Context, r *pb.FileReadRequest, send func(*pb.FileChunk) error) error {
	abs, err := resolvePath(r.Path)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	fh, err := platform.OpenShared(abs)
	if err != nil {
		return fileErr(err)
	}
	defer fh.Close()
	if fi, err := fh.Stat(); err == nil && fi.IsDir() {
		return status.Errorf(codes.InvalidArgument, "%s is a directory", abs)
	}
	return readFile(fh, r.Offset, send)
}

func (l localFiles) Write(_ context.Context, h *pb.FileWriteHeader, recv func() ([]byte, error)) (*pb.FileWriteResult, error) {
	abs, err := resolvePath(h.Path)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return l.f.write(recv, h, abs)
}

func (localFiles) Close() {}

type helperFiles struct {
	close func()
	c     pb.FilesClient
}

func (h helperFiles) Stat(ctx context.Context, r *pb.FileStatRequest) (*pb.FileInfo, error) {
	return h.c.Stat(ctx, r)
}

func (h helperFiles) List(ctx context.Context, r *pb.FileListRequest) (*pb.FileListResponse, error) {
	return h.c.List(ctx, r)
}

func (h helperFiles) Mkdir(ctx context.Context, r *pb.MkdirRequest) (*pb.FileInfo, error) {
	return h.c.Mkdir(ctx, r)
}

func (h helperFiles) Read(ctx context.Context, r *pb.FileReadRequest, send func(*pb.FileChunk) error) error {
	st, err := h.c.Read(ctx, r)
	if err != nil {
		return err
	}
	for {
		ch, err := st.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := send(ch); err != nil {
			return err
		}
	}
}

func (h helperFiles) Write(ctx context.Context, hdr *pb.FileWriteHeader, recv func() ([]byte, error)) (*pb.FileWriteResult, error) {
	// a failed read cancels the stream: the helper keeps the partial file
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ws, err := h.c.Write(wctx)
	if err != nil {
		return nil, err
	}
	if err := ws.Send(&pb.FileWriteRequest{Msg: &pb.FileWriteRequest_Header{Header: hdr}}); err != nil {
		_, err = ws.CloseAndRecv()
		return nil, err
	}
	for {
		data, err := recv()
		if errors.Is(err, io.EOF) {
			return ws.CloseAndRecv()
		}
		if err != nil {
			return nil, err
		}
		if err := ws.Send(&pb.FileWriteRequest{Msg: &pb.FileWriteRequest_Data{Data: data}}); err != nil {
			_, err = ws.CloseAndRecv() // the helper's reason
			return nil, err
		}
	}
}

func (h helperFiles) Close() { h.close() }
