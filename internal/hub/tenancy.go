package hub

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/tenancy"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Multi-tenancy on the hub (docs/security.md): Longarm users with teams
// and quotas, what each caller may see (its own jobs, team-shared jobs
// read-only; admins everything), fleet-wide quotas in the scheduler, and
// the tenancy policy pushed to workers (who enforce ownership, per-worker
// quotas and - with per-user isolation - OS separation).

// tenant is a Longarm user as the hub keeps it.
type tenant struct {
	u *pb.TenantUser
}

func (h *Hub) loadTenants() error {
	rows, err := h.store.Tenants()
	if err != nil {
		return err
	}
	for _, r := range rows {
		u := &pb.TenantUser{}
		if err := protojson.Unmarshal(r.Data, u); err != nil {
			h.log.Warn("skipping unreadable user", "user", r.Name, "err", err)
			continue
		}
		h.tenants[r.Name] = &tenant{u: u}
	}
	v, _ := h.store.Meta("tenancy_version")
	h.tenancyVersion, _ = strconv.ParseInt(v, 10, 64)
	if p, _ := h.store.Meta("tenancy_purge"); p != "" {
		var pl pb.TenancyPolicy
		if protojson.Unmarshal([]byte(p), &pl) == nil {
			h.purged = pl.Purge
		}
	}
	return nil
}

// tenancyOnLocked: users are separated once a user exists, a worker
// isolates users or has an owner, or [tenancy] enabled is set.
func (h *Hub) tenancyOnLocked() bool {
	if h.cfg.Tenancy.Enabled || len(h.tenants) > 0 {
		return true
	}
	for _, n := range h.nodes {
		if (n.rec.Isolation == tenancy.PerUser || n.rec.Owner != "") && n.rec.State != stateRevoked {
			return true
		}
	}
	return false
}

// TenancyOn reports whether users are separated.
func (h *Hub) TenancyOn() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tenancyOnLocked()
}

// viewerLocked is how the hub sees a caller: its user, admin or not, its
// teams. Without multi-tenancy everybody sees everything (as before).
func (h *Hub) viewerLocked(user string, admin bool) tenancy.Caller {
	c := tenancy.Caller{User: user, Admin: admin}
	if t := h.tenants[user]; t != nil {
		c.Teams = t.u.Teams
	}
	if !h.tenancyOnLocked() {
		c.Admin = true
	}
	return c
}

// viewer of a gRPC caller, for what it may read (nodes, jobs, events,
// history): admin certificates get exactly their user's view - their own
// workers and devices, the workers shared with them, fleet workers, their
// own and team-shared jobs. The only fleet-wide view of machines admins have
// is the anonymous FleetSummary (docs/security.md "What admins see").
// Without multi-tenancy (one user) everybody sees everything.
func (h *Hub) viewer(c caller) tenancy.Caller {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.viewerLocked(c.id.Owner(), false)
}

// opNodeView is what an admin operation on a node by name (approve, revoke,
// drain, label, profile, remove ...) returns: the node as v sees it as a
// user, or - for nodes v may not see (other users' private workers and
// devices) - only what v already knows: the name, role, owner, state and
// drain, none of the machine's details.
func opNodeView(v tenancy.Caller, n *pb.NodeInfo) *pb.NodeInfo {
	if n == nil {
		return nil
	}
	if canSeeNode(v, n) {
		return nodeView(v, n)
	}
	return &pb.NodeInfo{Name: n.Name, Role: n.Role, State: n.State, Owner: n.Owner, User: n.User, Online: n.Online,
		Draining: n.Draining, AdminDrain: n.AdminDrain, AdminWorker: n.AdminWorker}
}

// opResult applies opNodeView to an operation's (info, err) result.
func (h *Hub) opResult(v tenancy.Caller) func(*pb.NodeInfo, error) (*pb.NodeInfo, error) {
	return func(info *pb.NodeInfo, err error) (*pb.NodeInfo, error) {
		if err != nil {
			return nil, err
		}
		return opNodeView(v, info), nil
	}
}

// canSeeJob reports whether v may read j.
func canSeeJob(v tenancy.Caller, j *pb.Job) bool {
	return v.CanRead(tenancy.OwnerOf(j.GetSpec()), j.GetSpec().GetShareTeams())
}

// nodeView strips what a non-admin must not see of a node: other users'
// process lists and mirrors (repository names).
func nodeView(v tenancy.Caller, n *pb.NodeInfo) *pb.NodeInfo {
	if v.Admin || n == nil {
		return n
	}
	n = proto.Clone(n).(*pb.NodeInfo)
	if n.Metrics != nil {
		n.Metrics.TopCpu, n.Metrics.TopMem = nil, nil
	}
	if n.Owner != v.User { // whom else a worker is shared with is its owner's business
		n.Shares = nil
		// so is its admin worker (owner-only): the machine is the worker's
		if n.Companion != "" && !n.AdminWorker {
			n.Companion, n.Machine = "", n.Name
		}
	}
	own := func(repos []string) []string {
		var out []string
		for _, r := range repos {
			if u, repo, ok := cutUserRepo(r); ok && u == v.User {
				out = append(out, repo)
			}
		}
		return out
	}
	n.Mirrors, n.WarmRepos = own(n.Mirrors), own(n.WarmRepos)
	return n
}

// cutUserRepo splits a per-user worker's mirror entry "<user>/<repo>".
func cutUserRepo(s string) (user, repo string, ok bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return s[:i], s[i+1:], true
		}
	}
	return "", s, false
}

// quotaLocked is user's effective quota.
func (h *Hub) quotaLocked(user string) *pb.Quota {
	var over *pb.Quota
	if t := h.tenants[user]; t != nil {
		over = t.u.Quota
	}
	return tenancy.MergeQuota(h.cfg.Tenancy.DefaultQuota.PB(), over)
}

// usageLocked is user's fleet-wide active jobs and reserved memory
// (including placements not yet submitted).
func (h *Hub) usageLocked(user string) *pb.QuotaUsage {
	u := &pb.QuotaUsage{}
	for _, jm := range h.jobs {
		for _, e := range jm {
			if terminal(e.job.State) || tenancy.OwnerOf(e.job.GetSpec()) != user {
				continue
			}
			u.Jobs++
			u.MemGb += jobMemGB(e.job, h.params)
		}
	}
	for _, r := range h.reservations {
		if r.owner == user {
			u.Jobs++
			u.MemGb += r.memGB
		}
	}
	return u
}

// overQuotaLocked says why user may not start a job of memGB now ("" = ok).
func (h *Hub) overQuotaLocked(user string, memGB float64) string {
	if !h.tenancyOnLocked() || user == "" {
		return ""
	}
	q, u := h.quotaLocked(user), h.usageLocked(user)
	if q.MaxJobs > 0 && u.Jobs+1 > q.MaxJobs {
		return fmt.Sprintf("quota: %d active jobs of %s in the fleet (max %d)", u.Jobs, user, q.MaxJobs)
	}
	if q.MaxMemGb > 0 && u.MemGb+memGB > q.MaxMemGb+1e-9 {
		return fmt.Sprintf("quota: %.1f GB reserved by %s's jobs + %.1f GB > %.1f GB", u.MemGb, user, memGB, q.MaxMemGb)
	}
	return ""
}

// ---------------------------------------------------------------- policy

// policyLocked is the tenancy policy for workers.
func (h *Hub) policyLocked() *pb.TenancyPolicy {
	p := &pb.TenancyPolicy{Version: h.tenancyVersion, Enabled: h.tenancyOnLocked(),
		DefaultQuota: h.cfg.Tenancy.DefaultQuota.PB(), Purge: append([]string(nil), h.purged...)}
	names := make([]string, 0, len(h.tenants))
	for n := range h.tenants {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		u := h.tenants[n].u
		p.Users = append(p.Users, &pb.TenantUser{Name: u.Name, Teams: u.Teams, Quota: u.Quota, Disabled: u.Disabled})
	}
	return p
}

// tenancyChangedLocked bumps the policy version and pushes it to every
// connected worker.
func (h *Hub) tenancyChangedLocked() {
	h.tenancyVersion++
	h.store.SetMeta("tenancy_version", strconv.FormatInt(h.tenancyVersion, 10))
	h.lastTenancyOn = h.tenancyOnLocked()
	h.store.SetMeta("tenancy_on", strconv.FormatBool(h.lastTenancyOn))
	cmd := &pb.HubCommand{Msg: &pb.HubCommand_Tenancy{Tenancy: h.policyLocked()}}
	for _, n := range h.nodes {
		if n.rec.Role == string(pki.RoleWorker) {
			h.sendLocked(n, cmd)
		}
	}
	h.recheckPortLinksLocked()
}

// checkTenancyFlipLocked pushes a new policy when separation turned on or
// off (a worker with per-user isolation joined or left) and removes the port
// links a worker's (new) isolation no longer allows.
func (h *Hub) checkTenancyFlipLocked() {
	if on := h.tenancyOnLocked(); on != h.lastTenancyOn {
		h.tenancyChangedLocked()
	}
	h.recheckPortLinksLocked()
}

func (h *Hub) saveTenantLocked(u *pb.TenantUser) error {
	b, err := protojson.Marshal(u)
	if err != nil {
		return err
	}
	return h.store.PutTenant(u.Name, b)
}

// ---------------------------------------------------------------- users

// devicesLocked lists the client/admin nodes of user.
func (h *Hub) devicesLocked(user string) []string {
	var out []string
	for _, n := range h.nodes {
		if n.rec.Role == string(pki.RoleWorker) || n.rec.State == stateRevoked {
			continue
		}
		if nodeUser(n.rec) == user {
			out = append(out, n.rec.Name)
		}
	}
	sort.Strings(out)
	return out
}

// nodeUser is the user of a client/admin node.
func nodeUser(r *store.Node) string {
	if r.User != "" {
		return r.User
	}
	return r.Name
}

func (h *Hub) userInfoLocked(t *tenant) *pb.TenantUser {
	u := proto.Clone(t.u).(*pb.TenantUser)
	// counts, not names: admins don't learn other users' device names
	u.DeviceCount = int32(len(h.devicesLocked(u.Name)))
	for _, n := range h.nodes {
		if n.rec.Owner == u.Name && n.rec.Role == string(pki.RoleWorker) && n.rec.State != stateRevoked {
			u.WorkerCount++
		}
	}
	u.Usage = h.usageLocked(u.Name)
	if pu, err := h.store.GetUser(u.Name); err == nil && pu.Tenant == u.Name {
		u.Panel = true
	}
	return u
}

func validQuota(q *pb.Quota) error {
	if q == nil {
		return nil
	}
	if q.MaxJobs < 0 || q.MaxMemGb < 0 || q.MaxJobsPerWorker < 0 || q.MaxMemGbPerWorker < 0 || q.MaxSessionsPerWorker < 0 ||
		q.JobMemGb < 0 || q.MaxJobMemGb < 0 || q.JobCpus < 0 || q.MaxJobCpus < 0 {
		return status.Error(codes.InvalidArgument, "quota values must not be negative")
	}
	return nil
}

// PutUser creates or updates a user (teams, quota, disabled) and can issue
// a join code for a device of the user and a panel login.
func (h *Hub) PutUser(by string, req *pb.PutUserRequest) (*pb.PutUserResponse, error) {
	if !pki.ValidUser(req.Name) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid user name %q (a-z 0-9 _ -, starting with a letter, at most 17 characters)", req.Name)
	}
	for _, t := range req.Teams {
		if !tenancy.ValidTeam(t) {
			return nil, status.Errorf(codes.InvalidArgument, "invalid team name %q", t)
		}
	}
	if err := validQuota(req.Quota); err != nil {
		return nil, err
	}
	h.mu.Lock()
	t := h.tenants[req.Name]
	if t != nil && req.Create {
		h.mu.Unlock()
		return nil, status.Errorf(codes.AlreadyExists, "user %s exists", req.Name)
	}
	if n := h.nodes[req.Name]; t == nil && n != nil && n.rec.Role == string(pki.RoleWorker) {
		h.mu.Unlock()
		return nil, status.Errorf(codes.AlreadyExists, "%s is a worker's name", req.Name)
	}
	created := t == nil
	if created {
		t = &tenant{u: &pb.TenantUser{Name: req.Name, CreatedAt: tspb(time.Now().UTC()), CreatedBy: by}}
	}
	u := proto.Clone(t.u).(*pb.TenantUser)
	if req.SetTeams || created {
		teams := slices.Clone(req.Teams)
		sort.Strings(teams)
		u.Teams = slices.Compact(teams)
	}
	if req.SetQuota || (created && req.Quota != nil) {
		u.Quota = req.Quota
		if tenancy.QuotaEmpty(u.Quota) {
			u.Quota = nil
		}
	}
	if req.SetDisabled {
		u.Disabled = req.Disabled
	}
	if req.SetCanShare {
		u.CanShare = req.CanShare
	}
	if err := h.saveTenantLocked(u); err != nil {
		h.mu.Unlock()
		return nil, status.Error(codes.Internal, err.Error())
	}
	t.u = u
	h.tenants[u.Name] = t
	// a user added again after a purge is not purged any more
	h.purged = slices.DeleteFunc(h.purged, func(s string) bool { return s == u.Name })
	h.savePurgeLocked()
	h.tenancyChangedLocked()
	h.mu.Unlock()
	h.auditHub(by, map[bool]string{true: "users/add", false: "users/update"}[created], map[string]any{
		"user": u.Name, "teams": u.Teams, "disabled": u.Disabled, "quota": protoJSON(u.Quota), "can_share": u.CanShare})
	h.log.Info("user saved", "user", u.Name, "by", by, "created", created)
	if u.Disabled {
		h.closeUserConns(u.Name)
		h.dropWorkerCodes(by, u.Name)
	}

	resp := &pb.PutUserResponse{}
	if req.JoinCode {
		role := string(pki.RoleClient)
		if req.Admin {
			role = string(pki.RoleAdmin)
		}
		ttl := req.Ttl
		if ttl == nil {
			ttl = durationpb.New(24 * time.Hour)
		}
		device := req.Device
		if device == "" {
			device = h.freeDeviceName(u.Name)
		}
		jc, err := h.CreateJoinCode(by, &pb.CreateJoinCodeRequest{Role: role, Ttl: ttl, Name: device, User: u.Name})
		if err != nil {
			return nil, err
		}
		resp.JoinCode = jc
	}
	if req.Panel {
		pw := randomPassword()
		secret, err := AddPanelUser(h.store, u.Name, pw, "user", u.Name, true)
		if err != nil {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		h.store.DeleteUserSessions(u.Name)
		resp.PanelPassword, resp.TotpSecret, resp.Otpauth = pw, secret, TOTPURI(u.Name, secret)
		h.auditHub(by, "users/panel", map[string]any{"user": u.Name})
	}
	h.mu.Lock()
	resp.User = h.userInfoLocked(h.tenants[u.Name])
	h.mu.Unlock()
	return resp, nil
}

// freeDeviceName is user, or user-2, user-3 ... (the first free node name).
func (h *Hub) freeDeviceName(user string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	taken := func(n string) bool {
		x := h.nodes[n]
		return x != nil && x.rec.State != stateRevoked
	}
	if !taken(user) {
		return user
	}
	for i := 2; ; i++ {
		if n := fmt.Sprintf("%s-%d", user, i); !taken(n) {
			return n
		}
	}
}

func randomPassword() string {
	const chars = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"
	b := make([]byte, 20)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		b[i] = chars[n.Int64()]
	}
	return string(b)
}

func protoJSON(m proto.Message) string {
	if m == nil {
		return ""
	}
	b, _ := protojson.Marshal(m)
	return string(b)
}

// ListUsers returns every user with devices and usage.
func (h *Hub) ListUsers() *pb.ListUsersResponse {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := &pb.ListUsersResponse{DefaultQuota: h.cfg.Tenancy.DefaultQuota.PB(), Tenancy: h.tenancyOnLocked()}
	names := make([]string, 0, len(h.tenants))
	for n := range h.tenants {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		out.Users = append(out.Users, h.userInfoLocked(h.tenants[n]))
	}
	return out
}

// RemoveUser revokes every certificate of the user (all devices), deletes
// the user and its panel login and - with purge - has every worker delete
// its jobs, data and OS account.
func (h *Hub) RemoveUser(by string, req *pb.RemoveUserRequest) (*pb.RemoveUserResponse, error) {
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "missing user")
	}
	h.mu.Lock()
	_, known := h.tenants[req.Name]
	devices := h.devicesLocked(req.Name)
	// the user's own workers go too: nobody else could use them anyway
	for _, n := range h.nodes {
		if n.rec.Owner == req.Name && n.rec.Role == string(pki.RoleWorker) && n.rec.State != stateRevoked {
			devices = append(devices, n.rec.Name)
		}
	}
	h.mu.Unlock()
	if !known && len(devices) == 0 && !req.Purge {
		return nil, status.Errorf(codes.NotFound, "no user %q", req.Name)
	}
	resp := &pb.RemoveUserResponse{Purge: req.Purge}
	for _, d := range devices {
		if _, err := h.Revoke(by+" (remove user "+req.Name+")", d); err != nil {
			return nil, err
		}
		resp.RevokedCount++
	}
	h.mu.Lock()
	delete(h.tenants, req.Name)
	h.entraForgetLocked(req.Name) // the name may go to someone else
	if err := h.store.DeleteTenant(req.Name); err != nil {
		h.mu.Unlock()
		return nil, status.Error(codes.Internal, err.Error())
	}
	h.removePortLinksLocked(func(l *pb.PortLink) bool { return l.Owner == req.Name }, by, "user removed")
	if req.Purge && !slices.Contains(h.purged, req.Name) {
		h.purged = append(h.purged, req.Name)
		h.savePurgeLocked()
	}
	h.tenancyChangedLocked()
	h.mu.Unlock()
	if pu, err := h.store.GetUser(req.Name); err == nil && pu.Tenant == req.Name {
		h.store.DeleteUser(req.Name)
	}
	h.dropWorkerCodes(by, req.Name)
	// the audit log, which admins read, counts the revoked machines; their
	// names go to the hub's own log only (the operators': docs/security.md)
	h.auditHub(by, "users/remove", map[string]any{"user": req.Name, "purge": req.Purge, "revoked": len(devices)})
	h.log.Info("user removed", "user", req.Name, "by", by, "purge", req.Purge, "revoked", devices)
	return resp, nil
}

func (h *Hub) savePurgeLocked() {
	b, _ := protojson.Marshal(&pb.TenancyPolicy{Purge: h.purged})
	h.store.SetMeta("tenancy_purge", string(b))
}

// closeUserConns ends what a disabled user still has open on the hub: the
// connections of its devices (event streams, waits) and its panel
// sessions. Workers close theirs when the policy arrives.
func (h *Hub) closeUserConns(user string) {
	n := h.conns.CloseIf(func(c *x509.Certificate) bool {
		id, err := pki.IdentityFromCert(c)
		return err == nil && (id.Role == pki.RoleClient || id.Role == pki.RoleAdmin) && id.Owner() == user
	})
	if pu, err := h.store.GetUser(user); err == nil && pu.Tenant == user {
		h.store.DeleteUserSessions(user)
	}
	h.log.Info("user disabled: connections closed", "user", user, "connections", n)
}

// SetTeam adds and removes members of a team.
func (h *Hub) SetTeam(by string, req *pb.SetTeamRequest) (*pb.ListTeamsResponse, error) {
	if !tenancy.ValidTeam(req.Team) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid team name %q", req.Team)
	}
	h.mu.Lock()
	for _, m := range append(slices.Clone(req.Add), req.Remove...) {
		if h.tenants[m] == nil {
			h.mu.Unlock()
			return nil, status.Errorf(codes.NotFound, "no user %q (longarm admin user add %s)", m, m)
		}
	}
	for _, m := range req.Add {
		u := proto.Clone(h.tenants[m].u).(*pb.TenantUser)
		if !slices.Contains(u.Teams, req.Team) {
			u.Teams = append(u.Teams, req.Team)
			sort.Strings(u.Teams)
		}
		if err := h.saveTenantLocked(u); err != nil {
			h.mu.Unlock()
			return nil, status.Error(codes.Internal, err.Error())
		}
		h.tenants[m].u = u
	}
	for _, m := range req.Remove {
		u := proto.Clone(h.tenants[m].u).(*pb.TenantUser)
		u.Teams = slices.DeleteFunc(u.Teams, func(t string) bool { return t == req.Team })
		if err := h.saveTenantLocked(u); err != nil {
			h.mu.Unlock()
			return nil, status.Error(codes.Internal, err.Error())
		}
		h.tenants[m].u = u
	}
	h.tenancyChangedLocked()
	h.mu.Unlock()
	h.auditHub(by, "teams/set", map[string]any{"team": req.Team, "add": req.Add, "remove": req.Remove})
	return h.ListTeams(), nil
}

// ListTeams lists teams (from the users' memberships).
func (h *Hub) ListTeams() *pb.ListTeamsResponse {
	h.mu.Lock()
	defer h.mu.Unlock()
	teams := map[string][]string{}
	for _, t := range h.tenants {
		for _, team := range t.u.Teams {
			teams[team] = append(teams[team], t.u.Name)
		}
	}
	out := &pb.ListTeamsResponse{}
	for name, members := range teams {
		sort.Strings(members)
		out.Teams = append(out.Teams, &pb.Team{Name: name, Members: members})
	}
	sort.Slice(out.Teams, func(i, j int) bool { return out.Teams[i].Name < out.Teams[j].Name })
	return out
}

// userDisabledLocked reports whether a user may not use the hub.
func (h *Hub) userDisabledLocked(user string) bool {
	if t := h.tenants[user]; t != nil && t.u.Disabled {
		return true
	}
	return slices.Contains(h.purged, user)
}

// ---------------------------------------------------------------- gRPC

func (s *hubAdminServer) PutUser(ctx context.Context, req *pb.PutUserRequest) (*pb.PutUserResponse, error) {
	return s.h.PutUser(by(ctx), req)
}

func (s *hubAdminServer) ListUsers(ctx context.Context, _ *pb.ListUsersRequest) (*pb.ListUsersResponse, error) {
	return s.h.ListUsersFor(s.h.viewer(callerFrom(ctx))), nil
}

// ListUsersFor is ListUsers projected for v (created_by may name another
// admin's device).
func (h *Hub) ListUsersFor(v tenancy.Caller) *pb.ListUsersResponse {
	r := h.ListUsers()
	p := h.projector(v)
	for _, u := range r.Users {
		p.tenantUser(u)
	}
	return r
}

func (s *hubAdminServer) RemoveUser(ctx context.Context, req *pb.RemoveUserRequest) (*pb.RemoveUserResponse, error) {
	return s.h.RemoveUser(by(ctx), req)
}

func (s *hubAdminServer) SetTeam(ctx context.Context, req *pb.SetTeamRequest) (*pb.ListTeamsResponse, error) {
	return s.h.SetTeam(by(ctx), req)
}

func (s *hubAdminServer) ListTeams(context.Context, *pb.ListTeamsRequest) (*pb.ListTeamsResponse, error) {
	return s.h.ListTeams(), nil
}

// eventFilter is what a viewer may see of the event stream (projector.event):
// its jobs and those shared with its teams, nodes it may see without other
// users' process lists and mirrors, its own queue events; names of machines
// it may not see projected out of every message.
func (h *Hub) eventFilter(v tenancy.Caller) func(*pb.FleetEvent, string) *pb.FleetEvent {
	if v.Admin {
		return nil
	}
	return func(ev *pb.FleetEvent, owner string) *pb.FleetEvent {
		return h.projector(v).event(ev, owner) // the directory as it is now
	}
}

// eventFilter without a hub (tests): no directory, so events without a
// node record are dropped.
func eventFilter(v tenancy.Caller) func(*pb.FleetEvent, string) *pb.FleetEvent {
	return (&Hub{}).eventFilter(v)
}

// nodeUserOf is the user of a client/admin NodeInfo.
func nodeUserOf(n *pb.NodeInfo) string {
	if n.User != "" {
		return n.User
	}
	return n.Name
}

// ---------------------------------------------------------------- panel

// panelAdmin reports whether a panel account administers the hub.
func (h *Hub) panelAdmin(user string) bool {
	u, err := h.store.GetUser(user)
	return err == nil && u.Admin()
}

// panelViewer is how the hub sees a panel account: what its Longarm user
// may see. Administrators are no exception (with multi-tenancy): they see
// their own user's machines and jobs (the login's tenant, else the login
// name), and the fleet only through the anonymous summary (/api/admin/summary).
func (h *Hub) panelViewer(user string) tenancy.Caller {
	u, err := h.store.GetUser(user)
	h.mu.Lock()
	defer h.mu.Unlock()
	if err != nil {
		return tenancy.Caller{User: user} // vanished meanwhile: nothing
	}
	if u.Admin() {
		name := user
		if u.Tenant != "" {
			name = u.Tenant
		}
		return h.viewerLocked(name, false)
	}
	v := h.viewerLocked(u.Tenant, false)
	v.Admin = false // a user login never sees others' jobs, tenancy or not
	return v
}

// adminOnly refuses panel accounts that are not administrators.
func (h *Hub) adminOnly(fn func(http.ResponseWriter, *http.Request, string)) func(http.ResponseWriter, *http.Request, string) {
	return func(w http.ResponseWriter, r *http.Request, user string) {
		if !h.panelAdmin(user) {
			httpError(w, http.StatusForbidden, "administrators only")
			return
		}
		fn(w, r, user)
	}
}

// mayControl checks that v may kill job id.
func (h *Hub) mayControl(v tenancy.Caller, id, worker string) error {
	fj, err := h.GetJobFor(v, id, worker)
	if err != nil {
		return err
	}
	if !v.CanControl(tenancy.OwnerOf(fj.Job.GetSpec())) {
		return status.Errorf(codes.PermissionDenied, "job %s is not yours (shared jobs are read-only)", id)
	}
	return nil
}

// tenancyRoutes are the panel's user/team/quota administration
// (mirroring longarm admin user|team):
//
//	GET    /api/tenants                 users, teams, devices, usage, default quota
//	POST   /api/tenants                 PutUserRequest (protojson): add / update / join code / panel login
//	DELETE /api/tenants/{name}?purge=1  remove (revoke devices; purge data on workers)
//	GET    /api/teams
//	POST   /api/teams/{team}            {"add": [...], "remove": [...]}
func (h *Hub) tenancyRoutes(api, admin func(string, func(http.ResponseWriter, *http.Request, string))) {
	admin("GET /api/tenants", func(w http.ResponseWriter, _ *http.Request, user string) {
		writePB(w, h.ListUsersFor(h.panelViewer(user)))
	})
	admin("POST /api/tenants", func(w http.ResponseWriter, r *http.Request, user string) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		req := &pb.PutUserRequest{}
		if err := protojson.Unmarshal(body, req); err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		res, err := h.PutUser("web/"+user, req)
		writeResult(w, res, err)
	})
	admin("DELETE /api/tenants/{name}", func(w http.ResponseWriter, r *http.Request, user string) {
		p := r.URL.Query().Get("purge")
		res, err := h.RemoveUser("web/"+user, &pb.RemoveUserRequest{Name: r.PathValue("name"), Purge: p == "1" || p == "true"})
		writeResult(w, res, err)
	})
	admin("GET /api/teams", func(w http.ResponseWriter, _ *http.Request, _ string) {
		writePB(w, h.ListTeams())
	})
	admin("POST /api/teams/{team}", func(w http.ResponseWriter, r *http.Request, user string) {
		var body struct {
			Add    []string `json:"add"`
			Remove []string `json:"remove"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		res, err := h.SetTeam("web/"+user, &pb.SetTeamRequest{Team: r.PathValue("team"), Add: body.Add, Remove: body.Remove})
		writeResult(w, res, err)
	})
}

// panelDisabled reports whether a panel login acts for a disabled user.
func (h *Hub) panelDisabled(login string) bool {
	u, err := h.store.GetUser(login)
	if err != nil || u.Admin() {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.userDisabledLocked(u.Tenant)
}
