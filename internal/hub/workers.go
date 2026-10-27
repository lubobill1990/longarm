package hub

import (
	"context"
	"io"
	"net/http"
	"slices"
	"sort"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/tenancy"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// User-owned workers (docs/usage.md "Your own workers"): a user registers a
// machine with a worker code of its own (longarm worker add); the worker
// joins approved and is usable by that user only - hidden from everybody
// else, never placed for them - until shared. Sharing needs an admin's
// permission (TenantUser.can_share); admins share and unshare any worker.
// Workers without owner (registered by admins) are the shared fleet, as
// before. The hub pushes owner and shares to the worker
// (HubCommand.worker_access), which enforces them on every call.

// maxWorkerCodes caps a user's active (unused, unexpired) worker codes.
const maxWorkerCodes = 10

func sharesPB(s *store.WorkerShares) *pb.WorkerShares {
	if s == nil {
		return nil
	}
	return &pb.WorkerShares{Users: slices.Clone(s.Users), Teams: slices.Clone(s.Teams), Everyone: s.Everyone}
}

// teamsLocked are user's teams.
func (h *Hub) teamsLocked(user string) []string {
	if t := h.tenants[user]; t != nil {
		return t.u.Teams
	}
	return nil
}

// mayUseLocked reports whether user may use worker n (run, exec, place
// jobs on it). Admins are no exception: an owned worker is its owner's.
func (h *Hub) mayUseLocked(user string, n *node) bool {
	return tenancy.MayUseWorker(user, h.teamsLocked(user), n.rec.Owner, sharesPB(n.rec.Shares))
}

// canSeeNode reports whether v may see the node at all: admins everything;
// users their own devices, fleet workers and the workers they may use.
func canSeeNode(v tenancy.Caller, n *pb.NodeInfo) bool {
	if v.Admin {
		return true
	}
	if n.Role != string(pki.RoleWorker) {
		return nodeUserOf(n) == v.User
	}
	return tenancy.MayUseWorker(v.User, v.Teams, n.Owner, n.Shares)
}

func workerAccessLocked(n *node) *pb.WorkerAccess {
	return &pb.WorkerAccess{Version: n.rec.AccessVersion, Owner: n.rec.Owner, Shares: sharesPB(n.rec.Shares)}
}

func workerAccessCmd(a *pb.WorkerAccess) *pb.HubCommand {
	return &pb.HubCommand{Msg: &pb.HubCommand_WorkerAccess{WorkerAccess: a}}
}

// CreateWorkerCode makes a worker join code owned by user (Fleet.CreateWorkerCode).
func (h *Hub) CreateWorkerCode(by, user string, req *pb.CreateWorkerCodeRequest) (*pb.JoinCode, error) {
	if user == "" {
		return nil, status.Error(codes.PermissionDenied, "no user")
	}
	h.mu.Lock()
	disabled := h.userDisabledLocked(user)
	h.mu.Unlock()
	if disabled {
		return nil, status.Errorf(codes.PermissionDenied, "user %s is disabled", user)
	}
	ttl := req.Ttl
	if ttl == nil {
		ttl = durationpb.New(24 * time.Hour)
	}
	return h.CreateJoinCode(by, &pb.CreateJoinCodeRequest{Role: string(pki.RoleWorker), Ttl: ttl, Labels: req.Labels,
		Name: req.Name, Owner: user})
}

// activeWorkerCodes counts owner's unused, unexpired worker codes.
func (h *Hub) activeWorkerCodes(owner string) (int, error) {
	now := time.Now()
	all, err := h.store.JoinCodes(now)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, c := range all {
		if c.Owner == owner && c.UsedAt.IsZero() && c.ExpiresAt.After(now) {
			n++
		}
	}
	return n, nil
}

// ShareWorker changes who may use a worker: an admin (admin = the caller
// has an admin certificate) for any owned worker, its owner if an admin
// granted the owner can_share.
func (h *Hub) ShareWorker(by, user string, admin bool, req *pb.ShareWorkerRequest) (*pb.NodeInfo, error) {
	for _, u := range append(slices.Clone(req.AddUsers), req.RemoveUsers...) {
		if !pki.ValidUser(u) {
			return nil, status.Errorf(codes.InvalidArgument, "invalid user name %q", u)
		}
	}
	for _, t := range append(slices.Clone(req.AddTeams), req.RemoveTeams...) {
		if !tenancy.ValidTeam(t) {
			return nil, status.Errorf(codes.InvalidArgument, "invalid team name %q", t)
		}
	}
	h.mu.Lock()
	n, err := h.ownedWorkerLocked(req.Worker, user, admin)
	if err != nil {
		h.mu.Unlock()
		return nil, err
	}
	if n.rec.Owner == "" {
		h.mu.Unlock()
		return nil, status.Errorf(codes.FailedPrecondition, "worker %s has no owner: every user may use it", req.Worker)
	}
	if !admin {
		if t := h.tenants[user]; t == nil || !t.u.CanShare {
			h.mu.Unlock()
			return nil, status.Errorf(codes.PermissionDenied, "sharing workers needs an admin's permission (longarm admin user set %s --can-share)", user)
		}
	}
	for _, u := range req.AddUsers {
		if h.tenants[u] == nil {
			h.mu.Unlock()
			return nil, status.Errorf(codes.NotFound, "no user %q", u)
		}
	}
	s := store.WorkerShares{}
	if n.rec.Shares != nil {
		s = *n.rec.Shares
	}
	s.Users = editList(s.Users, req.AddUsers, req.RemoveUsers)
	s.Teams = editList(s.Teams, req.AddTeams, req.RemoveTeams)
	if req.SetEveryone {
		s.Everyone = req.Everyone
	}
	rec := *n.rec
	rec.Shares = &s
	if len(s.Users) == 0 && len(s.Teams) == 0 && !s.Everyone {
		rec.Shares = nil
	}
	rec.AccessVersion++
	if err := h.store.PutNode(&rec); err != nil {
		h.mu.Unlock()
		return nil, status.Error(codes.Internal, err.Error())
	}
	n.rec = &rec
	h.sendLocked(n, workerAccessCmd(workerAccessLocked(n)))
	h.publishNodeLocked(n, "shares changed by "+by)
	h.recheckPortLinksLocked() // links of users it is no longer shared with end
	info := h.nodeInfoLocked(n)
	h.mu.Unlock()
	h.auditHub(by, "workers/share", map[string]any{"worker": req.Worker, "owner": rec.Owner,
		"add_users": req.AddUsers, "remove_users": req.RemoveUsers, "add_teams": req.AddTeams, "remove_teams": req.RemoveTeams,
		"set_everyone": req.SetEveryone, "everyone": req.Everyone, "shares": sharesPB(rec.Shares)})
	h.log.Info("worker shares changed", "worker", req.Worker, "by", by, "access_version", rec.AccessVersion)
	h.kickQueue() // queued jobs of the new users may fit now
	return info, nil
}

// ownedWorkerLocked finds a worker the caller may manage (share, remove):
// its owner's or, for admins, any. Workers the caller may not even see are
// "not found", like on ListNodes.
func (h *Hub) ownedWorkerLocked(name, user string, admin bool) (*node, error) {
	n := h.nodes[name]
	if n == nil || n.rec.Role != string(pki.RoleWorker) || (!admin && !h.mayUseLocked(user, n)) {
		return nil, status.Errorf(codes.NotFound, "no worker %q", name)
	}
	if !admin && n.rec.Owner != user {
		return nil, status.Errorf(codes.PermissionDenied, "worker %s belongs to %s", name, ownerName(n.rec.Owner))
	}
	return n, nil
}

func ownerName(o string) string {
	if o == "" {
		return "the fleet"
	}
	return o
}

// editList adds and removes names (sorted, unique).
func editList(list, add, remove []string) []string {
	out := slices.Clone(list)
	out = append(out, add...)
	out = slices.DeleteFunc(out, func(s string) bool { return slices.Contains(remove, s) })
	sort.Strings(out)
	return slices.Compact(out)
}

// RemoveWorker revokes a worker of user's (or, for admins, any worker).
func (h *Hub) RemoveWorker(by, user string, admin bool, name string) (*pb.NodeInfo, error) {
	h.mu.Lock()
	n, err := h.ownedWorkerLocked(name, user, admin)
	var owner string
	if err == nil {
		owner = n.rec.Owner
	}
	h.mu.Unlock()
	if err != nil {
		return nil, err
	}
	info, err := h.Revoke(by, name)
	h.auditHub(by, "workers/remove", map[string]any{"worker": name, "owner": owner, "error": errString(err)})
	return info, err
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ---------------------------------------------------------------- gRPC

func (s *fleetServer) CreateWorkerCode(ctx context.Context, req *pb.CreateWorkerCodeRequest) (*pb.JoinCode, error) {
	c := callerFrom(ctx)
	return s.h.CreateWorkerCode(c.id.String(), c.id.Owner(), req)
}

func (s *fleetServer) ShareWorker(ctx context.Context, req *pb.ShareWorkerRequest) (*pb.NodeInfo, error) {
	c := callerFrom(ctx)
	return s.h.ShareWorker(c.id.String(), c.id.Owner(), c.id.Role == pki.RoleAdmin, req)
}

func (s *fleetServer) RemoveWorker(ctx context.Context, req *pb.NodeRequest) (*pb.NodeInfo, error) {
	c := callerFrom(ctx)
	return s.h.RemoveWorker(c.id.String(), c.id.Owner(), c.id.Role == pki.RoleAdmin, req.Name)
}

// ---------------------------------------------------------------- panel

// workerRoutes are the panel's own-worker actions (mirroring longarm worker):
//
//	POST /api/workercodes             {name, ttl, labels}: a worker code of the login's user (user logins)
//	POST /api/nodes/{name}/share      ShareWorkerRequest (protojson; worker = the path)
//	POST /api/nodes/{name}/remove     revoke a worker of the login's user (admins: any)
//
// A user login acts as its Longarm user; admin logins as admins.
func (h *Hub) workerRoutes(api func(string, func(http.ResponseWriter, *http.Request, string))) {
	who := func(login string) (user string, admin bool) {
		if h.panelAdmin(login) {
			return login, true
		}
		return h.panelViewer(login).User, false
	}
	api("POST /api/workercodes", func(w http.ResponseWriter, r *http.Request, login string) {
		var body struct {
			Name   string            `json:"name"`
			TTL    string            `json:"ttl"`
			Labels map[string]string `json:"labels"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		user, admin := who(login)
		if admin {
			httpError(w, http.StatusBadRequest, "administrators: POST /api/joincodes with role worker and owner")
			return
		}
		req := &pb.CreateWorkerCodeRequest{Name: body.Name, Labels: body.Labels}
		if body.TTL != "" {
			d, err := time.ParseDuration(body.TTL)
			if err != nil {
				httpError(w, http.StatusBadRequest, "bad ttl")
				return
			}
			req.Ttl = durationpb.New(d)
		}
		jc, err := h.CreateWorkerCode("web/"+login, user, req)
		writeResult(w, jc, err)
	})
	api("POST /api/nodes/{name}/share", func(w http.ResponseWriter, r *http.Request, login string) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		req := &pb.ShareWorkerRequest{}
		if err := protojson.Unmarshal(body, req); err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		req.Worker = r.PathValue("name")
		user, admin := who(login)
		info, err := h.ShareWorker("web/"+login, user, admin, req)
		writeResult(w, info, err)
	})
	api("POST /api/nodes/{name}/remove", func(w http.ResponseWriter, r *http.Request, login string) {
		user, admin := who(login)
		info, err := h.RemoveWorker("web/"+login, user, admin, r.PathValue("name"))
		writeResult(w, info, err)
	})
}

// relayCheck guards what the hub relays to a worker's session for a caller
// (kill, logs, submit: panel and longarm admin): on a user-owned worker the
// caller's user must be one who may use it - admins included, or the hub
// session would be a way around "owner only". Refusals are audited.
func (h *Hub) relayCheck(by, user, worker, action, job string) error {
	h.mu.Lock()
	n := h.nodes[worker]
	ok := n == nil || n.rec.Owner == "" || h.mayUseLocked(user, n)
	h.mu.Unlock()
	if ok {
		return nil
	}
	h.auditHub(by, "workers/denied", map[string]any{"worker": worker, "action": action, "job": job, "user": user})
	h.log.Info("hub request on a user-owned worker refused", "worker", worker, "action", action, "job", job, "by", by)
	return status.Errorf(codes.PermissionDenied, "user %q may not %s on worker %s (a user's own worker, not shared with them)",
		user, action, worker)
}

// dropWorkerCodes invalidates owner's unused worker codes (the user was
// removed or disabled: owned codes join without approval).
func (h *Hub) dropWorkerCodes(by, owner string) {
	ids, err := h.store.DeleteOwnedJoinCodes(owner)
	if err != nil {
		h.log.Warn("drop worker codes", "owner", owner, "err", err)
		return
	}
	if len(ids) > 0 {
		h.auditHub(by, "workers/codes-dropped", map[string]any{"owner": owner, "ids": ids})
		h.log.Info("worker codes dropped", "owner", owner, "ids", ids)
	}
}
