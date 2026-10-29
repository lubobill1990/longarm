package hub

import (
	"context"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

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
// The audit log, an admin security tool, still names nodes.

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

// visibleNameLocked is visibleNodeLocked by name; unknown names are left
// alone (they name nothing anybody could look up).
func (h *Hub) visibleNameLocked(v tenancy.Caller, name string) bool {
	n := h.nodes[name]
	return n == nil || h.visibleNodeLocked(v, n)
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

// scrubNamesLocked replaces node names v may not see in a message.
func (h *Hub) scrubNamesLocked(v tenancy.Caller, s string) string {
	if v.Admin || s == "" {
		return s
	}
	return nameToken.ReplaceAllStringFunc(s, func(t string) string {
		if n := h.nodes[t]; n != nil && !h.visibleNodeLocked(v, n) {
			return hiddenNode
		}
		return t
	})
}

// rolloutForLocked is the rollout as v may see it: counts of every list,
// names of v's visible workers only.
func (h *Hub) rolloutForLocked(v tenancy.Caller, r *pb.Rollout) *pb.Rollout {
	r = proto.Clone(r).(*pb.Rollout)
	r.CanaryCount, r.TargetedCount, r.UpdatedCount = int32(len(r.Canary)), int32(len(r.Targeted)), int32(len(r.Updated))
	r.FailedCount, r.WaitingCount, r.NotCapableCount = int32(len(r.Failed)), int32(len(r.Waiting)), int32(len(r.NotCapable))
	r.Canary, r.Targeted, r.Updated = h.visibleNamesLocked(v, r.Canary), h.visibleNamesLocked(v, r.Targeted), h.visibleNamesLocked(v, r.Updated)
	r.Failed, r.Waiting, r.NotCapable = h.visibleNamesLocked(v, r.Failed), h.visibleNamesLocked(v, r.Waiting), h.visibleNamesLocked(v, r.NotCapable)
	r.Message = h.scrubNamesLocked(v, r.Message)
	return r
}

// rolloutFor is rolloutForLocked for callers without the lock.
func (h *Hub) rolloutFor(v tenancy.Caller, r *pb.Rollout, err error) (*pb.Rollout, error) {
	if err != nil || r == nil {
		return r, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.rolloutForLocked(v, r), nil
}

// ProfilesFor lists the toolchain profiles; a canary v may not see is not named.
func (h *Hub) ProfilesFor(v tenancy.Caller) []*pb.Profile {
	out := h.Profiles()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range out {
		if p.Canary != "" && !h.visibleNameLocked(v, p.Canary) {
			p.Canary = hiddenNode
		}
		p.Message = h.scrubNamesLocked(v, p.Message)
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
		if n := h.nodes[r.Client]; n == nil || !h.visibleNodeLocked(v, n) {
			continue
		}
		r = proto.Clone(r).(*pb.LinkReport)
		r.Links = slices.DeleteFunc(r.Links, func(l *pb.WorkerLink) bool { return !h.visibleNameLocked(v, l.Worker) })
		out.Reports = append(out.Reports, r)
	}
	return out
}

// ---------------------------------------------------------------- summary

// machineRole is a FleetMachine's role.
func machineRole(worker, adminWorker, adminCert bool) string {
	switch {
	case worker && adminWorker:
		return "admin-worker"
	case worker:
		return "worker"
	case adminCert:
		return "admin"
	}
	return "client"
}

// FleetSummary is HubAdmin.FleetSummary: per user, counts of machines by
// role and state, optional anonymous rows and the workers' versions. No
// names, IDs, host names, labels, metrics, processes, sessions or jobs.
func (h *Hub) FleetSummary(rows bool) *pb.FleetSummaryResponse {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	resp := &pb.FleetSummaryResponse{Time: timestamppb.New(now.Truncate(time.Minute)), Totals: &pb.FleetCounts{},
		Tenancy: h.tenancyOnLocked()}
	users := map[string]*pb.UserFleetSummary{}
	get := func(u string) *pb.UserFleetSummary {
		s := users[u]
		if s == nil {
			s = &pb.UserFleetSummary{User: u, Counts: &pb.FleetCounts{}}
			users[u] = s
		}
		return s
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
		var seen time.Time
		if worker {
			seen = r.LastSeen
			if n.sess != nil {
				seen = n.lastReport
			}
			seen = seen.Truncate(time.Minute)
			if seen.After(tsTime(s.LastSeen)) {
				s.LastSeen = timestamppb.New(seen)
			}
			if r.State != statePending {
				vc := versions[r.Version]
				if vc == nil {
					vc = &pb.VersionCount{Version: r.Version}
					versions[r.Version] = vc
				}
				vc.Workers++
				if online {
					vc.Online++
				}
			}
		}
		if rows {
			m := &pb.FleetMachine{Role: machineRole(worker, r.Admin, r.Role == string(pki.RoleAdmin)),
				Online: online, Version: r.Version, Os: r.OS}
			if !seen.IsZero() {
				m.LastSeen = timestamppb.New(seen)
			}
			s.Machines = append(s.Machines, m)
		}
	}
	for _, s := range users {
		// no stable order beyond what the rows say: role, state, version, OS
		sort.SliceStable(s.Machines, func(i, j int) bool {
			a, b := s.Machines[i], s.Machines[j]
			if a.Role != b.Role {
				return a.Role < b.Role
			}
			if a.Online != b.Online {
				return a.Online
			}
			if a.Version != b.Version {
				return update.Compare(a.Version, b.Version) > 0
			}
			if a.Os != b.Os {
				return a.Os < b.Os
			}
			return tsTime(a.LastSeen).After(tsTime(b.LastSeen))
		})
		resp.Users = append(resp.Users, s)
	}
	sort.Slice(resp.Users, func(i, j int) bool {
		a, b := resp.Users[i].User, resp.Users[j].User
		if (a == "") != (b == "") {
			return b == "" // the fleet workers last
		}
		return a < b
	})
	for _, vc := range versions {
		resp.WorkerVersions = append(resp.WorkerVersions, vc)
	}
	sort.Slice(resp.WorkerVersions, func(i, j int) bool {
		return update.Compare(resp.WorkerVersions[i].Version, resp.WorkerVersions[j].Version) > 0
	})
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
