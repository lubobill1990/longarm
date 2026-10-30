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
	"github.com/lubobill1990/longarm/internal/nodeid"
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
	pb.HubNode_Renew_FullMethodName:           {pki.RoleWorker, pki.RoleClient, pki.RoleAdmin, pki.RoleRelay},
	pb.HubNode_RelaySync_FullMethodName:       {pki.RoleRelay},
	pb.HubAdmin_Relays_FullMethodName:         adminOnly,
	pb.HubNode_Leave_FullMethodName:           anyNode,
	pb.Fleet_ReportLinks_FullMethodName:       users,
	pb.Fleet_Links_FullMethodName:             adminOnly, // other clients' paths
	pb.Fleet_History_FullMethodName:           users,
	pb.Fleet_CreatePortLink_FullMethodName:    users,
	pb.Fleet_ListPortLinks_FullMethodName:     users,
	pb.Fleet_RemovePortLink_FullMethodName:    users,
	pb.Fleet_HandoffPair_FullMethodName:       users, // the caller's own workers only (handoffpairs.go)
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
	pb.Fleet_SetWorkerLabels_FullMethodName:   users,
	pb.Fleet_RenameWorker_FullMethodName:      users,
	pb.HubAdmin_RenameNode_FullMethodName:     adminOnly,
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
	pb.HubAdmin_FleetSummary_FullMethodName:   adminOnly,
	pb.Telemetry_Push_FullMethodName:          anyNode,
}

type callerKey struct{}

// caller is the authenticated identity of a gRPC call.
type caller struct {
	id     pki.Identity
	cert   *x509.Certificate
	addr   string
	nodeID string // the caller's node (also for certificates without an ID)
	// user is the Longarm user the caller acts as (userOfLocked): empty
	// for workers, relays and the release credential, which never act as
	// a user whatever their name.
	user string
}

// userOfLocked is the Longarm user a certificate of node nodeID acts as on
// the hub. Only clients and admins have one: the certificate's user, else
// the user the hub recorded for that device (pinned at enrollment, by the
// node ID migration and before a rename: a rename never changes whom a
// device acts as), else (an old single-user certificate) its name - a
// client/admin name the hub never hands to another user's device
// (nodeNameReservedLocked). A worker's name is never a user: a worker
// named like a user sees nothing of that user's.
func (h *Hub) userOfLocked(id pki.Identity, nodeID string) string {
	if id.Role != pki.RoleClient && id.Role != pki.RoleAdmin {
		return ""
	}
	if id.User != "" {
		return id.User
	}
	if n := h.nodes[nodeID]; n != nil && n.rec.Role == string(id.Role) && n.rec.User != "" {
		return n.rec.User
	}
	return id.Name
}

// nodeNameReservedLocked reports whether a node of role for user may not
// take name (a new node's, or a rename's): a Longarm user's name (live or
// purged) belongs to that user - only devices bound to that user may carry
// it: never a worker, another user's device or a device bound to no user
// (whose name would be its user, userOfLocked).
func (h *Hub) nodeNameReservedLocked(name, role, user string) bool {
	if h.tenants[name] == nil && !slices.Contains(h.purged, name) {
		return false
	}
	if role != string(pki.RoleClient) && role != string(pki.RoleAdmin) {
		return true
	}
	return user != name
}

// errNameReserved is the refusal of a node name that is a user's.
func errNameReserved(name string) error {
	return status.Errorf(codes.AlreadyExists, "node name %q is a user's name (pick another with --name)", name)
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
	nodeID, err := h.checkNode(id, cert)
	if err != nil {
		return nil, err
	}
	h.supersedeIfDue(nodeID, cert)
	h.mu.Lock()
	c.user = h.userOfLocked(id, nodeID)
	disabled := c.user != "" && h.userDisabledLocked(c.user)
	h.mu.Unlock()
	if disabled {
		return nil, status.Errorf(codes.PermissionDenied, "user %s is disabled", c.user)
	}
	c.id, c.cert, c.nodeID = id, cert, nodeID
	return context.WithValue(ctx, callerKey{}, c), nil
}

// checkNode finds the caller's node (nodeForCertLocked) and refuses
// revoked nodes and role mismatches; it returns the node's ID. A valid
// certificate of the hub's CA that the hub has not seen before (issued by
// hand with `longarm admin issue`, e.g. a release certificate) registers
// its node as approved - with the ID the certificate carries, else a new
// one - but only for a name without history: no node ever had it (as its
// name or its certificates'), no certificate was ever recorded for it. A
// certificate of a name with history that the hub can't map by ID or
// recorded serial is refused, never mapped by name (it could be a copy
// of a revoked node's, or a renamed node's old one): re-issue it with the
// node's ID (`longarm admin issue --id`) or enroll again.
func (h *Hub) checkNode(id pki.Identity, cert *x509.Certificate) (string, error) {
	serial := pki.Serial(cert)
	h.mu.Lock()
	defer h.mu.Unlock()
	n := h.nodeForCertLocked(id, cert)
	switch {
	case n == nil:
		if o := h.nameHistoryLocked(id.Name); o != "" {
			h.log.Warn("unknown certificate of a name with history refused", "identity", id.String(), "id", id.ID, "serial", serial, "history", o)
			return "", status.Errorf(codes.PermissionDenied,
				"certificate %s of %s is not known to the hub, and the name %s has history (%s): it is never mapped by name - re-issue it for the node (longarm admin issue --id <node id>) or enroll again",
				serial, id, id.Name, o)
		}
		rec := &store.Node{ID: id.ID, Name: id.Name, Role: string(id.Role), State: stateApproved, JoinedAt: time.Now().UTC(),
			ApprovedAt: time.Now().UTC(), CertSerial: serial, CertNotAfter: cert.NotAfter, User: id.User, CertHasID: id.ID != ""}
		if rec.Role != string(pki.RoleWorker) {
			rec.User = id.Owner() // pinned: a later name must not change whom it acts as
		}
		if rec.ID == "" {
			rec.ID = nodeid.New()
		}
		if err := h.store.PutNode(rec); err != nil {
			return "", status.Error(codes.Internal, err.Error())
		}
		n = &node{rec: rec}
		h.nodes[rec.ID] = n
		h.log.Info("node registered by certificate", "node", id.String(), "id", rec.ID, "serial", serial)
		h.publishNodeLocked(n, "registered by certificate")
	case n.rec.State == stateRevoked:
		return "", status.Errorf(codes.Unauthenticated, "node %s (%s) is revoked", n.rec.Name, nodeid.Short(n.rec.ID))
	case n.rec.Role != string(id.Role):
		return "", status.Errorf(codes.PermissionDenied, "node %s is a %s, not a %s", n.rec.Name, n.rec.Role, id.Role)
	case n.rec.State == statePending:
		// A pending node has no certificate of the hub yet; this one is from
		// an older enrollment or issued by hand: let it through as approved.
	}
	if _, ok := h.knownSerials[serial]; !ok {
		if err := h.store.AddCert(store.Cert{Serial: serial, Node: id.Name, NodeID: n.rec.ID, NotAfter: cert.NotAfter, IssuedAt: cert.NotBefore,
			HasID: id.ID != ""}); err != nil {
			h.log.Warn("record certificate", "err", err) // retried on the next call
		} else {
			h.knownSerials[serial] = n.rec.ID
			if id.ID == "" {
				h.reloadLegacyLocked()
			}
		}
	}
	return n.rec.ID, nil
}

// callerNodeLocked is the caller's node (nil: unknown).
func (h *Hub) callerNodeLocked(c caller) *node {
	if n := h.nodes[c.nodeID]; n != nil {
		return n
	}
	if c.cert != nil {
		return h.nodeForCertLocked(c.id, c.cert)
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
