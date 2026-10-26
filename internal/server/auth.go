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
// Worker certificates are for serving; they get no client rights in P0a.
var MethodRoles = map[string][]pki.Role{
	pb.Node_Info_FullMethodName:        anyUser,
	pb.Exec_Run_FullMethodName:         anyUser,
	pb.Jobs_Submit_FullMethodName:      anyUser,
	pb.Jobs_Get_FullMethodName:         anyUser,
	pb.Jobs_List_FullMethodName:        anyUser,
	pb.Jobs_Logs_FullMethodName:        anyUser,
	pb.Jobs_Wait_FullMethodName:        anyUser,
	pb.Jobs_Kill_FullMethodName:        anyUser,
	pb.Metrics_Snapshot_FullMethodName: anyUser,
	pb.Metrics_Stream_FullMethodName:   anyUser,
	pb.Admin_Shutdown_FullMethodName:   adminOnly,
	pb.Admin_Gc_FullMethodName:         adminOnly,
	pb.Sessions_Attach_FullMethodName:  anyUser,
	pb.Sessions_List_FullMethodName:    anyUser,
	pb.Sessions_Kill_FullMethodName:    anyUser,
	pb.Sessions_Logs_FullMethodName:    anyUser,
	pb.Forward_Open_FullMethodName:     anyUser,
	pb.Forward_Ssh_FullMethodName:      anyUser,
	pb.Files_Stat_FullMethodName:       anyUser,
	pb.Files_List_FullMethodName:       anyUser,
	pb.Files_Read_FullMethodName:       anyUser,
	pb.Files_Write_FullMethodName:      anyUser,
	pb.Files_Mkdir_FullMethodName:      anyUser,
	pb.Workspace_Refs_FullMethodName:   anyUser,
	pb.Workspace_Sync_FullMethodName:   anyUser,
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
	if why := s.Access.Deny(id, method); why != "" {
		s.Audit.Log(id, method, map[string]any{"denied": why, "peer": p.Addr.String()})
		return nil, status.Errorf(codes.PermissionDenied, "%s may not call %s on this worker (%s)", id, method, why)
	}
	return context.WithValue(ctx, idKey{}, id), nil
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
