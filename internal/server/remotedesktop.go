package server

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"longarm/internal/jobs"
	"longarm/internal/rdesk"
	"longarm/internal/screen"
	pb "longarm/proto/longarm/v1"
)

// FeatureDesktopNative: the RemoteDesktop service can show this worker's
// desktop (Node.Info lists it only when it can).
const FeatureDesktopNative = jobs.FeatureDesktopNative

// DesktopAgent reaches the Longarm Desktop agent (SYSTEM in the console
// session: the UAC prompt and lock screen too). nil, or ok false: capture
// in longarmd itself.
type DesktopAgent func(ctx context.Context) (c pb.RemoteDesktopClient, ok bool)

type remoteDesktopServer struct {
	pb.UnimplementedRemoteDesktopServer
	s *Server

	mu   sync.Mutex
	stop context.CancelFunc // the session being watched (a new viewer takes over)
	done chan struct{}      // closed when it has ended
}

// desktopReason says why the desktop cannot be shown ("" = it can).
func (s *Server) desktopReason(ctx context.Context) string {
	if s.DesktopAgent != nil {
		if _, ok := s.DesktopAgent(ctx); ok {
			return ""
		}
	}
	return screen.Available()
}

// desktopOwner refuses everybody but the worker's owner: an admin
// certificate of somebody else does not see the owner's screen.
func (r *remoteDesktopServer) desktopOwner(ctx context.Context, method string, fields map[string]any) error {
	id := IdentityFrom(ctx)
	if !r.s.machineOwner(id.Owner()) {
		f := map[string]any{"denied": "not the owner"}
		for k, v := range fields {
			f[k] = v
		}
		r.s.Audit.Log(id, method, f)
		return status.Errorf(codes.PermissionDenied, "only the owner of %s may see its desktop", r.s.Name)
	}
	return nil
}

func (r *remoteDesktopServer) Displays(ctx context.Context, _ *pb.DesktopDisplaysRequest) (*pb.DesktopDisplaysResponse, error) {
	if err := r.desktopOwner(ctx, pb.RemoteDesktop_Displays_FullMethodName, nil); err != nil {
		return nil, err
	}
	if r.s.DesktopAgent != nil {
		if c, ok := r.s.DesktopAgent(ctx); ok {
			return c.Displays(ctx, &pb.DesktopDisplaysRequest{})
		}
	}
	out := &pb.DesktopDisplaysResponse{NativeReason: screen.Available(), Codecs: rdesk.Codecs()}
	if out.NativeReason == "" {
		ds, err := screen.Displays()
		if err != nil {
			out.NativeReason = err.Error()
		}
		for _, d := range ds {
			out.Displays = append(out.Displays, &pb.DesktopDisplay{Index: int32(d.Index), Name: d.Name,
				X: int32(d.X), Y: int32(d.Y), Width: int32(d.W), Height: int32(d.H)})
		}
	}
	return out, nil
}

func (r *remoteDesktopServer) Stream(st pb.RemoteDesktop_StreamServer) (err error) {
	method := pb.RemoteDesktop_Stream_FullMethodName
	if err := r.desktopOwner(st.Context(), method, nil); err != nil {
		return err
	}
	first, err := st.Recv()
	if err != nil {
		return err
	}
	start := first.GetStart()
	if start == nil {
		return status.Error(codes.InvalidArgument, "the first message must be start")
	}
	id := IdentityFrom(st.Context())
	fields := map[string]any{"display": start.Display, "view_only": start.ViewOnly}
	if reason := r.s.desktopReason(st.Context()); reason != "" {
		fields["denied"] = reason
		r.s.Audit.Log(id, method, fields)
		return status.Error(codes.FailedPrecondition, reason)
	}
	r.s.Audit.Log(id, method, fields)
	began := time.Now()

	// One viewer at a time: a new one takes the desktop over, once the
	// previous one has let go of the capture.
	ctx, cancel := context.WithCancel(st.Context())
	done := make(chan struct{})
	r.mu.Lock()
	prevStop, prevDone := r.stop, r.done
	r.stop, r.done = cancel, done
	r.mu.Unlock()
	if prevStop != nil {
		prevStop()
		select {
		case <-prevDone:
		case <-time.After(5 * time.Second):
		}
	}
	defer func() {
		cancel()
		close(done)
		f := map[string]any{"ended": time.Since(began).Round(time.Second).String()}
		if err != nil {
			f["error"] = errString(err)
		}
		r.s.Audit.Log(id, method, f)
	}()

	if r.s.DesktopAgent != nil {
		if c, ok := r.s.DesktopAgent(ctx); ok {
			return proxyDesktop(ctx, c, first, st)
		}
	}
	err = rdesk.Serve(ctxStream{st, ctx}, start, r.s.Log)
	if errors.Is(err, screen.ErrUnsupported) {
		return status.Error(codes.Unimplemented, err.Error())
	}
	if err != nil && ctx.Err() == nil {
		return status.Error(codes.Unavailable, err.Error())
	}
	return nil
}

// ctxStream is a stream with its own (cancelable) context.
type ctxStream struct {
	pb.RemoteDesktop_StreamServer
	ctx context.Context
}

func (c ctxStream) Context() context.Context { return c.ctx }

// proxyDesktop relays a viewer's stream to the desktop agent.
func proxyDesktop(ctx context.Context, c pb.RemoteDesktopClient, first *pb.DesktopClientMessage, st pb.RemoteDesktop_StreamServer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	up, err := c.Stream(ctx)
	if err != nil {
		return status.Errorf(codes.Unavailable, "the Longarm Desktop agent: %v", err)
	}
	if err := up.Send(first); err != nil {
		return status.Errorf(codes.Unavailable, "the Longarm Desktop agent: %v", err)
	}
	go func() {
		defer cancel()
		for {
			m, err := st.Recv()
			if err != nil {
				up.CloseSend()
				return
			}
			if err := up.Send(m); err != nil {
				return
			}
		}
	}()
	for {
		m, err := up.Recv()
		if errors.Is(err, io.EOF) || ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}
		if err := st.Send(m); err != nil {
			return err
		}
	}
}
