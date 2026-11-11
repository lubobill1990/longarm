package server

// Speed tests (longarm speedtest, worker feature "speedtest"): how fast bytes
// move between the client and this worker (Source, Sink), and between this
// worker and another (Speed, on the target: it dials the other as Pull does
// and runs Source and Sink there with a speed offer's grant). The bytes are
// filler: nothing is read or written on disk.
//
// Trust as for copies (copy.go): the owner's client calls directly; a peer
// worker only with the token of a speed offer (Offer speed = true) the
// owner gave for exactly that worker, which serves no file.

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/transport"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Limits of speed tests (variables for tests).
var (
	speedDefault  = 3 * time.Second
	speedMax      = 10 * time.Second
	speedMaxBytes = int64(1 << 30)
	// speedSlots: tests at a time on this worker (each one fills a link)
	speedSlots = make(chan struct{}, 2)
)

const speedChunk = 256 << 10

// speedFiller is what Source and Speed send (random: nothing on the way
// compresses it).
var speedFiller = sync.OnceValue(func() []byte {
	b := make([]byte, speedChunk)
	rand.Read(b)
	return b
})

// PathConn is a peer connection that knows its transport path (PeerDial's,
// longarmd: client.Conn.Path).
type PathConn interface {
	grpc.ClientConnInterface
	Path() transport.PathInfo
}

func speedLimits(d *durationpb.Duration, maxBytes int64) (time.Duration, int64) {
	t := d.AsDuration()
	if d == nil || t <= 0 {
		t = speedDefault
	}
	if t > speedMax {
		t = speedMax
	}
	if maxBytes <= 0 || maxBytes > speedMaxBytes {
		maxBytes = speedMaxBytes
	}
	return t, maxBytes
}

// speedCaller checks a Source or Sink caller: the owner's client, or the
// peer worker a speed offer names (with its token).
func (c *copyServer) speedCaller(ctx context.Context, g *pb.CopyGrant, method string) error {
	id := IdentityFrom(ctx)
	fields := map[string]any{}
	if id.Role == pki.RoleWorker {
		if _, err := c.grant(ctx, g, method, fields, true); err != nil {
			return err
		}
		c.s.Audit.Log(id, method, fields)
		return nil
	}
	if why := c.s.copyDeny(id.Owner()); why != "" {
		fields["denied"] = why
		c.s.Audit.Log(id, method, fields)
		return status.Error(codes.PermissionDenied, why)
	}
	return nil
}

func speedSlot() (func(), error) {
	select {
	case speedSlots <- struct{}{}:
		return func() { <-speedSlots }, nil
	default:
		return nil, status.Errorf(codes.ResourceExhausted, "%d speed tests run on this worker already: try again shortly", cap(speedSlots))
	}
}

// Source sends filler until the duration or max_bytes is up.
func (c *copyServer) Source(req *pb.SpeedSourceRequest, st pb.Copy_SourceServer) error {
	ctx := st.Context()
	if err := c.speedCaller(ctx, req.Grant, pb.Copy_Source_FullMethodName); err != nil {
		return err
	}
	done, err := speedSlot()
	if err != nil {
		return err
	}
	defer done()
	d, maxBytes := speedLimits(req.Duration, req.MaxBytes)
	return sendFiller(ctx, d, maxBytes, func(b []byte) error { return st.Send(&pb.FileChunk{Data: b}) })
}

func sendFiller(ctx context.Context, d time.Duration, maxBytes int64, send func([]byte) error) error {
	buf := speedFiller()
	end := time.Now().Add(d)
	for sent := int64(0); sent < maxBytes && time.Now().Before(end); {
		if err := ctx.Err(); err != nil {
			return err
		}
		b := buf
		if left := maxBytes - sent; left < int64(len(b)) {
			b = b[:left]
		}
		if err := send(b); err != nil {
			return err
		}
		sent += int64(len(b))
	}
	return nil
}

// Sink takes what comes and says how much, and in how long from its first
// byte.
func (c *copyServer) Sink(st pb.Copy_SinkServer) error {
	ctx := st.Context()
	first, err := st.Recv()
	if errors.Is(err, io.EOF) {
		return st.SendAndClose(&pb.SpeedSinkResponse{})
	}
	if err != nil {
		return err
	}
	if err := c.speedCaller(ctx, first.Grant, pb.Copy_Sink_FullMethodName); err != nil {
		return err
	}
	done, err := speedSlot()
	if err != nil {
		return err
	}
	defer done()
	start := time.Now()
	n := int64(len(first.Data))
	deadline := time.NewTimer(speedMax + 5*time.Second) // a sender that never stops
	defer deadline.Stop()
	for {
		select {
		case <-deadline.C:
			return status.Error(codes.DeadlineExceeded, "a speed test runs at most "+speedMax.String())
		default:
		}
		m, err := st.Recv()
		if errors.Is(err, io.EOF) {
			return st.SendAndClose(&pb.SpeedSinkResponse{Bytes: n, Took: durationpb.New(time.Since(start))})
		}
		if err != nil {
			return err
		}
		n += int64(len(m.Data))
	}
}

// Speed (the owner's client, on the target): measure to worker from, both
// ways, over the connection a Pull would use.
func (c *copyServer) Speed(ctx context.Context, req *pb.CopySpeedRequest) (*pb.SpeedResult, error) {
	id := IdentityFrom(ctx)
	owner := id.Owner()
	fields := map[string]any{"transfer": req.GetGrant().GetTransferId(), "from": req.From}
	fail := func(err error) (*pb.SpeedResult, error) {
		fields["error"] = errString(err)
		c.s.Audit.Log(id, pb.Copy_Speed_FullMethodName, fields)
		return nil, err
	}
	switch {
	case !transferIDRE.MatchString(req.GetGrant().GetTransferId()) || len(req.GetGrant().GetToken()) < 16:
		return fail(status.Error(codes.InvalidArgument, "bad transfer"))
	case req.From == "" || req.From == c.s.Name || len(req.FromEndpoints) == 0:
		return fail(status.Error(codes.InvalidArgument, "bad worker to measure to"))
	}
	if why := c.s.copyDeny(owner); why != "" {
		return fail(status.Error(codes.PermissionDenied, why))
	}
	if c.s.PeerDial == nil {
		return fail(status.Error(codes.FailedPrecondition, "this worker does not dial other workers"))
	}
	d, maxBytes := speedLimits(req.Duration, req.MaxBytes)
	conn, closeConn, err := c.s.PeerDial(req.From, req.FromEndpoints)
	if err != nil {
		return fail(status.Errorf(codes.Unavailable, "connect to %s: %v", req.From, err))
	}
	defer closeConn()
	src := pb.NewCopyClient(conn)
	res := &pb.SpeedResult{}
	// down: from the other worker to this one; its identity checked as
	// Pull checks the source's, before the result counts
	var p peer.Peer
	rd, err := src.Source(ctx, &pb.SpeedSourceRequest{Grant: req.Grant, Duration: durationpb.New(d), MaxBytes: maxBytes}, grpc.Peer(&p))
	if err != nil {
		return fail(err)
	}
	var start time.Time
	for {
		m, err := rd.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fail(err)
		}
		if start.IsZero() {
			start = time.Now()
		}
		res.DownBytes += int64(len(m.Data))
	}
	if why := LinkPeerTrustDeny(c.s.Trust.Load(), clientPeerCert(&p), owner); why != "" {
		return fail(status.Error(codes.PermissionDenied, why))
	}
	if req.FromId != "" {
		if sid, err := pki.IdentityFromCert(clientPeerCert(&p)); err != nil || !strings.EqualFold(sid.ID, req.FromId) {
			return fail(status.Errorf(codes.PermissionDenied, "%s is not the node the test was offered by", req.From))
		}
	}
	if !start.IsZero() {
		res.DownMbps = mbps(res.DownBytes, time.Since(start))
	}
	if pc, ok := conn.(PathConn); ok { // connected now (the dial is lazy)
		p := pc.Path()
		res.Transport, res.Path, res.Rtt, res.Detail = p.Transport, string(p.Kind), durationpb.New(p.RTT), p.Detail
	}
	// up: from this worker to the other, timed by the other's sink
	wr, err := src.Sink(ctx)
	if err != nil {
		return fail(err)
	}
	first := true
	err = sendFiller(ctx, d, maxBytes, func(b []byte) error {
		m := &pb.SpeedSinkChunk{Data: b}
		if first {
			m.Grant, first = req.Grant, false
		}
		return wr.Send(m)
	})
	if err != nil && !errors.Is(err, io.EOF) {
		return fail(err)
	}
	sum, err := wr.CloseAndRecv()
	if err != nil {
		return fail(err)
	}
	res.UpBytes, res.UpMbps = sum.Bytes, mbps(sum.Bytes, sum.Took.AsDuration())
	fields["up_mbps"], fields["down_mbps"], fields["path"] = res.UpMbps, res.DownMbps, res.Path
	c.s.Audit.Log(id, pb.Copy_Speed_FullMethodName, fields)
	return res, nil
}

func mbps(n int64, took time.Duration) float64 {
	if took <= 0 {
		return 0
	}
	return float64(n) * 8 / took.Seconds() / 1e6
}
