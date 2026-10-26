package server

import (
	"context"
	"crypto/x509"
	"net"
	"strconv"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/portlink"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// LinkRegistry is what the server needs of longarmd's port links
// (internal/longarmd: the hub-pushed list, the listeners for links from
// this worker).
type LinkRegistry interface {
	// Roots verify the hub's grant signatures (the fleet CA).
	Roots() *x509.CertPool
	// Incoming returns the active link id whose target is this worker, and
	// a context that ends when the link is removed or expires.
	Incoming(id string) (*pb.PortLink, context.Context, bool)
	// Env is the environment of a job of owner on this worker: the owner's
	// links from here (portlink.Env); nil = none.
	Env(owner string) map[string]string
}

type linkTunnelServer struct {
	pb.UnimplementedLinkTunnelServer
	s *Server
}

// Dial is the target side of a port link: the caller is the link's "from"
// worker (its worker certificate); see the proto.
func (t *linkTunnelServer) Dial(st pb.LinkTunnel_DialServer) error {
	s := t.s
	id := IdentityFrom(st.Context())
	first, err := st.Recv()
	if err != nil {
		return err
	}
	deny := func(code codes.Code, fields map[string]any, format string, a ...any) error {
		err := status.Errorf(code, format, a...)
		fields["denied"] = status.Convert(err).Message()
		s.Audit.Log(id, pb.LinkTunnel_Dial_FullMethodName, fields)
		return err
	}
	if s.Links == nil {
		return deny(codes.FailedPrecondition, map[string]any{}, "this worker has no port links")
	}
	g, err := portlink.Verify(s.Links.Roots(), first.Grant, time.Now())
	if err != nil {
		return deny(codes.PermissionDenied, map[string]any{}, "%v", err)
	}
	fields := map[string]any{"link": g.Id, "owner": g.Owner, "from": g.From, "port": g.Port}
	switch {
	case g.From != id.Name:
		return deny(codes.PermissionDenied, fields, "link %s is not for %s", g.Id, id.Name)
	case g.To != s.Name:
		return deny(codes.PermissionDenied, fields, "link %s targets %s, not this worker", g.Id, g.To)
	}
	l, linkCtx, ok := s.Links.Incoming(g.Id)
	if !ok {
		// Not (yet / any more) in the hub's list for this worker: removed,
		// expired, or the hub's push has not arrived yet (the caller retries).
		return deny(codes.FailedPrecondition, fields, "link %s is not active on %s", g.Id, s.Name)
	}
	if l.Owner != g.Owner || l.From != g.From || l.Port != g.Port {
		return deny(codes.PermissionDenied, fields, "link %s: grant and the hub's list disagree", g.Id)
	}
	if s.Policy != nil && s.Policy.Disabled(g.Owner) {
		return deny(codes.PermissionDenied, fields, "user %s is disabled", g.Owner)
	}
	// The same checks as Forward.Open for the link's owner.
	host := "127.0.0.1"
	target := net.JoinHostPort(host, strconv.Itoa(int(g.Port)))
	fields["target"] = target
	if err := s.targetAllowed(host); err != nil {
		return deny(codes.PermissionDenied, fields, "%s", status.Convert(err).Message())
	}
	if s.perUser() {
		if err := s.portOwnedBy(g.Owner, int(g.Port)); err != nil {
			return deny(status.Code(err), fields, "link %s: %s (owner %s)", g.Id, status.Convert(err).Message(), g.Owner)
		}
	}
	s.Audit.Log(id, pb.LinkTunnel_Dial_FullMethodName, fields)
	c, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		return status.Errorf(codes.Unavailable, "connect %s: %v", target, err)
	}
	defer c.Close()
	// Accepted: the caller waits for this (empty) frame before it sends data.
	if err := st.Send(&pb.ForwardFrame{}); err != nil {
		return err
	}
	stop := context.AfterFunc(linkCtx, func() { c.Close() }) // link removed: cut
	defer stop()
	if len(first.Data) > 0 {
		if _, err := c.Write(first.Data); err != nil {
			return status.Errorf(codes.Unavailable, "write %s: %v", target, err)
		}
	}
	pipeStream(st, c, first.Eof, linkCtx.Done())
	if linkCtx.Err() != nil {
		return status.Errorf(codes.Aborted, "link %s was removed", g.Id)
	}
	return nil
}

// linkEnv adds the owner's link variables to env (variables the caller set
// win); env itself is not modified.
func (s *Server) linkEnv(owner string, env map[string]string) map[string]string {
	if s.Links == nil {
		return env
	}
	add := s.Links.Env(owner)
	if len(add) == 0 {
		return env
	}
	out := make(map[string]string, len(env)+len(add))
	for k, v := range add {
		out[k] = v
	}
	for k, v := range env {
		out[k] = v
	}
	return out
}

// retryLinkEnv gives a retried submit the link variables of the first one
// (links may have changed since; the specs must stay equal).
func retryLinkEnv(env, first map[string]string) map[string]string {
	var out map[string]string
	for k, v := range first {
		if _, set := env[k]; set || !portlink.IsEnv(k) {
			continue
		}
		if out == nil {
			out = make(map[string]string, len(env)+len(first))
			for k2, v2 := range env {
				out[k2] = v2
			}
		}
		out[k] = v
	}
	if out == nil {
		return env
	}
	return out
}
