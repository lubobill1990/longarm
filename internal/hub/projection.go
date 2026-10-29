package hub

import (
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"

	"google.golang.org/protobuf/proto"

	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/tenancy"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Projection (docs/security.md "What admins see"): every record the hub
// sends out - events, jobs, rollouts, profiles, join codes, port links,
// audit records, users - goes through a projector for its caller, which
// hides other users' machines (names, actors) the caller may not see.
//
// The projector needs no hub lock: event filters run under the bus lock,
// often while the publisher holds the hub's. It reads a copy-on-write
// directory of every node record the hub stored (name -> role, user,
// owner, shares), updated whenever a node record is saved.

// dirEntry is what projection needs of a node.
type dirEntry struct {
	role, user, owner string
	adminWorker       bool
	shares            *pb.WorkerShares
}

type directory map[string]dirEntry

type nodeDirectory struct {
	mu sync.Mutex // writers
	p  atomic.Pointer[directory]
}

func (d *nodeDirectory) load() directory {
	if p := d.p.Load(); p != nil {
		return *p
	}
	return nil
}

// indexNode records (or updates) a node in the hub's directory (the
// store's OnNodePut hook; tests that add nodes directly call it too).
func (h *Hub) indexNode(r *store.Node) {
	e := dirEntry{role: r.Role, owner: r.Owner, adminWorker: r.Admin, shares: sharesPB(r.Shares)}
	if r.Role != string(pki.RoleWorker) {
		e.user, e.owner = nodeUser(r), ""
	}
	h.dir.mu.Lock()
	defer h.dir.mu.Unlock()
	old := h.dir.load()
	next := make(directory, len(old)+1)
	for k, v := range old {
		next[k] = v
	}
	next[r.Name] = e
	h.dir.p.Store(&next)
}

// visibleEntry is canSeeNode for a directory entry.
func visibleEntry(v tenancy.Caller, e dirEntry) bool {
	if v.Admin {
		return true
	}
	if e.role != string(pki.RoleWorker) {
		return e.user == v.User
	}
	if e.adminWorker {
		return e.owner != "" && e.owner == v.User
	}
	return tenancy.MayUseWorker(v.User, v.Teams, e.owner, e.shares)
}

// projector projects outbound records for one caller.
type projector struct {
	v tenancy.Caller
	d directory
}

func (h *Hub) projector(v tenancy.Caller) projector {
	return projector{v: v, d: h.dir.load()}
}

// known reports whether name is a node the hub knows.
func (p projector) known(name string) bool { _, ok := p.d[name]; return ok }

// hidden reports whether name is a node the caller may not see (names that
// are no node are not hidden: they name nothing to look up).
func (p projector) hidden(name string) bool {
	if p.v.Admin {
		return false
	}
	e, ok := p.d[name]
	return ok && !visibleEntry(p.v, e)
}

// actorTokenRE: an actor ("client/x", "admin/x", "worker/x") or a bare name.
var actorTokenRE = regexp.MustCompile(`(?:\b(client|admin|worker)/)?[A-Za-z0-9._-]+`)

// actorOf projects one token: a hidden actor becomes its role and user
// ("admin bob"; a worker: "worker (another user's machine)"), a hidden
// bare name the placeholder.
func (p projector) token(t string) string {
	role, name, isActor := strings.Cut(t, "/")
	if !isActor || (role != "client" && role != "admin" && role != "worker") {
		if p.hidden(t) {
			return hiddenNode
		}
		return t
	}
	if !p.hidden(name) {
		return t
	}
	e := p.d[name]
	if role == "worker" || e.user == "" {
		return role + " " + hiddenNode
	}
	return role + " " + e.user
}

// text projects free text (messages, JSON lines): hidden actors and names.
func (p projector) text(s string) string {
	if p.v.Admin || s == "" {
		return s
	}
	return actorTokenRE.ReplaceAllStringFunc(s, p.token)
}

// actor projects an actor identifier ("admin/bob-laptop" -> "admin bob").
func (p projector) actor(who string) string { return p.text(who) }

// names keeps the visible ones.
func (p projector) names(ns []string) []string {
	var out []string
	for _, n := range ns {
		if !p.hidden(n) {
			out = append(out, n)
		}
	}
	return out
}

// fleetJob projects a job the caller may read (team-shared jobs of others
// included): a hidden worker and the submitting device are not named.
func (p projector) fleetJob(fj *pb.FleetJob) *pb.FleetJob {
	if p.v.Admin || fj == nil {
		return fj
	}
	if !p.hidden(fj.Worker) {
		if sb := fj.Job.GetSpec().GetSubmittedBy(); sb == "" || p.actor(sb) == sb {
			return fj
		}
	}
	fj = proto.Clone(fj).(*pb.FleetJob)
	if p.hidden(fj.Worker) {
		fj.Worker = hiddenNode
	}
	if s := fj.Job.GetSpec(); s != nil {
		s.SubmittedBy = p.actor(s.SubmittedBy)
	}
	return fj
}

// event projects a fleet event for the caller (nil: not for it). Every
// event kind goes through here; unknown kinds are dropped.
func (p projector) event(ev *pb.FleetEvent, owner string) *pb.FleetEvent {
	if p.v.Admin {
		return ev
	}
	// each kind keeps only its own fields: an event carrying more than its
	// kind uses can't smuggle them past the projection
	switch ev.Kind {
	case "job":
		if ev.Job == nil || !canSeeJob(p.v, ev.Job.Job) {
			return nil
		}
		return &pb.FleetEvent{Time: ev.Time, Kind: ev.Kind, Node: p.text(ev.Node), Message: p.text(ev.Message),
			Job: p.fleetJob(proto.Clone(ev.Job).(*pb.FleetJob))}
	case "queue":
		if owner == "" || owner != p.v.User {
			return nil
		}
		return &pb.FleetEvent{Time: ev.Time, Kind: ev.Kind, Node: p.text(ev.Node), Message: p.text(ev.Message)}
	case "node", "metrics":
		out := &pb.FleetEvent{Time: ev.Time, Kind: ev.Kind, Node: ev.Node}
		if ev.NodeInfo == nil {
			// no record to check: only a node the directory knows and
			// the caller may see (fail closed)
			if !p.known(ev.Node) || p.hidden(ev.Node) {
				return nil
			}
			out.Message = p.text(ev.Message)
			return out
		}
		if !canSeeNode(p.v, ev.NodeInfo) || (ev.Node != "" && ev.Node != ev.NodeInfo.Name) {
			return nil
		}
		out.NodeInfo = nodeView(p.v, proto.Clone(ev.NodeInfo).(*pb.NodeInfo))
		out.Message = ev.Message
		// a message naming the owner-only admin worker is the owner's
		if a := ev.NodeInfo.Companion; a != "" && out.NodeInfo.Companion == "" && strings.Contains(out.Message, a) {
			out.Message = "updated"
		}
		out.Message = p.text(out.Message)
		return out
	case "hub": // the hub restarting: about nobody's data
		return &pb.FleetEvent{Time: ev.Time, Kind: ev.Kind, Message: p.text(ev.Message)}
	default: // links, release, ...: administration of the whole fleet
		return nil
	}
}

// rollout projects a rollout: counts of every list, visible names only.
func (p projector) rollout(r *pb.Rollout) *pb.Rollout {
	if r == nil {
		return nil
	}
	r = proto.Clone(r).(*pb.Rollout)
	r.CanaryCount, r.TargetedCount, r.UpdatedCount = int32(len(r.Canary)), int32(len(r.Targeted)), int32(len(r.Updated))
	r.FailedCount, r.WaitingCount, r.NotCapableCount = int32(len(r.Failed)), int32(len(r.Waiting)), int32(len(r.NotCapable))
	if p.v.Admin {
		return r
	}
	r.Canary, r.Targeted, r.Updated = p.names(r.Canary), p.names(r.Targeted), p.names(r.Updated)
	r.Failed, r.Waiting, r.NotCapable = p.names(r.Failed), p.names(r.Waiting), p.names(r.NotCapable)
	r.Message = p.text(r.Message)
	return r
}

// profile projects a toolchain profile (listings and mutation answers).
func (p projector) profile(pr *pb.Profile) *pb.Profile {
	if pr == nil || p.v.Admin {
		return pr
	}
	pr = proto.Clone(pr).(*pb.Profile)
	if pr.Canary != "" && p.hidden(pr.Canary) {
		pr.Canary = hiddenNode
	}
	pr.Message = p.text(pr.Message)
	return pr
}

// joinCode projects a join code the caller may list.
func (p projector) joinCode(jc *pb.JoinCode) *pb.JoinCode {
	if p.v.Admin {
		return jc
	}
	if jc.UsedBy != "" && (!p.known(jc.UsedBy) || p.hidden(jc.UsedBy)) {
		jc.UsedBy = "used"
	}
	jc.CreatedBy = p.actor(jc.CreatedBy)
	return jc
}

// tenantUser projects a user record (created_by may be another admin's device).
func (p projector) tenantUser(u *pb.TenantUser) *pb.TenantUser {
	if u == nil || p.v.Admin {
		return u
	}
	u.CreatedBy = p.actor(u.CreatedBy)
	return u
}

// auditRecord projects an audit record the caller may read. A hub record
// about another user (the caller acted on it: removed its link, labelled
// its worker, removed the user) keeps time, actor, method and the user it
// is about, nothing else; other records get their names projected.
func (p projector) auditRecord(r store.AuditRow) *pb.AuditRecord {
	out := &pb.AuditRecord{Node: r.Node, Time: tspb(r.Time), Who: p.actor(r.Who), Method: r.Method}
	if p.v.Admin {
		out.Who, out.Json = r.Who, r.Line
		return out
	}
	if r.Node == "hub" && r.SubjectUser != "" && r.SubjectUser != p.v.User {
		b, _ := json.Marshal(map[string]any{"time": r.Time.UTC().Format("2006-01-02T15:04:05.000Z07:00"), "who": out.Who,
			"method": r.Method, "about_user": r.SubjectUser, "redacted": "details of another user's things"})
		out.Json = string(b)
		return out
	}
	out.Node, out.Json = p.text(r.Node), p.text(r.Line)
	return out
}
