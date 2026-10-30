package server

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
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

	mu      sync.Mutex
	pending map[string]int // unverified streams per peer
}

// Limits on streams whose grant is not verified yet (a worker certificate
// gets this far without a grant): how long the first frame may take, and
// how many such streams one peer may have open (variables for tests).
var (
	linkFirstFrameTimeout = 10 * time.Second
	linkMaxPendingPerPeer = 16
)

// pendingSlot counts one more unverified stream of peer (free it with the
// returned func); false when the peer already has linkMaxPendingPerPeer.
func (t *linkTunnelServer) pendingSlot(peer string) (func(), bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pending == nil {
		t.pending = map[string]int{}
	}
	if t.pending[peer] >= linkMaxPendingPerPeer {
		return nil, false
	}
	t.pending[peer]++
	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			if t.pending[peer]--; t.pending[peer] <= 0 {
				delete(t.pending, peer)
			}
		})
	}, true
}

// firstFrame receives the stream's first frame within linkFirstFrameTimeout.
// On timeout the handler returns, which ends the stream and the Recv.
func firstFrame(st pb.LinkTunnel_DialServer) (*pb.ForwardFrame, error) {
	type res struct {
		f   *pb.ForwardFrame
		err error
	}
	ch := make(chan res, 1)
	go func() {
		f, err := st.Recv()
		ch <- res{f, err}
	}()
	t := time.NewTimer(linkFirstFrameTimeout)
	defer t.Stop()
	select {
	case r := <-ch:
		return r.f, r.err
	case <-t.C:
		return nil, status.Errorf(codes.DeadlineExceeded, "no link grant within %s", linkFirstFrameTimeout)
	}
}

// Dial is the target side of a port link: the caller is the link's "from"
// worker (its worker certificate); see the proto.
func (t *linkTunnelServer) Dial(st pb.LinkTunnel_DialServer) error {
	s := t.s
	id := IdentityFrom(st.Context())
	deny := func(code codes.Code, fields map[string]any, format string, a ...any) error {
		err := status.Errorf(code, format, a...)
		fields["denied"] = status.Convert(err).Message()
		s.Audit.Log(id, pb.LinkTunnel_Dial_FullMethodName, fields)
		// telemetry: the outcome only (no link, owner, worker or port)
		telemetry.Count("link.dial", map[string]string{"outcome": "denied", "grpc_code": code.String()}, 1)
		return err
	}
	// Before anything is verified: a bounded number of streams per peer,
	// each with a deadline for its grant.
	done, ok := t.pendingSlot(id.String())
	if !ok {
		return deny(codes.ResourceExhausted, map[string]any{}, "too many link streams of %s waiting for their grant (max %d)", id, linkMaxPendingPerPeer)
	}
	defer done()
	first, err := firstFrame(st)
	if err != nil {
		if status.Code(err) == codes.DeadlineExceeded {
			return deny(codes.DeadlineExceeded, map[string]any{}, "%s", status.Convert(err).Message())
		}
		return err
	}
	if s.Links == nil {
		return deny(codes.FailedPrecondition, map[string]any{}, "this worker has no port links")
	}
	var revoked func(*x509.Certificate) bool
	if s.Revoked != nil {
		revoked = s.Revoked.Revoked
	}
	g, err := portlink.VerifyRevocable(s.Links.Roots(), first.Grant, time.Now(), revoked)
	if err != nil {
		return deny(codes.PermissionDenied, map[string]any{}, "%v", err)
	}
	done() // verified: the link's own connection cap applies from here
	fields := map[string]any{"link": g.Id, "owner": g.Owner, "from": g.From, "port": g.Port}
	self := s.selfIdentity()
	switch {
	case g.From != id.Name:
		return deny(codes.PermissionDenied, fields, "link %s is not for %s", g.Id, id.Name)
	case g.To != s.Name:
		return deny(codes.PermissionDenied, fields, "link %s targets %s, not this worker", g.Id, g.To)
	// node IDs: the grant binds the ends' immutable IDs (a certificate
	// without one - issued before IDs - is held to the names above)
	case g.FromId != "" && id.ID != "" && !strings.EqualFold(g.FromId, id.ID):
		return deny(codes.PermissionDenied, fields, "link %s is for node %s, not %s", g.Id, g.FromId, id.ID)
	case g.ToId != "" && self.ID != "" && !strings.EqualFold(g.ToId, self.ID):
		return deny(codes.PermissionDenied, fields, "link %s targets node %s, not this worker (%s)", g.Id, g.ToId, self.ID)
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
	if l.Owner != g.Owner || l.From != g.From || l.Port != g.Port || !strings.EqualFold(l.FromId, g.FromId) || !strings.EqualFold(l.ToId, g.ToId) {
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
	peerE, why := LinkPeerTrust(s.Trust.Load(), peerCert(st.Context()), g.Owner)
	if why != "" {
		return deny(codes.PermissionDenied, fields, "link %s: %s", g.Id, why)
	}
	// With end-to-end trust the hub's grant is not enough: the owner must
	// have signed this very link (a device endorsed by the pinned key), and
	// both ends must be the workers the owner signed for - by node ID, or
	// (a consent from before IDs) by their endorsed names, never by the
	// hub's labels.
	if p := s.Trust.Load(); p.Active() {
		if err := portlink.VerifyConsent(p, l); err != nil {
			return deny(codes.PermissionDenied, fields, "end-to-end trust: %v", err)
		}
		spec, _, _ := portlink.ParseConsent(l.Consent)
		if err := portlink.CheckEnd(spec, true, id, peerE); err != nil {
			return deny(codes.PermissionDenied, fields, "end-to-end trust: %v", err)
		}
		if err := portlink.CheckEnd(spec, false, self, s.selfEndorsement()); err != nil {
			return deny(codes.PermissionDenied, fields, "end-to-end trust: %v", err)
		}
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
	_, why := LinkPeerTrust(p, peer, owner)
	return why
}

// LinkPeerTrust is LinkPeerTrustDeny returning the peer's verified
// endorsement (nil without end-to-end trust), for portlink.CheckEnd.
func LinkPeerTrust(p *pki.TrustPolicy, peer *x509.Certificate, owner string) (*pki.Endorsement, string) {
	if !p.Active() {
		return nil, ""
	}
	if peer == nil {
		return nil, "end-to-end trust: no peer certificate"
	}
	e, err := p.CheckEndorsement(peer)
	if err != nil {
		return nil, "end-to-end trust: " + err.Error()
	}
	if e.User != owner {
		id, _ := pki.IdentityFromCert(peer)
		return nil, fmt.Sprintf("end-to-end trust: %s is endorsed by %s, not by the link's owner", id, e.User)
	}
	return e, ""
}

// selfIdentity is this worker's identity as its current certificate says
// (Server.SelfCert; else the name it started with).
func (s *Server) selfIdentity() pki.Identity {
	if s.SelfCert != nil {
		if c := s.SelfCert(); c != nil {
			if id, err := pki.IdentityFromCert(c); err == nil {
				return id
			}
		}
	}
	return pki.Identity{Role: pki.RoleWorker, Name: s.Name}
}

// selfEndorsement is the endorsement this worker's current certificate
// carries (nil: none).
func (s *Server) selfEndorsement() *pki.Endorsement {
	if s.SelfCert == nil {
		return nil
	}
	c := s.SelfCert()
	if c == nil {
		return nil
	}
	if t, err := pki.TrustOf(c); err == nil && t != nil {
		return t.Endorsement
	}
	return nil
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

// Variables every session gets (jobs.FeatureSessionEnv): where it runs,
// for a longarm used inside it (handoff's default --from).
const (
	EnvSessionWorker = "LONGARM_WORKER"
	EnvSessionName   = "LONGARM_SESSION"
)

func isSessionEnv(k string) bool { return k == EnvSessionWorker || k == EnvSessionName }

// sessionEnv adds LONGARM_WORKER and LONGARM_SESSION to a new session's
// env (what the caller set wins).
func (s *Server) sessionEnv(name string, env map[string]string) map[string]string {
	out := make(map[string]string, len(env)+2)
	for k, v := range env {
		out[k] = v
	}
	for k, v := range map[string]string{EnvSessionWorker: s.Name, EnvSessionName: name} {
		if _, set := out[k]; !set && v != "" {
			out[k] = v
		}
	}
	return out
}

// retryLinkEnv gives a retried submit the link variables of the first one
// (links may have changed since; the specs must stay equal).
func retryLinkEnv(env, first map[string]string) map[string]string {
	var out map[string]string
	for k, v := range first {
		if _, set := env[k]; set || !(portlink.IsEnv(k) || isSessionEnv(k)) {
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
