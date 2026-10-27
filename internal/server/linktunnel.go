package server

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/portlink"
	"github.com/lubobill1990/longarm/internal/telemetry"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// LinkRegistry is what the server needs of longarmd's port links
// (internal/longarmd: the hub-pushed list, the listeners for links from
// this worker).
type LinkRegistry interface {
	// Roots verify the hub's grant signatures (the fleet CA).
	Roots() *x509.CertPool
	// Incoming returns the active link id whose target is this worker and
	// takes one of its connection slots (release frees it), with a context
	// that ends when the link is removed or expires or its connections are
	// cut (owner disabled). Errors: ErrLinkInactive, or the link has too
	// many open connections.
	Incoming(id string) (l *pb.PortLink, ctx context.Context, release func(), err error)
	// Env is the environment of a job of owner on this worker: the owner's
	// links from here (portlink.Env); nil = none.
	Env(owner string) map[string]string
}

// ErrLinkInactive: the link is not (yet / any more) in this worker's list.
var ErrLinkInactive = errors.New("link not active")

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
		// telemetry: the outcome only (no link, owner, worker or port)
		telemetry.Count("link.dial", map[string]string{"outcome": "denied", "grpc_code": code.String()}, 1)
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
	l, linkCtx, release, err := s.Links.Incoming(g.Id)
	if errors.Is(err, ErrLinkInactive) {
		// Not (yet / any more) in the hub's list for this worker: removed,
		// expired, or the hub's push has not arrived yet (the caller retries).
		return deny(codes.FailedPrecondition, fields, "link %s is not active on %s", g.Id, s.Name)
	}
	if err != nil {
		return deny(codes.ResourceExhausted, fields, "%v", err)
	}
	defer release()
	if l.Owner != g.Owner || l.From != g.From || l.Port != g.Port {
		return deny(codes.PermissionDenied, fields, "link %s: grant and the hub's list disagree", g.Id)
	}
	// This worker's own policy decides, not the hub's grant: the owner must
	// be a user it serves ([access], owner/shares, trust), and with
	// end-to-end trust the calling worker must be endorsed by the owner's
	// pinned key - the hub can't pair a user's port with a machine of its
	// choice.
	if why := s.LinkOwnerDeny(g.Owner); why != "" {
		return deny(codes.PermissionDenied, fields, "link %s: %s", g.Id, why)
	}
	if why := LinkPeerTrustDeny(s.Trust.Load(), peerCert(st.Context()), g.Owner); why != "" {
		return deny(codes.PermissionDenied, fields, "link %s: %s", g.Id, why)
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
		telemetry.Count("link.dial", map[string]string{"outcome": "unreachable"}, 1)
		return status.Errorf(codes.Unavailable, "connect %s: %v", target, err)
	}
	defer c.Close()
	telemetry.Count("link.dial", map[string]string{"outcome": "accepted"}, 1)
	// Accepted: the caller waits for this (empty) frame before it sends data.
	if err := st.Send(&pb.ForwardFrame{}); err != nil {
		return err
	}
	stop := context.AfterFunc(linkCtx, func() { c.Close() }) // link removed, owner disabled: cut
	defer stop()
	if len(first.Data) > 0 {
		if _, err := c.Write(first.Data); err != nil {
			return status.Errorf(codes.Unavailable, "write %s: %v", target, err)
		}
	}
	pipeStream(st, c, first.Eof, linkCtx.Done())
	if linkCtx.Err() != nil {
		return status.Errorf(codes.Aborted, "link %s was removed (or its owner disabled)", g.Id)
	}
	return nil
}

// LinkOwnerDeny says why this worker's own policy refuses port links owned
// by owner ("" = it doesn't). It applies on both ends of a link - the
// target (LinkTunnel.Dial) and the source (listener, local connections) -
// and the hub can only narrow it: the owner must not be disabled, must be a
// user of this worker ([access] users / owners), allowed by the owner and
// shares of a user-owned worker, and with end-to-end trust have a pinned
// user key here.
func (s *Server) LinkOwnerDeny(owner string) string {
	if s.Policy != nil && s.Policy.Disabled(owner) {
		return "user " + owner + " is disabled"
	}
	if !s.Access.AllowsUser(owner) {
		return "the link's owner is not a user of this worker ([access])"
	}
	if why := s.ownerDeny(owner); why != "" {
		return "the link's owner may not use this worker (" + why + ")"
	}
	if p := s.Trust.Load(); p.Active() {
		if _, ok := p.Users[owner]; !ok {
			return "end-to-end trust: no key of the link's owner is trusted on this worker"
		}
	}
	return ""
}

// LinkPeerTrustDeny says why the worker at the other end of a link (its
// certificate peer) is refused under the trust policy p ("" = accepted, or
// no end-to-end trust). The TLS layer already demands an endorsement by
// some trusted user; a link also demands that it is the link owner's: the
// owner endorsed both of the link's machines, so a hub can't route the
// owner's port to (or the owner's traffic from) a worker somebody else
// controls.
func LinkPeerTrustDeny(p *pki.TrustPolicy, peer *x509.Certificate, owner string) string {
	if !p.Active() {
		return ""
	}
	if peer == nil {
		return "end-to-end trust: no peer certificate"
	}
	u, err := p.Check(peer)
	if err != nil {
		return "end-to-end trust: " + err.Error()
	}
	if u != owner {
		id, _ := pki.IdentityFromCert(peer)
		return fmt.Sprintf("end-to-end trust: %s is endorsed by %s, not by the link's owner", id, u)
	}
	return ""
}

// peerCert is the TLS peer's leaf certificate of a stream (nil: none).
func peerCert(ctx context.Context) *x509.Certificate {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil
	}
	ti, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(ti.State.PeerCertificates) == 0 {
		return nil
	}
	return ti.State.PeerCertificates[0]
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
