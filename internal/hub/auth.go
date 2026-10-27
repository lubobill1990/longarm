package hub

import (
	"context"
	"crypto/x509"
	"slices"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

var (
	users     = []pki.Role{pki.RoleClient, pki.RoleAdmin}
	adminOnly = []pki.Role{pki.RoleAdmin}
	anyNode   = []pki.Role{pki.RoleWorker, pki.RoleClient, pki.RoleAdmin}
	// releaser: the CI credential (role release) publishes releases and
	// starts, watches and aborts rollouts - and can call nothing else.
	releaser = []pki.Role{pki.RoleAdmin, pki.RoleRelease}
)

// openMethods need no client certificate: the join code / poll token (or
// for EntraJoin the Microsoft access token, for pairings the approval of
// a signed-in user) is the credential.
var openMethods = map[string]bool{
	pb.Enroll_Join_FullMethodName:       true,
	pb.Enroll_JoinStatus_FullMethodName: true,
	pb.Enroll_AuthInfo_FullMethodName:   true,
	pb.Enroll_EntraJoin_FullMethodName:  true,
	pb.Enroll_StartLink_FullMethodName:  true,
	pb.Enroll_PollLink_FullMethodName:   true,
}

// hubMethodRoles is the hub's authorization table (default deny).
var hubMethodRoles = map[string][]pki.Role{
	pb.HubNode_Connect_FullMethodName:         {pki.RoleWorker},
	pb.HubNode_Renew_FullMethodName:           anyNode,
	pb.HubNode_Leave_FullMethodName:           anyNode,
	pb.Fleet_ReportLinks_FullMethodName:       users,
	pb.Fleet_Links_FullMethodName:             adminOnly, // other clients' paths
	pb.Fleet_History_FullMethodName:           users,
	pb.Fleet_CreatePortLink_FullMethodName:    users,
	pb.Fleet_ListPortLinks_FullMethodName:     users,
	pb.Fleet_RemovePortLink_FullMethodName:    users,
	pb.HubAdmin_KillJob_FullMethodName:        adminOnly,
	pb.HubAdmin_JobLogs_FullMethodName:        adminOnly,
	pb.HubAdmin_SubmitJob_FullMethodName:      adminOnly,
	pb.HubAdmin_ToolchainApply_FullMethodName: adminOnly,
	pb.Fleet_ListNodes_FullMethodName:         anyNode,
	pb.Fleet_Whoami_FullMethodName:            anyNode,
	pb.Fleet_ListJobs_FullMethodName:          users,
	pb.Fleet_GetJob_FullMethodName:            users,
	pb.Fleet_WaitJob_FullMethodName:           users,
	pb.Fleet_Events_FullMethodName:            users,
	pb.Fleet_Place_FullMethodName:             users,
	pb.Fleet_Audit_FullMethodName:             adminOnly,
	// own workers: the handlers check owner / can_share / admin
	pb.Fleet_CreateWorkerCode_FullMethodName:  users,
	pb.Fleet_ShareWorker_FullMethodName:       users,
	pb.Fleet_RemoveWorker_FullMethodName:      users,
	pb.HubAdmin_CreateJoinCode_FullMethodName: adminOnly,
	pb.HubAdmin_ListJoinCodes_FullMethodName:  adminOnly,
	pb.HubAdmin_Approve_FullMethodName:        adminOnly,
	pb.HubAdmin_Revoke_FullMethodName:         adminOnly,
	pb.HubAdmin_SetDrain_FullMethodName:       adminOnly,
	pb.HubAdmin_SetLabels_FullMethodName:      adminOnly,
	pb.HubAdmin_PutProfile_FullMethodName:     adminOnly,
	pb.HubAdmin_ListProfiles_FullMethodName:   adminOnly,
	pb.HubAdmin_AssignProfile_FullMethodName:  adminOnly,
	pb.Fleet_TailnetKey_FullMethodName:        anyNode,
	pb.HubAdmin_TailnetStatus_FullMethodName:  adminOnly,
	pb.HubAdmin_TailnetRekey_FullMethodName:   adminOnly,
	pb.Fleet_CheckUpdate_FullMethodName:       {pki.RoleWorker},
	pb.Fleet_DownloadRelease_FullMethodName:   {pki.RoleWorker, pki.RoleAdmin},
	pb.HubAdmin_ListReleases_FullMethodName:   releaser,
	pb.HubAdmin_PushRelease_FullMethodName:    releaser,
	pb.HubAdmin_PublishRelease_FullMethodName: releaser,
	pb.HubAdmin_GetRollout_FullMethodName:     releaser,
	pb.HubAdmin_AbortRollout_FullMethodName:   releaser,
	pb.HubAdmin_PutUser_FullMethodName:        adminOnly,
	pb.HubAdmin_ListUsers_FullMethodName:      adminOnly,
	pb.HubAdmin_RemoveUser_FullMethodName:     adminOnly,
	pb.HubAdmin_SetTeam_FullMethodName:        adminOnly,
	pb.HubAdmin_ListTeams_FullMethodName:      adminOnly,
	pb.Telemetry_Push_FullMethodName:          anyNode,
}

type callerKey struct{}

// caller is the authenticated identity of a gRPC call.
type caller struct {
	id   pki.Identity
	cert *x509.Certificate
	addr string
}

func callerFrom(ctx context.Context) caller {
	c, _ := ctx.Value(callerKey{}).(caller)
	return c
}

func (h *Hub) authorize(ctx context.Context, method string) (context.Context, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "no peer")
	}
	c := caller{addr: p.Addr.String()}
	if openMethods[method] {
		return context.WithValue(ctx, callerKey{}, c), nil
	}
	ti, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(ti.State.PeerCertificates) == 0 {
		return nil, status.Error(codes.Unauthenticated, "client certificate required")
	}
	cert := ti.State.PeerCertificates[0]
	id, err := pki.IdentityFromCert(cert)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, err.Error())
	}
	if h.revoked.Revoked(cert) {
		return nil, status.Errorf(codes.Unauthenticated, "certificate %s is revoked", pki.Serial(cert))
	}
	if !slices.Contains(hubMethodRoles[method], id.Role) {
		return nil, status.Errorf(codes.PermissionDenied, "%s may not call %s", id, method)
	}
	if err := h.checkNode(id, cert); err != nil {
		return nil, err
	}
	if id.Role == pki.RoleClient || id.Role == pki.RoleAdmin {
		h.mu.Lock()
		disabled := h.userDisabledLocked(id.Owner())
		h.mu.Unlock()
		if disabled {
			return nil, status.Errorf(codes.PermissionDenied, "user %s is disabled", id.Owner())
		}
	}
	c.id, c.cert = id, cert
	return context.WithValue(ctx, callerKey{}, c), nil
}

// checkNode refuses revoked nodes and identity/role mismatches. A valid
// certificate of the hub's CA that the hub has not seen before (issued by
// hand with `longarm admin issue` before the hub existed) registers its node as
// approved; its serial is recorded so a later revocation covers it.
func (h *Hub) checkNode(id pki.Identity, cert *x509.Certificate) error {
	serial := pki.Serial(cert)
	h.mu.Lock()
	defer h.mu.Unlock()
	n := h.nodes[id.Name]
	switch {
	case n == nil:
		rec := &store.Node{Name: id.Name, Role: string(id.Role), State: stateApproved, JoinedAt: time.Now().UTC(),
			ApprovedAt: time.Now().UTC(), CertSerial: serial, CertNotAfter: cert.NotAfter, User: id.User}
		if err := h.store.PutNode(rec); err != nil {
			return status.Error(codes.Internal, err.Error())
		}
		n = &node{rec: rec}
		h.nodes[id.Name] = n
		h.log.Info("node registered by certificate", "node", id.String(), "serial", serial)
		h.publishNodeLocked(n, "registered by certificate")
	case n.rec.State == stateRevoked:
		return status.Errorf(codes.Unauthenticated, "node %s is revoked", id.Name)
	case n.rec.Role != string(id.Role):
		return status.Errorf(codes.PermissionDenied, "the name %s belongs to a %s, not a %s", id.Name, n.rec.Role, id.Role)
	case n.rec.State == statePending:
		// A pending node has no certificate of the hub yet; this one is from
		// an older enrollment or issued by hand: let it through as approved.
	}
	if !h.knownSerials[serial] {
		if err := h.store.AddCert(store.Cert{Serial: serial, Node: id.Name, NotAfter: cert.NotAfter, IssuedAt: cert.NotBefore}); err != nil {
			h.log.Warn("record certificate", "err", err) // retried on the next call
		} else {
			h.knownSerials[serial] = true
		}
	}
	return nil
}

func (h *Hub) unaryAuth(ctx context.Context, req any, info *grpc.UnaryServerInfo, hd grpc.UnaryHandler) (any, error) {
	ctx, err := h.authorize(ctx, info.FullMethod)
	if err != nil {
		return nil, err
	}
	return hd(ctx, req)
}

type authStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (a *authStream) Context() context.Context { return a.ctx }

func (h *Hub) streamAuth(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, hd grpc.StreamHandler) error {
	ctx, err := h.authorize(ss.Context(), info.FullMethod)
	if err != nil {
		return err
	}
	return hd(srv, &authStream{ServerStream: ss, ctx: ctx})
}

// Node states.
const (
	statePending  = "pending"
	stateApproved = "approved"
	stateRevoked  = "revoked"
)
