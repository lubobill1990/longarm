package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/tenancy"
	"github.com/lubobill1990/longarm/internal/update"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// What admins see (docs/security.md "What admins see"). With multi-tenancy
// an admin certificate or panel login sees machines and jobs exactly as its
// user does (viewer, panelViewer): its own workers and devices, the workers
// shared with it, fleet workers. Admin answers that would otherwise name
// other users' machines - rollouts, toolchain canaries, tailnet status,
// link reports, join codes, operations by name - are cut down to that view
// here; the fleet as a whole is visible only as the anonymous FleetSummary.
// The audit log too: AuditFor and projector.auditRecord.

// visibleNodeLocked reports whether v may see node n (as ListNodes shows it).
func (h *Hub) visibleNodeLocked(v tenancy.Caller, n *node) bool {
	if v.Admin {
		return true
	}
	r := n.rec
	if r.Role != string(pki.RoleWorker) {
		return nodeUser(r) == v.User
	}
	if r.Admin {
		return r.Owner != "" && r.Owner == v.User
	}
	return tenancy.MayUseWorker(v.User, v.Teams, r.Owner, sharesPB(r.Shares))
}

// visibleNameLocked is visibleNodeLocked by name (or node ID); unknown
// names are left alone (they name nothing anybody could look up). Names
// are unique among non-revoked nodes - as names and as the names in their
// certificates (a renamed node answers to its old one until it renews) -
// so a non-revoked node of that name decides; else (only revoked nodes had
// it, names are reused) the name is visible only if every one of them is.
func (h *Hub) visibleNameLocked(v tenancy.Caller, name string) bool {
	if v.Admin {
		return true
	}
	if n := h.nodes[name]; n != nil { // an ID
		return h.visibleNodeLocked(v, n)
	}
	if n := h.nameTakenLocked(name, ""); n != nil {
		return h.visibleNodeLocked(v, n)
	}
	for _, n := range h.nodes {
		if (n.rec.Name == name || n.rec.DialName() == name) && !h.visibleNodeLocked(v, n) {
			return false
		}
	}
	return true
}

// visibleNamesLocked keeps the names v may see.
func (h *Hub) visibleNamesLocked(v tenancy.Caller, names []string) []string {
	if v.Admin {
		return names
	}
	return slices.DeleteFunc(slices.Clone(names), func(s string) bool { return !h.visibleNameLocked(v, s) })
}

var nameToken = regexp.MustCompile(`[A-Za-z0-9._-]+`)

// hiddenNode replaces the names of nodes in free text that v may not see.
const hiddenNode = "(another user's machine)"

// rolloutForLocked is the rollout as v may see it (projector.rollout).
func (h *Hub) rolloutForLocked(v tenancy.Caller, r *pb.Rollout) *pb.Rollout {
	return h.projector(v).rollout(r)
}

// rolloutFor is rolloutForLocked for callers without the lock.
func (h *Hub) rolloutFor(v tenancy.Caller, r *pb.Rollout, err error) (*pb.Rollout, error) {
	if err != nil || r == nil {
		return r, err
	}
	return h.projector(v).rollout(r), nil
}

// ProfilesFor lists the toolchain profiles as v may see them (projector.profile).
func (h *Hub) ProfilesFor(v tenancy.Caller) []*pb.Profile {
	p := h.projector(v)
	out := h.Profiles()
	for i, pr := range out {
		out[i] = p.profile(pr)
	}
	return out
}

// LinksFor returns the fresh link reports of v's own devices, with the
// links to workers v may see.
func (h *Hub) LinksFor(v tenancy.Caller) *pb.LinksResponse {
	all := h.Links()
	if v.Admin {
		return all
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out := &pb.LinksResponse{}
	for _, r := range all.Reports {
		n := h.nodes[r.ClientId]
		if n == nil && r.ClientId == "" { // reported before node IDs
			n = h.nameTakenLocked(r.Client, "")
		}
		if n == nil || !h.visibleNodeLocked(v, n) {
			continue
		}
		r = proto.Clone(r).(*pb.LinkReport)
		r.Links = slices.DeleteFunc(r.Links, func(l *pb.WorkerLink) bool { return !h.visibleNameLocked(v, l.Worker) })
		out.Reports = append(out.Reports, r)
	}
	return out
}

// ---------------------------------------------------------------- summary

// FleetSummary is HubAdmin.FleetSummary: per user, counts of machines by
// role and state (online for workers), and the versions - fleet-wide and,
// with rows, per user. Aggregates only: no names, IDs, host names, OS,
// last-seen times, labels, CPU or memory, processes, sessions or jobs,
// nothing per machine that could be joined back to a device.
func (h *Hub) FleetSummary(rows bool) *pb.FleetSummaryResponse {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	resp := &pb.FleetSummaryResponse{Time: timestamppb.New(now.Truncate(time.Minute)), Totals: &pb.FleetCounts{},
		Tenancy: h.tenancyOnLocked()}
	users := map[string]*pb.UserFleetSummary{}
	userVersions := map[string]map[string]*pb.VersionCount{}
	get := func(u string) *pb.UserFleetSummary {
		s := users[u]
		if s == nil {
			s = &pb.UserFleetSummary{User: u, Counts: &pb.FleetCounts{}}
			users[u] = s
			userVersions[u] = map[string]*pb.VersionCount{}
		}
		return s
	}
	count := func(m map[string]*pb.VersionCount, version string) *pb.VersionCount {
		vc := m[version]
		if vc == nil {
			vc = &pb.VersionCount{Version: version}
			m[version] = vc
		}
		return vc
	}
	for name := range h.tenants {
		get(name)
	}
	versions := map[string]*pb.VersionCount{}
	for _, n := range h.nodes {
		r := n.rec
		if r.State == stateRevoked {
			continue
		}
		worker := r.Role == string(pki.RoleWorker)
		if !worker && r.Role != string(pki.RoleClient) && r.Role != string(pki.RoleAdmin) {
			continue // relays, release credentials: nobody's machines
		}
		user := r.Owner
		if !worker {
			user = nodeUser(r)
		}
		s := get(user)
		online := worker && n.sess != nil
		for _, c := range []*pb.FleetCounts{s.Counts, resp.Totals} {
			switch {
			case r.State == statePending:
				c.Pending++
			case worker && r.Admin:
				c.AdminWorkers++
				if online {
					c.AdminWorkersOnline++
				}
			case worker:
				c.Workers++
				if online {
					c.WorkersOnline++
				}
			default:
				c.Clients++
			}
		}
		if r.State == statePending {
			continue
		}
		if worker {
			vc := count(versions, r.Version)
			vc.Workers++
			if online {
				vc.Online++
			}
		}
		if rows {
			vc := count(userVersions[user], r.Version)
			if worker {
				vc.Workers++
				if online {
					vc.Online++
				}
			} else {
				vc.Clients++
			}
		}
	}
	newestFirst := func(m map[string]*pb.VersionCount) []*pb.VersionCount {
		var out []*pb.VersionCount
		for _, vc := range m {
			out = append(out, vc)
		}
		sort.Slice(out, func(i, j int) bool { return update.Compare(out[i].Version, out[j].Version) > 0 })
		return out
	}
	for u, s := range users {
		if rows {
			s.Versions = newestFirst(userVersions[u])
		}
		resp.Users = append(resp.Users, s)
	}
	sort.Slice(resp.Users, func(i, j int) bool {
		a, b := resp.Users[i].User, resp.Users[j].User
		if (a == "") != (b == "") {
			return b == "" // the fleet workers last
		}
		return a < b
	})
	resp.WorkerVersions = newestFirst(versions)
	return resp
}

func tsTime(t *timestamppb.Timestamp) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.AsTime()
}

func (s *hubAdminServer) FleetSummary(ctx context.Context, req *pb.FleetSummaryRequest) (*pb.FleetSummaryResponse, error) {
	resp := s.h.FleetSummary(req.Rows)
	s.h.auditHub(by(ctx), "fleet/summary", map[string]any{"rows": req.Rows})
	return resp, nil
}

// summaryRoutes: GET /api/admin/summary?rows=1 (panel administrators;
// audited), the panel's fleet-wide view.
func (h *Hub) summaryRoutes(admin func(string, func(http.ResponseWriter, *http.Request, string))) {
	admin("GET /api/admin/summary", func(w http.ResponseWriter, r *http.Request, user string) {
		rows := r.URL.Query().Get("rows")
		resp := h.FleetSummary(rows == "1" || rows == "true")
		h.auditHub("web/"+user, "fleet/summary", map[string]any{"rows": rows != ""})
		writePB(w, resp)
	})
}

// countedNamesLocked describes names (workers in some state) for v: the
// count, and the names of those v may see - "12 workers can't self-update
// (3 yours: a, b, c)". Hidden ones are counted, never named.
func (h *Hub) countedNamesLocked(v tenancy.Caller, names []string, what string) string {
	shown := h.visibleNamesLocked(v, names)
	s := fmt.Sprintf("%d worker(s) %s", len(names), what)
	if len(shown) > 0 {
		s += fmt.Sprintf(" (%d you may see: %s)", len(shown), strings.Join(shown, ", "))
	}
	return s
}

// ---------------------------------------------------------------- audit

// AuditFor is the audit trail as v may read it (Fleet.Audit, /api/audit).
// With multi-tenancy - admins included - only the records about v's user,
// by the attribution fixed when each was written (never by today's node
// ownership: a re-enrolled name doesn't reopen its old records): records
// whose actor was the user (any of its devices, its panel login) and
// records about the user (calls on its workers, hub actions on its things).
// Records from before attribution count only by their own "user" field.
// Every other record is only counted by method, and records are projected
// (projector.auditRecord). The complete trail stays in the hub's database
// and log, which only the hub's operators read (longarm-hub audit).
func (h *Hub) AuditFor(v tenancy.Caller, by string, req *pb.AuditRequest) (*pb.AuditResponse, error) {
	var since time.Time
	if req.Since != nil {
		since = req.Since.AsTime()
	}
	limit := int(req.Limit)
	if limit <= 0 || limit > 10000 {
		limit = 200
	}
	// counts: whether records v may not read are counted by method - in the
	// fleet-wide log and the hub's own records only, never for a machine
	counts := req.Node == ""
	nodeID, nodeName := "", ""
	if req.Node != "" {
		// a node filter names a machine: v must be able to see it first.
		// Unknown and hidden machines answer alike - nothing - and the
		// answer never counts other users' records on it (that would be
		// the activity of a machine v may not see, or of its other users).
		// A target (name, ID, ID prefix) resolves among the nodes v may
		// see only: a hidden node is as unknown as a missing one, and an
		// ambiguous prefix lists visible candidates only.
		if req.Node == "hub" {
			counts, nodeName = true, "hub" // the hub's own records (auditHub)
		} else {
			h.mu.Lock()
			n, err := h.resolveLocked(req.Node, h.visibleLocked(v), "node")
			if err == nil {
				nodeID = n.rec.ID
			}
			h.mu.Unlock()
			if status.Code(err) == codes.InvalidArgument {
				return nil, err
			}
			if err != nil {
				return &pb.AuditResponse{}, nil
			}
		}
	}
	fetch := limit
	if !v.Admin {
		fetch = 10000 // filtered below: look further back for the caller's own
	}
	rows, err := h.store.Audit(nodeID, nodeName, since, fetch)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	p := h.projector(v)
	out := &pb.AuditResponse{}
	hidden := map[string]int32{}
	for _, r := range rows {
		if !v.Admin && !auditAbout(v, r) {
			if counts {
				hidden[r.Method]++
				out.HiddenTotal++
			}
			continue
		}
		if len(out.Records) < limit {
			out.Records = append(out.Records, p.auditRecord(r))
		}
	}
	for m, n := range hidden {
		out.Hidden = append(out.Hidden, &pb.AuditCount{Method: m, Count: n})
	}
	sort.Slice(out.Hidden, func(i, j int) bool { return out.Hidden[i].Method < out.Hidden[j].Method })
	return out, nil
}

// auditAbout reports whether record r is about v's user, by what was
// recorded when it was written.
func auditAbout(v tenancy.Caller, r store.AuditRow) bool {
	if v.User == "" {
		return false
	}
	if r.Attributed {
		return r.ActorUser == v.User || r.SubjectUser == v.User
	}
	var rec struct {
		User string `json:"user"`
	}
	json.Unmarshal([]byte(r.Line), &rec)
	return rec.User == v.User
}

// auditActorUser is the Longarm user behind an audit "who" at the time of
// writing: a certificate (role/name: the node's user), a panel login
// (web/<login>: its tenant; an administrator without one: the login).
// Lock-free (the directory): auditHub runs with and without the hub lock.
func (h *Hub) auditActorUser(who string) string {
	who, _, _ = strings.Cut(who, " ") // "<who> via hub", "<who> (remove user x)"
	role, name, ok := strings.Cut(who, "/")
	if !ok {
		return ""
	}
	switch role {
	case string(pki.RoleClient), string(pki.RoleAdmin):
		if e, ok := h.dir.load()[name]; ok && e.user != "" {
			return e.user
		}
		return name // a certificate the hub didn't enroll (admin issue): its name is its user
	case "web":
		if pu, err := h.store.GetUser(name); err == nil {
			if pu.Tenant != "" {
				return pu.Tenant
			}
			if pu.Admin() {
				return name
			}
		}
	}
	return ""
}
