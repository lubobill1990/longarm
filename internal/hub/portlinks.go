package hub

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/portlink"
	"github.com/lubobill1990/longarm/internal/tenancy"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Port links (docs/usage.md "Port links"): a user's processes on worker A
// reach a loopback port of worker B as 127.0.0.1:<listen port> on A, for
// joint debugging. The hub stores the links, signs a grant per link with its
// hub certificate's key, pushes every worker the full list of links naming
// it (the list is authoritative: removal revokes), lets the relay carry
// worker<->worker traffic for linked pairs only, and removes links when they
// expire, their job ends, their owner is removed or a worker is revoked.
//
// Workers get no new rights here: A learns its links on its own hub
// session, and B accepts A's LinkTunnel.Dial only with a grant that names
// both and a link B's own list still has.
//
// Node identity: a link refers to its workers by ID (from_id, to_id); from
// and to are the names in their certificates (what the workers and the
// relay know them by), updated when a renamed worker renews.

// portLinkEntry is a link as the hub keeps it.
type portLinkEntry struct {
	l           *pb.PortLink // persisted fields (no status)
	status      *pb.PortLinkStatus
	grant       *pb.SignedPortLinkGrant // signed lazily
	grantSerial string                  // hub certificate that signed grant
}

// minLinkTTL is the shortest lifetime a link may ask for.
const minLinkTTL = time.Minute

func (h *Hub) loadPortLinks() error {
	rows, err := h.store.PortLinks()
	if err != nil {
		return err
	}
	now := time.Now()
	for _, r := range rows {
		l := &pb.PortLink{}
		if err := protojson.Unmarshal(r.Data, l); err != nil {
			h.log.Warn("skipping unreadable port link", "id", r.ID, "err", err)
			continue
		}
		if !l.ExpiresAt.AsTime().After(now) {
			h.store.DeletePortLink(r.ID)
			continue
		}
		if l.FromId == "" || l.ToId == "" { // not mapped by the migration: by name
			f, t := h.activeByNameLocked(l.From), h.activeByNameLocked(l.To)
			if f == nil || t == nil {
				h.log.Warn("dropping a port link of unknown workers", "id", l.Id, "from", l.From, "to", l.To)
				h.store.DeletePortLink(r.ID)
				continue
			}
			l.FromId, l.ToId = f.rec.ID, t.rec.ID
			h.savePortLinkLocked(l)
		}
		h.portLinks[l.Id] = &portLinkEntry{l: l}
	}
	return nil
}

func (h *Hub) savePortLinkLocked(l *pb.PortLink) error {
	b, err := protojson.Marshal(l)
	if err != nil {
		return err
	}
	return h.store.PutPortLink(store.PortLinkRow{ID: l.Id, Owner: l.Owner, Data: b})
}

// CreatePortLink creates a link owned by the caller.
func (h *Hub) CreatePortLink(c caller, req *pb.CreatePortLinkRequest) (*pb.PortLink, error) {
	if h.cfg.Links.Disabled {
		return nil, status.Error(codes.FailedPrecondition, "port links are disabled on this hub ([links] disabled)")
	}
	if req.From == "" || req.To == "" {
		return nil, status.Error(codes.InvalidArgument, "a link needs a worker to listen on (from) and a target worker (to)")
	}
	if !portlink.ValidPort(req.Port) {
		return nil, status.Errorf(codes.InvalidArgument, "bad target port %d", req.Port)
	}
	listen := req.ListenPort
	if listen == 0 {
		listen = req.Port
	}
	if !portlink.ValidPort(listen) || listen < 1024 {
		return nil, status.Errorf(codes.InvalidArgument, "bad listen port %d (1024-65535)", listen)
	}
	ttl := h.cfg.Links.DefaultTTL.Duration
	if req.Ttl != nil {
		ttl = req.Ttl.AsDuration()
	}
	owner := c.id.Owner()
	now := time.Now()
	expires := now.Add(ttl)
	id := ""
	if req.Consent != nil {
		// The owner's signed consent (end-to-end trust): the link takes id
		// and expiry from it and must match it exactly - workers that trust
		// users check it against the owner's key; the hub can only check
		// that it is well-formed and the caller's.
		s, _, err := portlink.ParseConsent(req.Consent)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		if s.Owner != owner {
			return nil, status.Errorf(codes.PermissionDenied, "the link's consent is signed for %s, not for %s", s.Owner, owner)
		}
		if s.From != req.From || s.To != req.To || s.Port != req.Port || s.ListenPort != listen || s.JobId != req.JobId {
			return nil, status.Error(codes.InvalidArgument, "the link's consent does not match the request")
		}
		expires = time.Unix(s.ExpiresUnix, 0)
		if d := expires.Sub(now); d <= 0 || d > h.cfg.Links.MaxTTL.Duration+portlink.ClockSkew {
			return nil, status.Errorf(codes.InvalidArgument, "the link's consent expires at %s: at most %s from now ([links] max_ttl)",
				expires.UTC().Format(time.RFC3339), h.cfg.Links.MaxTTL.Duration)
		}
		id, ttl = s.Id, expires.Sub(now)
	} else if ttl < minLinkTTL || ttl > h.cfg.Links.MaxTTL.Duration {
		return nil, status.Errorf(codes.InvalidArgument, "ttl %s: between %s and %s ([links] max_ttl)", ttl, minLinkTTL, h.cfg.Links.MaxTTL.Duration)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	v := h.viewerLocked(owner, c.id.Role == pki.RoleAdmin)
	var ends [2]*node
	for i, target := range []string{req.From, req.To} {
		// User-owned workers: the link's owner must be allowed to use both
		// ends (admins are no exception); hidden workers stay hidden.
		n, err := h.resolveLocked(target, func(n *node) bool { return isWorker(n) && h.mayUseLocked(owner, n) }, "worker")
		if err != nil {
			return nil, err
		}
		if n.rec.State != stateApproved {
			return nil, status.Errorf(codes.NotFound, "no worker %q", target)
		}
		ends[i] = n
	}
	from, to := ends[0], ends[1]
	if from == to {
		return nil, status.Error(codes.InvalidArgument, "from and to are the same worker (use a local port there)")
	}
	if req.Consent != nil && (req.From != from.rec.DialName() || req.To != to.rec.DialName()) {
		// workers hold the signed spec to the names they know each other by
		return nil, status.Errorf(codes.InvalidArgument, "a link with consent names its workers as their certificates do: %s, %s",
			from.rec.DialName(), to.rec.DialName())
	}
	if why := h.portLinkExposesLocked(from.rec.ID, to.rec.ID, v.Admin); why != "" {
		return nil, status.Errorf(codes.FailedPrecondition, "%s (an admin may create it)", why)
	}
	if req.JobId != "" {
		e := h.jobs[from.rec.ID][req.JobId]
		if e == nil || !v.CanRead(tenancy.OwnerOf(e.job.GetSpec()), e.job.GetSpec().GetShareTeams()) {
			return nil, status.Errorf(codes.NotFound, "no job %s on %s", req.JobId, req.From)
		}
		if !v.CanControl(tenancy.OwnerOf(e.job.GetSpec())) {
			return nil, status.Errorf(codes.PermissionDenied, "job %s is not yours", req.JobId)
		}
		if terminal(e.job.State) {
			return nil, status.Errorf(codes.FailedPrecondition, "job %s has ended", req.JobId)
		}
	}
	mine := 0
	for _, e := range h.portLinks {
		if e.l.Owner == owner {
			mine++
		}
		if e.l.FromId == from.rec.ID && e.l.ListenPort == listen {
			if e.l.Owner == owner || v.Admin {
				return nil, status.Errorf(codes.AlreadyExists, "port %d on %s is taken by link %s", listen, from.rec.Name, e.l.Id)
			}
			return nil, status.Errorf(codes.AlreadyExists, "port %d on %s is taken by another link (choose --port)", listen, from.rec.Name)
		}
	}
	if mine >= h.cfg.Links.MaxPerUser {
		return nil, status.Errorf(codes.ResourceExhausted, "quota: %d port links (max %d, [links] max_per_user)", mine, h.cfg.Links.MaxPerUser)
	}
	if id == "" {
		id = portlink.NewID()
		for h.portLinks[id] != nil {
			id = portlink.NewID()
		}
	} else if h.portLinks[id] != nil {
		return nil, status.Errorf(codes.AlreadyExists, "link %s exists", id)
	}
	l := &pb.PortLink{Id: id, Owner: owner, From: from.rec.DialName(), To: to.rec.DialName(), Port: req.Port, ListenPort: listen,
		CreatedAt: timestamppb.New(now), ExpiresAt: timestamppb.New(expires), JobId: req.JobId, CreatedBy: c.id.String(),
		Consent: req.Consent, FromId: from.rec.ID, ToId: to.rec.ID}
	if err := h.savePortLinkLocked(l); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	h.portLinks[id] = &portLinkEntry{l: l}
	h.auditHub(c.id.String(), "links/create", map[string]any{"link": id, "owner": owner, "from": l.From, "to": l.To,
		"from_id": l.FromId, "to_id": l.ToId,
		"port": l.Port, "listen_port": l.ListenPort, "expires": l.ExpiresAt.AsTime().UTC().Format(time.RFC3339), "job": l.JobId})
	h.log.Info("port link created", "link", id, "owner", owner, "from", l.From, "to", fmt.Sprintf("%s:%d", l.To, l.Port), "listen", l.ListenPort, "ttl", ttl)
	// telemetry: no link, owner, worker or port
	h.tel().Event(context.Background(), "link.created", map[string]string{"role": string(c.id.Role), "job_bound": boolStr(l.JobId != "")},
		map[string]float64{"ttl_h": ttl.Hours()})
	h.portLinksChangedLocked(l.FromId, l.ToId)
	return h.portLinkViewLocked(h.portLinks[id]), nil
}

// ListPortLinks lists the caller's links (all: every user's, admins only).
func (h *Hub) ListPortLinks(c caller, all bool) (*pb.ListPortLinksResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v := h.viewerLocked(c.id.Owner(), c.id.Role == pki.RoleAdmin)
	if all && !v.Admin {
		return nil, status.Error(codes.PermissionDenied, "only admins list every user's links")
	}
	return h.listPortLinksLocked(func(l *pb.PortLink) bool { return all || l.Owner == v.User }), nil
}

func (h *Hub) listPortLinksLocked(keep func(*pb.PortLink) bool) *pb.ListPortLinksResponse {
	out := &pb.ListPortLinksResponse{}
	for _, e := range h.portLinks {
		if keep(e.l) {
			out.Links = append(out.Links, h.portLinkViewLocked(e))
		}
	}
	sort.Slice(out.Links, func(i, j int) bool {
		a, b := out.Links[i], out.Links[j]
		if !a.CreatedAt.AsTime().Equal(b.CreatedAt.AsTime()) {
			return a.CreatedAt.AsTime().Before(b.CreatedAt.AsTime())
		}
		return a.Id < b.Id
	})
	return out
}

// portLinkViewLocked is a link with its status for clients.
func (h *Hub) portLinkViewLocked(e *portLinkEntry) *pb.PortLink {
	l := proto.Clone(e.l).(*pb.PortLink)
	switch {
	case e.status != nil:
		l.Status = proto.Clone(e.status).(*pb.PortLinkStatus)
	default:
		l.Status = &pb.PortLinkStatus{Id: l.Id, State: "pending"}
	}
	if n := h.nodes[l.FromId]; n == nil || n.sess == nil {
		l.Status.State = "offline"
		l.Status.Error = "worker " + l.From + " is offline"
	}
	// the workers' display names (from / to are their certificates')
	if n := h.nodes[l.FromId]; n != nil {
		l.From = n.rec.Name
	}
	if n := h.nodes[l.ToId]; n != nil {
		l.To = n.rec.Name
	}
	return l
}

// RemovePortLink removes one of the caller's links (admins: any).
func (h *Hub) RemovePortLink(c caller, id string) (*pb.PortLink, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v := h.viewerLocked(c.id.Owner(), c.id.Role == pki.RoleAdmin)
	e := h.portLinks[id]
	if e == nil || (!v.Admin && e.l.Owner != v.User) {
		return nil, status.Errorf(codes.NotFound, "no link %s", id)
	}
	l := h.portLinkViewLocked(e)
	h.removePortLinkLocked(e, c.id.String(), "removed")
	return l, nil
}

// removePortLinkLocked deletes a link, audits it and tells both workers.
func (h *Hub) removePortLinkLocked(e *portLinkEntry, by, reason string) {
	l := e.l
	delete(h.portLinks, l.Id)
	if err := h.store.DeletePortLink(l.Id); err != nil {
		h.log.Warn("delete port link", "link", l.Id, "err", err)
	}
	h.auditHub(by, "links/remove", map[string]any{"link": l.Id, "owner": l.Owner, "from": l.From, "to": l.To,
		"from_id": l.FromId, "to_id": l.ToId, "port": l.Port, "listen_port": l.ListenPort, "reason": reason})
	h.log.Info("port link removed", "link", l.Id, "owner", l.Owner, "by", by, "reason", reason)
	h.tel().Event(context.Background(), "link.removed", map[string]string{"reason": linkRemovalClass(reason)},
		map[string]float64{"age_s": time.Since(l.CreatedAt.AsTime()).Seconds()})
	h.portLinksChangedLocked(l.FromId, l.ToId)
}

// linkRemovalClass is a removal reason without names (telemetry).
func linkRemovalClass(reason string) string {
	switch {
	case strings.HasPrefix(reason, "not allowed"):
		return "not_allowed"
	case reason == "removed", reason == "expired", reason == "job ended", reason == "user removed", reason == "node revoked":
		return strings.ReplaceAll(reason, " ", "_")
	}
	return "other"
}

// removePortLinksLocked removes every link matching cond.
func (h *Hub) removePortLinksLocked(cond func(*pb.PortLink) bool, by, reason string) {
	var del []*portLinkEntry
	for _, e := range h.portLinks {
		if cond(e.l) {
			del = append(del, e)
		}
	}
	for _, e := range del {
		h.removePortLinkLocked(e, by, reason)
	}
}

// portLinkExposesLocked says why a link from -> to would expose a user's
// private port ("" = it does not): a shared-isolation source lets every local
// process use the listener, so it would open a per-user target's port to the
// other users there. Links an admin creates are exempt. from, to: IDs.
func (h *Hub) portLinkExposesLocked(from, to string, admin bool) string {
	if admin || !h.tenancyOnLocked() {
		return ""
	}
	f, t := h.nodes[from], h.nodes[to]
	if f == nil || t == nil || f.rec.Isolation == tenancy.PerUser || t.rec.Isolation != tenancy.PerUser {
		return ""
	}
	return fmt.Sprintf("%s isolates users and %s does not: everybody on %s could use the link", t.rec.Name, f.rec.Name, f.rec.Name)
}

// recheckPortLinksLocked removes the links that are no longer allowed: their
// target turned per-user (or separation turned on) after they were created,
// or their owner may no longer use one of the workers (unshared, left a
// team). Called whenever tenancy, a worker's isolation or its shares may
// have changed, and at start.
func (h *Hub) recheckPortLinksLocked() {
	why := map[*portLinkEntry]string{}
	for _, e := range h.portLinks {
		admin := strings.HasPrefix(e.l.CreatedBy, string(pki.RoleAdmin)+"/")
		if w := h.portLinkExposesLocked(e.l.FromId, e.l.ToId, admin); w != "" {
			why[e] = w
			continue
		}
		for _, id := range []string{e.l.FromId, e.l.ToId} {
			if n := h.nodes[id]; n != nil && !h.mayUseLocked(e.l.Owner, n) {
				why[e] = fmt.Sprintf("%s may no longer use %s", e.l.Owner, n.rec.Name)
				break
			}
		}
	}
	for e, w := range why {
		h.removePortLinkLocked(e, "hub", "not allowed any more: "+w)
	}
}

// sweepPortLinksLocked removes expired links.
func (h *Hub) sweepPortLinksLocked(now time.Time) {
	h.removePortLinksLocked(func(l *pb.PortLink) bool { return !l.ExpiresAt.AsTime().After(now) }, "hub", "expired")
}

// portLinkJobsLocked removes links whose job on worker (an ID) ended (or
// vanished from the worker).
func (h *Hub) portLinkJobsLocked(worker string) {
	jm := h.jobs[worker]
	h.removePortLinksLocked(func(l *pb.PortLink) bool {
		if l.FromId != worker || l.JobId == "" {
			return false
		}
		e := jm[l.JobId]
		return e == nil || terminal(e.job.State)
	}, "hub", "job ended")
}

// portLinksChangedLocked pushes the link lists of the workers (IDs) and
// updates the relay's worker pairs.
func (h *Hub) portLinksChangedLocked(workers ...string) {
	h.portLinksVersion++
	done := map[string]bool{}
	for _, w := range workers {
		if done[w] {
			continue
		}
		done[w] = true
		if n := h.nodes[w]; n != nil && n.sess != nil {
			h.sendLocked(n, h.portLinksMsgLocked(w))
		}
	}
	h.updateRelayPairsLocked()
}

// updateRelayPairsLocked lets the relay carry traffic between linked
// workers (and nothing else between workers); the relay knows workers by
// their certificate names.
func (h *Hub) updateRelayPairsLocked() {
	if h.relay == nil {
		return
	}
	pairs := make([][2]string, 0, len(h.portLinks))
	for _, e := range h.portLinks {
		pairs = append(pairs, [2]string{e.l.From, e.l.To})
	}
	h.relay.SetWorkerPairs(pairs)
}

// portLinksMsgLocked is the list of links naming worker (an ID), with
// grants and the targets' endpoints for the links it listens for.
func (h *Hub) portLinksMsgLocked(worker string) *pb.HubCommand {
	msg := &pb.PortLinks{Version: h.portLinksVersion}
	for _, e := range h.portLinks {
		l := e.l
		if l.FromId != worker && l.ToId != worker {
			continue
		}
		a := &pb.PortLinkAssignment{Link: proto.Clone(l).(*pb.PortLink)}
		if l.FromId == worker {
			g, err := h.portLinkGrantLocked(e)
			if err != nil {
				h.log.Warn("sign port link grant", "link", l.Id, "err", err)
				continue
			}
			a.Grant = g
			if n := h.nodes[l.ToId]; n != nil {
				a.TargetEndpoints = append([]string(nil), n.rec.Endpoints...)
			}
		}
		msg.Links = append(msg.Links, a)
	}
	sort.Slice(msg.Links, func(i, j int) bool { return msg.Links[i].Link.Id < msg.Links[j].Link.Id })
	return &pb.HubCommand{Msg: &pb.HubCommand_PortLinks{PortLinks: msg}}
}

// portLinkGrantLocked returns the link's grant, signing it (again) when the
// hub certificate changed.
func (h *Hub) portLinkGrantLocked(e *portLinkEntry) (*pb.SignedPortLinkGrant, error) {
	cert := h.hubCert.Certificate()
	serial := pki.Serial(cert.Leaf)
	if e.grant != nil && e.grantSerial == serial {
		return e.grant, nil
	}
	l := e.l
	g, err := portlink.Sign(cert, &pb.PortLinkGrant{Id: l.Id, Owner: l.Owner, From: l.From, To: l.To, Port: l.Port,
		ListenPort: l.ListenPort, ExpiresUnix: l.ExpiresAt.AsTime().Unix(), IssuedUnix: time.Now().Unix()})
	if err != nil {
		return nil, err
	}
	e.grant, e.grantSerial = g, serial
	return g, nil
}

// portLinkTargetChangedLocked re-sends the lists of the workers listening
// for links to target (an ID; its endpoints changed).
func (h *Hub) portLinkTargetChangedLocked(target string) {
	var from []string
	for _, e := range h.portLinks {
		if e.l.ToId == target {
			from = append(from, e.l.FromId)
		}
	}
	if len(from) > 0 {
		h.portLinksChangedLocked(from...)
	}
}

// dialNameChangedLocked follows a worker's new certificate name (a renamed
// worker renewed): its links name it by that now (new grants), both ends
// get their lists again and the relay its pairs.
func (h *Hub) dialNameChangedLocked(n *node) {
	var touched []string
	for _, e := range h.portLinks {
		changed := false
		if e.l.FromId == n.rec.ID && e.l.From != n.rec.DialName() {
			e.l.From, changed = n.rec.DialName(), true
		}
		if e.l.ToId == n.rec.ID && e.l.To != n.rec.DialName() {
			e.l.To, changed = n.rec.DialName(), true
		}
		if changed {
			e.grant = nil
			if err := h.savePortLinkLocked(e.l); err != nil {
				h.log.Warn("save port link", "link", e.l.Id, "err", err)
			}
			touched = append(touched, e.l.FromId, e.l.ToId)
		}
	}
	if len(touched) > 0 {
		h.portLinksChangedLocked(touched...)
	}
}

// applyPortLinkReportLocked stores the status a worker reports for the
// links it listens for.
func (h *Hub) applyPortLinkReportLocked(n *node, rep *pb.PortLinkReport) {
	now := timestamppb.Now()
	for _, s := range rep.Links {
		e := h.portLinks[s.GetId()]
		if e == nil || e.l.FromId != n.rec.ID {
			continue
		}
		s.UpdatedAt = now
		e.status = s
	}
}

// ---------------------------------------------------------------- gRPC

func (s *fleetServer) CreatePortLink(ctx context.Context, req *pb.CreatePortLinkRequest) (*pb.PortLink, error) {
	return s.h.CreatePortLink(callerFrom(ctx), req)
}

func (s *fleetServer) ListPortLinks(ctx context.Context, req *pb.ListPortLinksRequest) (*pb.ListPortLinksResponse, error) {
	return s.h.ListPortLinks(callerFrom(ctx), req.All)
}

func (s *fleetServer) RemovePortLink(ctx context.Context, req *pb.RemovePortLinkRequest) (*pb.PortLink, error) {
	return s.h.RemovePortLink(callerFrom(ctx), req.Id)
}

// ---------------------------------------------------------------- panel

// portLinkRoutes are the panel's view of links:
//
//	GET    /api/port-links         the login's links (admins: everybody's)
//	DELETE /api/port-links/{id}    remove one (users: their own)
func (h *Hub) portLinkRoutes(api func(string, func(http.ResponseWriter, *http.Request, string))) {
	api("GET /api/port-links", func(w http.ResponseWriter, _ *http.Request, user string) {
		v := h.panelViewer(user)
		h.mu.Lock()
		res := h.listPortLinksLocked(func(l *pb.PortLink) bool { return v.Admin || l.Owner == v.User })
		h.mu.Unlock()
		writePB(w, res)
	})
	api("DELETE /api/port-links/{id}", func(w http.ResponseWriter, r *http.Request, user string) {
		v := h.panelViewer(user)
		id := r.PathValue("id")
		h.mu.Lock()
		e := h.portLinks[id]
		if e == nil || (!v.Admin && e.l.Owner != v.User) {
			h.mu.Unlock()
			writeResult(w, nil, status.Errorf(codes.NotFound, "no link %s", id))
			return
		}
		l := h.portLinkViewLocked(e)
		h.removePortLinkLocked(e, "web/"+user, "removed")
		h.mu.Unlock()
		writeResult(w, l, nil)
	})
}
