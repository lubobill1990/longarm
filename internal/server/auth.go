package server

import (
	"context"
	"slices"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

var (
	anyUser   = []pki.Role{pki.RoleClient, pki.RoleAdmin}
	adminOnly = []pki.Role{pki.RoleAdmin}
)

// MethodRoles is the authorization table: which certificate roles may call
// each RPC. Methods missing from the table are denied (default deny).
// Worker certificates are for serving; they get no client rights - only
// LinkTunnel.Dial, which needs a grant from the hub for a port link.
var MethodRoles = map[string][]pki.Role{
	pb.Node_Info_FullMethodName:             anyUser,
	pb.Exec_Run_FullMethodName:              anyUser,
	pb.Jobs_Submit_FullMethodName:           anyUser,
	pb.Jobs_Get_FullMethodName:              anyUser,
	pb.Jobs_List_FullMethodName:             anyUser,
	pb.Jobs_Logs_FullMethodName:             anyUser,
	pb.Jobs_Wait_FullMethodName:             anyUser,
	pb.Jobs_Kill_FullMethodName:             anyUser,
	pb.Metrics_Snapshot_FullMethodName:      anyUser,
	pb.Metrics_Stream_FullMethodName:        anyUser,
	pb.Admin_Shutdown_FullMethodName:        adminOnly,
	pb.Admin_Gc_FullMethodName:              adminOnly,
	pb.Sessions_Attach_FullMethodName:       anyUser,
	pb.Sessions_List_FullMethodName:         anyUser,
	pb.Sessions_Kill_FullMethodName:         anyUser,
	pb.Sessions_Logs_FullMethodName:         anyUser,
	pb.Sessions_Start_FullMethodName:        anyUser,
	pb.Sessions_Get_FullMethodName:          anyUser,
	pb.Sessions_Send_FullMethodName:         anyUser,
	pb.Sessions_Stop_FullMethodName:         anyUser,
	pb.Forward_Open_FullMethodName:          anyUser,
	pb.Forward_Ssh_FullMethodName:           anyUser,
	pb.Files_Stat_FullMethodName:            anyUser,
	pb.Files_List_FullMethodName:            anyUser,
	pb.Files_Read_FullMethodName:            anyUser,
	pb.Files_Write_FullMethodName:           anyUser,
	pb.Files_Mkdir_FullMethodName:           anyUser,
	pb.Files_Tail_FullMethodName:            anyUser,
	pb.Workspace_Refs_FullMethodName:        anyUser,
	pb.Workspace_Sync_FullMethodName:        anyUser,
	pb.Workspace_Fetch_FullMethodName:       anyUser,
	pb.Workspace_PushHandoff_FullMethodName: anyUser,
	// owners only (machineupdate.go checks): a client or admin certificate
	// of the worker's owner, end to end - the hub has no command for it
	pb.MachineUpdate_Start_FullMethodName:   anyUser,
	pb.MachineUpdate_Status_FullMethodName:  anyUser,
	pb.MachineUpdate_SetAuto_FullMethodName: anyUser,
	// The one RPC of worker certificates: port link data, with a hub-signed
	// grant naming the caller (linktunnel.go).
	pb.LinkTunnel_Dial_FullMethodName: {pki.RoleWorker},
	// Direct handoff transfers: the owner offers and pulls; the peer worker
	// fetches with the one-time token of the offer (handoff.go).
	pb.Handoff_Offer_FullMethodName: anyUser,
	pb.Handoff_Pull_FullMethodName:  anyUser,
	pb.Handoff_Fetch_FullMethodName: {pki.RoleWorker},
}

type idKey struct{}

// IdentityFrom returns the authenticated caller.
func IdentityFrom(ctx context.Context) pki.Identity {
	id, _ := ctx.Value(idKey{}).(pki.Identity)
	return id
}

func (s *Server) authorize(ctx context.Context, method string) (context.Context, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "no peer")
	}
	ti, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(ti.State.PeerCertificates) == 0 {
		return nil, status.Error(codes.Unauthenticated, "client certificate required")
	}
	id, err := pki.IdentityFromCert(ti.State.PeerCertificates[0])
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, err.Error())
	}
	if s.Revoked.Revoked(ti.State.PeerCertificates[0]) {
		s.Audit.Log(id, method, map[string]any{"denied": "revoked", "peer": p.Addr.String()})
		return nil, status.Errorf(codes.Unauthenticated, "certificate of %s is revoked", id)
	}
	if !slices.Contains(MethodRoles[method], id.Role) {
		s.Audit.Log(id, method, map[string]any{"denied": true, "peer": p.Addr.String()})
		return nil, status.Errorf(codes.PermissionDenied, "%s may not call %s", id, method)
	}
	if s.Policy != nil && s.Policy.Disabled(id.Owner()) {
		s.Audit.Log(id, method, map[string]any{"denied": "user disabled", "peer": p.Addr.String()})
		return nil, status.Errorf(codes.PermissionDenied, "user %s is disabled", id.Owner())
	}
	if id.Role == pki.RoleWorker {
		// Worker certificates reach LinkTunnel.Dial only (the table above).
		// The caller is a machine, not a user: [access], the owner/shares
		// rule and end-to-end trust are applied there to the link's owner
		// and to the calling worker's endorsement (linktunnel.go).
		return context.WithValue(ctx, idKey{}, id), nil
	}
	if why := s.Access.Deny(id, method); why != "" {
		s.Audit.Log(id, method, map[string]any{"denied": why, "peer": p.Addr.String()})
		return nil, status.Errorf(codes.PermissionDenied, "%s may not call %s on this worker (%s)", id, method, why)
	}
	// Last: the hub's owner/shares rule can only take away what passed the
	// checks above (and the TLS layer's end-to-end trust), never grant.
	if why := s.ownerDeny(id.Owner()); why != "" && method != pb.Node_Info_FullMethodName {
		s.Audit.Log(id, method, map[string]any{"denied": why, "peer": p.Addr.String()})
		return nil, status.Errorf(codes.PermissionDenied, "%s may not call %s on this worker (%s)", id, method, why)
	}
	return context.WithValue(ctx, idKey{}, id), nil
}

// ownerDeny says why user may not use this user-owned worker ("" = may):
// not its owner, not shared with the user, one of its teams or everyone.
// Admin certificates are no exception - owner-only means owner-only.
func (s *Server) ownerDeny(user string) string {
	if s.WorkerAccess.Owner() == "" {
		return ""
	}
	var teams []string
	if s.Policy != nil {
		teams = s.Policy.Teams(user)
	}
	if s.WorkerAccess.Allows(user, teams) {
		return ""
	}
	return "worker owner/shares"
}

func (s *Server) unaryAuth(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
	ctx, err := s.authorize(ctx, info.FullMethod)
	if err != nil {
		return nil, err
	}
	return h(ctx, req)
}

type authStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (a *authStream) Context() context.Context { return a.ctx }

func (s *Server) streamAuth(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, h grpc.StreamHandler) error {
	ctx, err := s.authorize(ss.Context(), info.FullMethod)
	if err != nil {
		return err
	}
	return h(srv, &authStream{ServerStream: ss, ctx: ctx})
}
