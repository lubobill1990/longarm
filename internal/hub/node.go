package hub

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/sched"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

type hubNodeServer struct {
	pb.UnimplementedHubNodeServer
	h *Hub
}

func (s *hubNodeServer) Renew(ctx context.Context, req *pb.RenewRequest) (*pb.RenewResponse, error) {
	return s.h.renew(callerFrom(ctx), req.CsrPem)
}

// Connect is a worker's session (see the proto).
func (s *hubNodeServer) Connect(stream pb.HubNode_ConnectServer) error {
	h := s.h
	c := callerFrom(stream.Context())
	first := make(chan *pb.NodeReport, 1)
	errc := make(chan error, 1)
	go func() {
		m, err := stream.Recv()
		if err != nil {
			errc <- err
			return
		}
		first <- m
	}()
	var hello *pb.NodeHello
	select {
	case m := <-first:
		if hello = m.GetHello(); hello == nil {
			return status.Error(codes.InvalidArgument, "the first message must be hello")
		}
	case err := <-errc:
		return err
	case <-time.After(30 * time.Second):
		return status.Error(codes.DeadlineExceeded, "no hello")
	}

	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	h.mu.Lock()
	n := h.nodes[c.id.Name]
	if n == nil || n.rec.State != stateApproved {
		h.mu.Unlock()
		return status.Errorf(codes.FailedPrecondition, "node %s is not approved", c.id.Name)
	}
	if n.sess != nil { // newest session wins (a reconnect raced the old one's timeout)
		n.sess.cancel()
		h.setOfflineLocked(n, n.sess.id)
	}
	h.sessionSeq++
	sess := &nodeSession{id: h.sessionSeq, out: make(chan *pb.HubCommand, 128), cancel: cancel}
	n.sess = sess
	now := time.Now()
	n.lastReport = now
	h.applyHelloLocked(n, hello)
	if n.rec.IssuedCert != "" && n.rec.CertSerial == pki.Serial(c.cert) {
		// The node collected its enrollment certificate: forget the pending data.
		n.rec.IssuedCert, n.rec.PollTokenHash = "", ""
	}
	n.rec.LastSeen, n.savedSeen = now.UTC(), now
	if err := h.store.PutNode(n.rec); err != nil {
		h.log.Warn("save node", "err", err)
	}
	welcome := &pb.HubWelcome{
		NodeState: n.rec.State, AuditOffset: n.rec.AuditOffset,
		MetricsInterval: durationpb.New(h.cfg.Heartbeat.Interval.Duration),
		RenewBefore:     durationpb.New(h.cfg.RenewBefore(n.rec.Role)),
		HubDraining:     n.rec.AdminDrain,
		Tailnet:         h.tailnetInfo(n),
	}
	revVersion := h.revVersion
	desired := h.desiredToolchainLocked(n)
	h.log.Info("node online", "node", n.rec.Name, "version", hello.Version, "endpoints", hello.Endpoints)
	h.publishNodeLocked(n, "online")
	h.advanceReleaseLocked()
	h.mu.Unlock()
	h.kickQueue()

	send := func(cmd *pb.HubCommand) { sess.out <- cmd } // fresh channel: cannot block yet
	send(&pb.HubCommand{Msg: &pb.HubCommand_Welcome{Welcome: welcome}})
	if hello.RevocationVersion != revVersion {
		send(h.revocationsMsg())
	}
	send(&pb.HubCommand{Msg: &pb.HubCommand_Toolchain{Toolchain: desired}})

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case cmd := <-sess.out:
				if err := stream.Send(cmd); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	nodeName := c.id.Name
	defer func() {
		h.mu.Lock()
		h.setOfflineLocked(n, sess.id)
		h.mu.Unlock()
	}()
	reports := make(chan *pb.NodeReport)
	go func() {
		defer close(reports)
		for {
			m, err := stream.Recv()
			if err != nil {
				if err != io.EOF && ctx.Err() == nil {
					h.log.Info("node session ended", "node", nodeName, "err", err)
				}
				cancel()
				return
			}
			select {
			case reports <- m:
			case <-ctx.Done():
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case m, ok := <-reports:
			if !ok {
				return nil
			}
			h.handleReport(n, sess, m)
		}
	}
}

func (h *Hub) applyHelloLocked(n *node, hello *pb.NodeHello) {
	r := n.rec
	r.Version, r.OS, r.Arch, r.Hostname = hello.Version, hello.Os, hello.Arch, hello.Hostname
	r.Endpoints, r.Mirrors, r.WarmRepos = hello.Endpoints, hello.Mirrors, hello.WarmRepos
	r.HelloLabels = hello.Labels
	if st := hello.State; st != nil {
		n.localDrain, n.localReason = st.Draining, st.Reason
		n.maxJobs, n.reservedGB = st.MaxJobs, st.ReservedMemGb
	}
	r.Channel, r.UpdateEnabled = hello.Channel, hello.UpdateEnabled
	h.applyTailnetLocked(n, hello.Tailnet)
	if hello.Update != nil {
		if b, err := protojson.Marshal(hello.Update); err == nil {
			r.Update = b
		}
	}
}

func (h *Hub) handleReport(n *node, sess *nodeSession, m *pb.NodeReport) {
	now := time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	if n.sess != sess { // replaced meanwhile
		return
	}
	n.lastReport = now
	if now.Sub(n.savedSeen) > 30*time.Second {
		n.rec.LastSeen, n.savedSeen = now.UTC(), now
		h.store.PutNode(n.rec)
	}
	switch x := m.Msg.(type) {
	case *pb.NodeReport_Metrics:
		n.metrics = x.Metrics
		recordMetricsLocked(n, x.Metrics)
		h.bus.publish(&pb.FleetEvent{Kind: "metrics", Node: n.rec.Name, NodeInfo: h.nodeInfoLocked(n)})
		h.kickQueue()
	case *pb.NodeReport_Jobs:
		h.applyJobsLocked(n, x.Jobs)
	case *pb.NodeReport_Audit:
		h.storeAuditLocked(n, sess, x.Audit)
	case *pb.NodeReport_Toolchain:
		n.toolchain = x.Toolchain
		if b, err := protojson.Marshal(x.Toolchain); err == nil {
			n.rec.Toolchain = b
			h.store.PutNode(n.rec)
		}
		h.publishNodeLocked(n, "toolchain "+x.Toolchain.State)
		h.advanceRolloutsLocked()
	case *pb.NodeReport_State:
		n.localDrain, n.localReason = x.State.Draining, x.State.Reason
		n.maxJobs, n.reservedGB = x.State.MaxJobs, x.State.ReservedMemGb
		h.publishNodeLocked(n, "state changed")
		h.kickQueue()
	case *pb.NodeReport_Update:
		if b, err := protojson.Marshal(x.Update); err == nil {
			n.rec.Update = b
			h.store.PutNode(n.rec)
		}
		h.log.Info("node update", "node", n.rec.Name, "state", x.Update.State, "from", x.Update.From, "to", x.Update.To, "err", x.Update.Error)
		h.publishNodeLocked(n, "update "+x.Update.State+" "+x.Update.To)
		h.advanceReleaseLocked()
	case *pb.NodeReport_Reply:
		h.deliverReplyLocked(n, sess, x.Reply)
	case *pb.NodeReport_Hello:
		h.applyHelloLocked(n, x.Hello)
		h.store.PutNode(n.rec)
		h.publishNodeLocked(n, "hello")
	}
}

// applyJobsLocked merges a jobs report into the index (worker = source of
// truth) and publishes changes.
func (h *Hub) applyJobsLocked(n *node, rep *pb.JobsReport) {
	worker := n.rec.Name
	jm := h.jobMap(worker)
	now := time.Now()
	seen := map[string]bool{}
	changed := false
	for _, j := range rep.Jobs {
		id := j.GetSpec().GetId()
		if id == "" {
			continue
		}
		seen[id] = true
		if r := h.reservations[id]; r != nil { // the placement turned into a job
			delete(h.reservations, id)
		}
		if old := jm[id]; old != nil && proto.Equal(old.job, j) {
			continue
		}
		jm[id] = &jobEntry{job: j, updated: now}
		changed = true
		if !rep.Full {
			if b, err := proto.Marshal(j); err == nil {
				h.store.PutJob(store.JobRow{Worker: worker, ID: id, State: int32(j.State), UpdatedAt: now, Data: b})
			}
		}
		h.bus.publish(&pb.FleetEvent{Kind: "job", Node: worker, Job: h.fleetJobLocked(worker, jm[id])})
	}
	removed := rep.Removed
	if rep.Full {
		for id := range jm {
			if !seen[id] {
				removed = append(removed, id)
			}
		}
	}
	for _, id := range removed {
		if _, ok := jm[id]; ok {
			delete(jm, id)
			changed = true
			if !rep.Full {
				h.store.DeleteJob(worker, id)
			}
		}
	}
	if rep.Full {
		rows := make([]store.JobRow, 0, len(jm))
		for id, e := range jm {
			if b, err := proto.Marshal(e.job); err == nil {
				rows = append(rows, store.JobRow{Worker: worker, ID: id, State: int32(e.job.State), UpdatedAt: e.updated, Data: b})
			}
		}
		if err := h.store.ReplaceWorkerJobs(worker, rows); err != nil {
			h.log.Warn("save jobs", "err", err)
		}
	}
	if changed {
		h.kickQueue()
	}
}

func (h *Hub) storeAuditLocked(n *node, sess *nodeSession, b *pb.AuditBatch) {
	rows := make([]store.AuditRow, 0, len(b.Lines))
	for _, l := range b.Lines {
		var rec struct {
			Time   string `json:"time"`
			Who    string `json:"who"`
			Method string `json:"method"`
		}
		json.Unmarshal([]byte(l), &rec)
		t, err := time.Parse(time.RFC3339Nano, rec.Time)
		if err != nil {
			t = time.Now()
		}
		rows = append(rows, store.AuditRow{Node: n.rec.Name, Time: t, Who: rec.Who, Method: rec.Method, Line: l})
	}
	if err := h.store.AddAudit(rows); err != nil {
		h.log.Warn("store audit", "node", n.rec.Name, "err", err)
		return
	}
	n.rec.AuditOffset = b.NextOffset
	h.store.PutNode(n.rec)
	h.sendLocked(n, &pb.HubCommand{Msg: &pb.HubCommand_AuditAck{AuditAck: &pb.AuditAck{Offset: b.NextOffset}}})
}

// setOfflineLocked ends session id of n (if it is still the current one).
func (h *Hub) setOfflineLocked(n *node, id uint64) {
	if n.sess == nil || n.sess.id != id {
		return
	}
	n.sess = nil
	h.failPendingLocked(n.rec.Name, id)
	n.rec.LastSeen = time.Now().UTC()
	h.store.PutNode(n.rec)
	h.log.Info("node offline", "node", n.rec.Name)
	h.publishNodeLocked(n, "offline")
}

// sendLocked queues a command for n's session; a session that does not
// keep up is closed (the worker reconnects and gets a fresh state).
func (h *Hub) sendLocked(n *node, cmd *pb.HubCommand) {
	if n.sess == nil {
		return
	}
	select {
	case n.sess.out <- cmd:
	default:
		h.log.Warn("node session backlog full; closing it", "node", n.rec.Name)
		n.sess.cancel()
	}
}

// broadcast sends cmd to every connected worker.
func (h *Hub) broadcast(cmd *pb.HubCommand) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, n := range h.nodes {
		h.sendLocked(n, cmd)
	}
}

func (h *Hub) publishNodeLocked(n *node, msg string) {
	h.bus.publish(&pb.FleetEvent{Kind: "node", Node: n.rec.Name, NodeInfo: h.nodeInfoLocked(n), Message: msg})
}

// labelsLocked are the node's effective labels: longarmd.toml's, the admin's
// (join code / SetLabels, which win) and os/arch.
func labelsOf(n *node) map[string]string {
	m := map[string]string{}
	maps.Copy(m, n.rec.HelloLabels)
	maps.Copy(m, n.rec.Labels)
	if n.rec.OS != "" {
		m["os"] = n.rec.OS
	}
	if n.rec.Arch != "" {
		m["arch"] = n.rec.Arch
	}
	return m
}

func (h *Hub) nodeInfoLocked(n *node) *pb.NodeInfo {
	r := n.rec
	info := &pb.NodeInfo{
		Name: r.Name, Role: r.Role, State: r.State, Online: n.sess != nil,
		Draining: r.AdminDrain || n.localDrain, AdminDrain: r.AdminDrain, Labels: labelsOf(n),
		Os: r.OS, Arch: r.Arch, Version: r.Version, Hostname: r.Hostname, Endpoints: r.Endpoints,
		Metrics: n.metrics, LastSeen: tspb(r.LastSeen), JoinedAt: tspb(r.JoinedAt), CertNotAfter: tspb(r.CertNotAfter),
		Mirrors: r.Mirrors, WarmRepos: r.WarmRepos, Profile: r.Profile, Toolchain: n.toolchain,
		MaxJobs: n.maxJobs, ReservedMemGb: n.reservedGB, Tailnet: n.tailnet,
	}
	switch {
	case r.AdminDrain:
		info.DrainReason = "drained by an admin"
	case n.localDrain:
		info.DrainReason = n.localReason
	}
	if n.sess != nil {
		info.LastSeen = tspb(n.lastReport)
	}
	info.JobsActive, info.ReservedGb = h.loadLocked(r.Name)
	return info
}

// loadLocked returns the active jobs and reserved memory of a worker.
func (h *Hub) loadLocked(worker string) (int32, float64) {
	var active int32
	var gb float64
	for _, e := range h.jobs[worker] {
		if terminal(e.job.State) {
			continue
		}
		active++
		gb += jobMemGB(e.job, h.params)
	}
	for _, r := range h.reservations {
		if r.worker == worker {
			gb += r.memGB
		}
	}
	return active, gb
}

func jobMemGB(j *pb.Job, p sched.Params) float64 {
	labels := j.GetSpec().GetLabels()
	if v, err := strconv.ParseFloat(labels[sched.LabelMemGB], 64); err == nil && v > 0 {
		return v
	}
	typ := labels[sched.LabelType]
	if typ == "" {
		typ = sched.TypeOf(j.GetSpec().GetArgv())
	}
	return p.MemFor(typ, 0)
}

func terminal(s pb.JobState) bool {
	switch s {
	case pb.JobState_JOB_STATE_DONE, pb.JobState_JOB_STATE_FAILED, pb.JobState_JOB_STATE_KILLED, pb.JobState_JOB_STATE_LOST:
		return true
	}
	return false
}

func (h *Hub) fleetJobLocked(worker string, e *jobEntry) *pb.FleetJob {
	n := h.nodes[worker]
	return &pb.FleetJob{Worker: worker, Job: e.job, UpdatedAt: tspb(e.updated), WorkerOnline: n != nil && n.sess != nil}
}

// findJobLocked finds a job by id (on worker, or anywhere).
func (h *Hub) findJobLocked(id, worker string) (string, *jobEntry) {
	if worker != "" {
		if e := h.jobs[worker][id]; e != nil {
			return worker, e
		}
		return "", nil
	}
	workers := make([]string, 0, len(h.jobs))
	for w := range h.jobs {
		workers = append(workers, w)
	}
	sort.Strings(workers)
	for _, w := range workers {
		if e := h.jobs[w][id]; e != nil {
			return w, e
		}
	}
	return "", nil
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// Leave deregisters the calling node (longarmd leave / longarm leave / uninstall):
// it is revoked like an admin revocation, so its certificates stop working
// everywhere at once.
func (s *hubNodeServer) Leave(ctx context.Context, req *pb.LeaveRequest) (*pb.LeaveResponse, error) {
	c := callerFrom(ctx)
	who := "self/" + c.id.Name
	if r := strings.TrimSpace(req.Reason); r != "" {
		if len(r) > 200 {
			r = r[:200]
		}
		who += " (" + r + ")"
	}
	// Revoking closes the caller's connections, this one included: answer
	// first, revoke right after.
	time.AfterFunc(300*time.Millisecond, func() {
		if _, err := s.h.Revoke(who, c.id.Name); err != nil {
			s.h.log.Warn("leave", "node", c.id.Name, "err", err)
			return
		}
		s.h.log.Info("node left", "node", c.id.Name, "role", c.id.Role)
	})
	return &pb.LeaveResponse{Name: c.id.Name, Message: "node " + c.id.Name + " left " + s.h.cfg.Name + "; its certificates are revoked"}, nil
}
