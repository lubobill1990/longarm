package longarmd

import (
	"bufio"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/metrics"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/server"
	"github.com/lubobill1990/longarm/internal/supervisor"
	"github.com/lubobill1990/longarm/internal/tenancy"
	"github.com/lubobill1990/longarm/internal/toolchain"
	"github.com/lubobill1990/longarm/internal/userspace"
	"github.com/lubobill1990/longarm/internal/version"
	"github.com/lubobill1990/longarm/internal/workspace"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// hubAgent is longarmd's session with longarm-hub (DESIGN §3.3): hello, metrics every
// 2 s, job changes, audit lines, toolchain state; it applies revocation
// lists and toolchain profiles, renews the certificate before it expires,
// and reconnects with backoff. Nothing here is on the data plane: the hub
// being away changes nothing for clients talking to this worker.
type hubAgent struct {
	// welcomes counts sessions the hub accepted (Run's reconnect backoff
	// starts over after one).
	welcomes  atomic.Int64
	addr      string
	dataDir   string
	reloader  *pki.Reloader
	live      *pki.Bundle
	spaces    *userspace.Spaces
	srv       *server.Server
	policy    *tenancy.Policy
	sampler   *metrics.Sampler
	revoked   *pki.RevocationList
	endpoints func() []string
	labels    map[string]string
	tc        *toolchain.Reconciler
	upd       *updater // nil when self-update is off
	tailnet   *tailnetLegs
	channel   string
	connected atomic.Bool // a session reached the welcome at least once
	log       *slog.Logger
	audit     *server.Audit
	// remoteJobs: what hub requests may do (see HubConfig.RemoteJobs).
	remoteJobs string
	// hideJobDetails: [access] hub_job_details = false (hubredact.go).
	hideJobDetails bool
	stop           context.CancelFunc // stops longarmd (after longarmd leave)
	logMaxBytes    int64

	mu          sync.Mutex
	link        *pb.HubLink
	out         chan *pb.NodeReport // current session's queue (nil = offline)
	localDrain  drainState          // longarmd drain (persisted)
	limits      *pb.LocalLimits     // longarmd limits (persisted)
	limitsMu    sync.Mutex          // serializes SetLimits (file + memory)
	toolDrain   drainState          // toolchain update
	adminDrain  bool
	renewBefore time.Duration
	watchers    map[chan *pb.LocalEvent]struct{}
}

type drainState struct {
	On     bool   `json:"draining"`
	Reason string `json:"reason,omitempty"`
}

// DrainFile persists `longarmd drain` across restarts.
const DrainFile = "drain.json"

func newHubAgent(addr string, a *hubAgent) *hubAgent {
	a.addr = addr
	a.link = &pb.HubLink{Address: addr}
	a.watchers = map[chan *pb.LocalEvent]struct{}{}
	if b, err := os.ReadFile(filepath.Join(a.dataDir, DrainFile)); err == nil {
		json.Unmarshal(b, &a.localDrain)
	}
	a.limits = loadLimits(a.dataDir)
	return a
}

func (a *hubAgent) Link() *pb.HubLink {
	a.mu.Lock()
	defer a.mu.Unlock()
	return proto.Clone(a.link).(*pb.HubLink)
}

func (a *hubAgent) setLink(connected bool, err error, nodeState string) {
	a.mu.Lock()
	changed := a.link.Connected != connected
	if changed || a.link.Since == nil {
		a.link.Since = timestamppb.Now()
	}
	a.link.Connected = connected
	if err != nil {
		a.link.Error = err.Error()
	} else if connected {
		a.link.Error = ""
	}
	if nodeState != "" {
		a.link.NodeState = nodeState
	}
	a.mu.Unlock()
	if changed {
		msg := "hub disconnected"
		if connected {
			msg = "hub connected"
		}
		a.notify(&pb.LocalEvent{Kind: "hub", Message: msg})
	}
}

// State is the worker's effective drain state.
func (a *hubAgent) State() *pb.NodeState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stateLocked()
}

func (a *hubAgent) stateLocked() *pb.NodeState {
	st := &pb.NodeState{}
	switch {
	case a.localDrain.On:
		st.Draining, st.Reason = true, nonEmpty(a.localDrain.Reason, "drained locally")
	case a.toolDrain.On:
		st.Draining, st.Reason = true, a.toolDrain.Reason
	}
	if a.limits != nil {
		st.MaxJobs, st.ReservedMemGb = a.limits.MaxJobs, a.limits.ReservedMemGb
	}
	return st
}

func nonEmpty(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// SetLocalDrain implements `longarmd drain` / `longarmd undrain`.
func (a *hubAgent) SetLocalDrain(on bool, reason string) error {
	a.mu.Lock()
	a.localDrain = drainState{On: on, Reason: reason}
	b, _ := json.Marshal(a.localDrain)
	st := a.stateLocked()
	a.mu.Unlock()
	if err := os.WriteFile(filepath.Join(a.dataDir, DrainFile), b, 0o600); err != nil {
		return err
	}
	a.send(&pb.NodeReport{Msg: &pb.NodeReport_State{State: st}}, false)
	a.notify(&pb.LocalEvent{Kind: "drain", Message: map[bool]string{true: "draining: " + reason, false: "accepting work"}[on]})
	return nil
}

// setToolDrain is the toolchain reconciler's drain hook.
func (a *hubAgent) setToolDrain(on bool, reason string) {
	a.mu.Lock()
	a.toolDrain = drainState{On: on, Reason: reason}
	st := a.stateLocked()
	a.mu.Unlock()
	a.send(&pb.NodeReport{Msg: &pb.NodeReport_State{State: st}}, false)
}

// send queues a report for the current session. Lossy reports (metrics)
// are dropped when the queue is full; others wait briefly.
func (a *hubAgent) send(m *pb.NodeReport, lossy bool) {
	a.mu.Lock()
	out := a.out
	a.mu.Unlock()
	if out == nil {
		return
	}
	if lossy {
		select {
		case out <- m:
		default:
		}
		return
	}
	select {
	case out <- m:
	case <-time.After(5 * time.Second):
		a.log.Warn("hub: report queue stuck; dropping a report")
	}
}

func (a *hubAgent) notify(ev *pb.LocalEvent) {
	ev.Time = timestamppb.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	for ch := range a.watchers {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (a *hubAgent) watch() (chan *pb.LocalEvent, func()) {
	ch := make(chan *pb.LocalEvent, 64)
	a.mu.Lock()
	a.watchers[ch] = struct{}{}
	a.mu.Unlock()
	return ch, func() {
		a.mu.Lock()
		delete(a.watchers, ch)
		a.mu.Unlock()
	}
}

// Run keeps a session with the hub until ctx is done.
func (a *hubAgent) Run(ctx context.Context) {
	wait := time.Second
	for ctx.Err() == nil {
		start, welcomed := time.Now(), a.welcomes.Load()
		err := a.session(ctx)
		if ctx.Err() != nil {
			break
		}
		a.setLink(false, err, "")
		// The backoff is for a hub that can't be reached or refuses us; a
		// session the hub accepted and kept for a while (it restarted, the
		// connection broke) reconnects after the first delay again. (It
		// only reset after 30 s before, so a hub restarted a few times in a
		// row was waited for 8, 16, 30 s.) One dropped right after the
		// welcome keeps backing off.
		if a.welcomes.Load() != welcomed && time.Since(start) > 2*time.Second || time.Since(start) > 30*time.Second {
			wait = time.Second
		}
		a.log.Info("hub: session ended; reconnecting", "err", err, "in", wait)
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
		if wait *= 2; wait > 30*time.Second {
			wait = 30 * time.Second
		}
	}
	a.setLink(false, nil, "")
}

func (a *hubAgent) hello() *pb.NodeHello {
	host, _ := os.Hostname()
	h := &pb.NodeHello{Version: version.Version, Os: runtime.GOOS, Arch: runtime.GOARCH, Hostname: host,
		NumCpu: int32(runtime.NumCPU()), Endpoints: a.endpoints(), Labels: a.labels, State: a.State(),
		RevocationVersion: a.revoked.Version()}
	h.Channel = a.channel
	if a.upd != nil {
		h.UpdateEnabled = a.upd.enabled()
		h.Update = a.upd.Status()
	}
	h.Mirrors, h.WarmRepos = a.mirrors()
	h.Tailnet = a.tailnet.state()
	h.Isolation = a.spaces.Mode()
	h.TenancyVersion = a.policy.Version()
	return h
}

// mirrors lists repo keys with a mirror and those used in the last day.
// Per-user isolation: users' mirrors are "<user>/<repo>" (the scheduler's
// affinity is per user: nobody else's mirror helps).
func (a *hubAgent) mirrors() (all, warm []string) {
	for _, sp := range a.spaces.All() {
		prefix := ""
		if sp.Owner != "" {
			prefix = sp.Owner + "/"
		} else if a.spaces.PerUser() {
			continue // legacy data: used by nobody any more
		}
		ents, _ := os.ReadDir(filepath.Join(sp.Root, workspace.MirrorsDir))
		for _, e := range ents {
			repo, ok := strings.CutSuffix(e.Name(), ".git")
			if !e.IsDir() || !ok {
				continue
			}
			all = append(all, prefix+repo)
			if time.Since(sp.WS.MirrorLastUsed(repo)) < 24*time.Hour {
				warm = append(warm, prefix+repo)
			}
		}
	}
	return all, warm
}

// fullJobLimit caps the terminal jobs of a full report (active jobs are
// always included).
const fullJobLimit = 500

func comparable(j *pb.Job) *pb.Job {
	c := proto.Clone(j).(*pb.Job)
	c.LogSize = 0 // grows constantly; not worth an update on its own
	return c
}

func (a *hubAgent) session(ctx context.Context) error {
	conn, err := hubclient.Dial(a.addr, a.live)
	if err != nil {
		return err
	}
	defer conn.Close()
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	st, err := conn.Node().Connect(sctx)
	if err != nil {
		return err
	}
	hello := a.hello()
	if err := st.Send(&pb.NodeReport{Msg: &pb.NodeReport_Hello{Hello: hello}}); err != nil {
		return err
	}
	first, err := st.Recv()
	if err != nil {
		return err
	}
	w := first.GetWelcome()
	if w == nil {
		return errors.New("hub: no welcome")
	}
	interval := w.MetricsInterval.AsDuration()
	if interval <= 0 {
		interval = 2 * time.Second
	}
	a.mu.Lock()
	a.renewBefore, a.adminDrain = w.RenewBefore.AsDuration(), w.HubDraining
	out := make(chan *pb.NodeReport, 256)
	a.out = out
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.out = nil
		a.mu.Unlock()
	}()
	a.setLink(true, nil, w.NodeState)
	a.welcomes.Add(1)
	a.connected.Store(true)
	if a.upd != nil {
		a.upd.reported(hello.Update)
		a.upd.trigger()
	}
	a.log.Info("hub: connected", "hub", a.addr, "state", w.NodeState)
	a.tailnet.apply(w.Tailnet)

	errc := make(chan error, 2)
	go func() {
		for {
			select {
			case <-sctx.Done():
				return
			case m := <-out:
				if err := st.Send(m); err != nil {
					errc <- err
					return
				}
			}
		}
	}()
	go func() {
		for {
			cmd, err := st.Recv()
			if err != nil {
				errc <- err
				return
			}
			a.handle(cmd)
		}
	}()

	// Full job report first (the hub rebuilds its index from it).
	sent := map[string]*pb.Job{}
	full := &pb.JobsReport{Full: true}
	terminal := 0
	for _, j := range a.spaces.ListJobs(nil, 0) {
		p := a.jobForHub(j)
		if j.Status.State.Terminal() {
			if terminal++; terminal > fullJobLimit {
				continue
			}
		}
		full.Jobs = append(full.Jobs, p)
		sent[p.Spec.Id] = comparable(p)
	}
	a.send(&pb.NodeReport{Msg: &pb.NodeReport_Jobs{Jobs: full}}, false)
	if a.tc != nil {
		a.send(&pb.NodeReport{Msg: &pb.NodeReport_Toolchain{Toolchain: a.tc.Status()}}, false)
	}
	if m, err := a.sampler.Latest(sctx); err == nil {
		a.send(&pb.NodeReport{Msg: &pb.NodeReport_Metrics{Metrics: a.metricsForHub(m)}}, true)
	}

	auditOff := w.AuditOffset
	metricsT := time.NewTicker(interval)
	defer metricsT.Stop()
	slowT := time.NewTicker(2 * time.Second) // audit tail, job resync, renewal
	defer slowT.Stop()
	a.maybeRenew(sctx, conn)
	for {
		changed := a.spaces.Changed()
		select {
		case <-sctx.Done():
			return ctx.Err()
		case err := <-errc:
			return err
		case <-changed:
			a.reportJobs(sent, false)
		case <-metricsT.C:
			if m, err := a.sampler.Latest(sctx); err == nil {
				a.send(&pb.NodeReport{Msg: &pb.NodeReport_Metrics{Metrics: a.metricsForHub(m)}}, true)
			}
		case <-slowT.C:
			a.reportJobs(sent, true)
			auditOff = a.forwardAudit(auditOff)
			a.maybeRenew(sctx, conn)
		}
	}
}

// jobForHub is a job as reported to the hub.
func (a *hubAgent) jobForHub(j supervisor.Job) *pb.Job {
	p := server.JobToPB(j)
	if a.hideJobDetails {
		return redactJob(p)
	}
	return p
}

// metricsForHub is a metrics snapshot as reported to the hub.
func (a *hubAgent) metricsForHub(m metrics.Snapshot) *pb.MetricsSnapshot {
	p := server.SnapshotToPB(m)
	if a.hideJobDetails {
		return redactMetrics(p)
	}
	return p
}

// reportJobs sends the jobs that changed since the last report (and ids
// that disappeared, e.g. by GC).
func (a *hubAgent) reportJobs(sent map[string]*pb.Job, includeRemoved bool) {
	rep := &pb.JobsReport{}
	seen := map[string]bool{}
	for _, j := range a.spaces.ListJobs(nil, 0) {
		p := a.jobForHub(j)
		id := p.Spec.Id
		seen[id] = true
		c := comparable(p)
		if old, ok := sent[id]; ok && proto.Equal(old, c) {
			continue
		}
		if _, ok := sent[id]; !ok && j.Status.State.Terminal() && len(sent) > fullJobLimit*2 {
			continue // old terminal job never reported: not interesting
		}
		sent[id] = c
		rep.Jobs = append(rep.Jobs, p)
	}
	if includeRemoved {
		for id := range sent {
			if !seen[id] {
				rep.Removed = append(rep.Removed, id)
				delete(sent, id)
			}
		}
	}
	if len(rep.Jobs) > 0 || len(rep.Removed) > 0 {
		a.send(&pb.NodeReport{Msg: &pb.NodeReport_Jobs{Jobs: rep}}, false)
	}
}

// forwardAudit sends complete audit.log lines from off (at most 256 KiB per
// batch) and returns the new offset.
func (a *hubAgent) forwardAudit(off int64) int64 {
	f, err := os.Open(filepath.Join(a.dataDir, AuditFile))
	if err != nil {
		return off
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return off
	}
	if off > fi.Size() { // audit.log was replaced: start over
		off = 0
	}
	if off == fi.Size() {
		return off
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return off
	}
	br := bufio.NewReaderSize(io.LimitReader(f, 256<<10), 64<<10)
	var lines []string
	next := off
	for {
		line, err := br.ReadString('\n')
		if err != nil { // incomplete last line: next time
			break
		}
		next += int64(len(line))
		if l := strings.TrimSpace(line); l != "" {
			if a.hideJobDetails {
				l = redactAuditLine(l)
			}
			lines = append(lines, l)
		}
	}
	if next == off {
		// No newline within the batch limit: a line longer than any record
		// longarmd writes now (older audit.log). Skip it so forwarding goes on.
		f.Seek(off, io.SeekStart)
		r := bufio.NewReader(f)
		skipped := int64(0)
		for {
			chunk, err := r.ReadSlice('\n')
			skipped += int64(len(chunk))
			if err == nil {
				break
			}
			if err != bufio.ErrBufferFull {
				return off // incomplete: wait for the newline
			}
		}
		next = off + skipped
		b, _ := json.Marshal(map[string]any{"time": time.Now().UTC().Format(time.RFC3339Nano), "who": "longarmd",
			"method": "audit", "skipped_bytes": skipped})
		lines = append(lines, string(b))
	}
	a.send(&pb.NodeReport{Msg: &pb.NodeReport_Audit{Audit: &pb.AuditBatch{Lines: lines, NextOffset: next}}}, false)
	return next
}

func (a *hubAgent) handle(cmd *pb.HubCommand) {
	switch x := cmd.Msg.(type) {
	case *pb.HubCommand_Revocations:
		m := map[string]time.Time{}
		for _, c := range x.Revocations.Certs {
			m[c.Serial] = c.NotAfter.AsTime()
		}
		if err := a.revoked.Replace(x.Revocations.Version, m); err != nil {
			a.log.Warn("hub: save revocation list", "err", err)
		}
		a.log.Info("hub: revocation list updated", "version", x.Revocations.Version, "revoked", a.revoked.Len())
		a.notify(&pb.LocalEvent{Kind: "hub", Message: "revocation list updated"})
	case *pb.HubCommand_Toolchain:
		if a.tc != nil {
			a.tc.SetDesired(x.Toolchain)
		}
	case *pb.HubCommand_Drain:
		a.mu.Lock()
		a.adminDrain = x.Drain.Draining
		a.mu.Unlock()
		a.notify(&pb.LocalEvent{Kind: "drain", Message: map[bool]string{true: "drained by an admin", false: "admin drain lifted"}[x.Drain.Draining]})
	case *pb.HubCommand_Update:
		if a.upd != nil {
			a.log.Info("hub: update available", "version", x.Update.Version)
			a.upd.trigger()
		}
	case *pb.HubCommand_Tailnet:
		a.tailnet.apply(x.Tailnet)
	case *pb.HubCommand_Tenancy:
		a.applyTenancy(x.Tenancy)
	case *pb.HubCommand_AuditAck:
	case *pb.HubCommand_KillJob, *pb.HubCommand_LogRequest, *pb.HubCommand_SubmitJob, *pb.HubCommand_ToolchainApply:
		go a.hubRequest(cmd) // may take a while (kill waits for the tree); never block the session
	}
}

// everConnected reports whether a hub session got as far as the welcome.
func (a *hubAgent) everConnected() bool { return a.connected.Load() }

// maybeRenew renews the certificate when it expires within renew_before.
func (a *hubAgent) maybeRenew(ctx context.Context, conn *hubclient.Conn) {
	a.mu.Lock()
	rb := a.renewBefore
	a.mu.Unlock()
	leaf := a.live.Leaf()
	if !hubclient.NeedsRenewal(leaf, rb) {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	certPEM, keyPEM, caPEM, err := hubclient.Renew(rctx, conn, a.live.Identity.Name, nil)
	if err != nil {
		a.log.Warn("hub: certificate renewal failed", "err", err, "not_after", leaf.NotAfter)
		return
	}
	if err := pki.WriteBundleAtomic(filepath.Join(a.dataDir, TLSDir), caPEM, certPEM, keyPEM); err != nil {
		a.log.Warn("hub: write renewed certificate", "err", err)
		return
	}
	if _, err := a.reloader.Check(); err != nil {
		a.log.Warn("hub: load renewed certificate", "err", err)
		return
	}
	a.log.Info("hub: certificate renewed", "not_after", a.live.Leaf().NotAfter)
	a.notify(&pb.LocalEvent{Kind: "hub", Message: "certificate renewed"})
}

// clientEndpoints turns longarmd's listener names into endpoints for clients
// (no ssh listeners; loopback only when nothing else exists).
func clientEndpoints(listeners []string) []string {
	var out, loop []string
	for _, l := range listeners {
		if strings.HasPrefix(l, "ssh+") {
			continue
		}
		if strings.HasPrefix(l, "longnet://") {
			out = append(out, l)
			continue
		}
		u, err := url.Parse(l)
		if err == nil && (u.Hostname() == "0.0.0.0" || u.Hostname() == "::") {
			continue // not dialable: set [hub] advertise
		}
		if err == nil && (strings.HasPrefix(u.Hostname(), "127.") || u.Hostname() == "::1" || u.Hostname() == "localhost") {
			loop = append(loop, l)
			continue
		}
		out = append(out, l)
	}
	return append(out, loop...)
}

// watchJobs turns job state changes into local events (LocalWorker.Events),
// with or without a hub.
func (a *hubAgent) watchJobs(ctx context.Context) {
	last := map[string]pb.JobState{}
	first := true
	for {
		changed := a.spaces.Changed()
		for _, j := range a.spaces.ListJobs(nil, 200) {
			p := server.JobToPB(j)
			if old, ok := last[p.Spec.Id]; !first && (!ok || old != p.State) {
				a.notify(&pb.LocalEvent{Kind: "job", Job: p})
			}
			last[p.Spec.Id] = p.State
		}
		first = false
		select {
		case <-ctx.Done():
			return
		case <-changed:
		}
	}
}

// applyTenancy stores the hub's tenancy policy (users, teams, quotas) and
// purges the data of removed users.
func (a *hubAgent) applyTenancy(p *pb.TenancyPolicy) {
	old := a.policy.Version()
	if err := a.policy.Set(p); err != nil {
		a.log.Warn("hub: save tenancy policy", "err", err)
	}
	a.log.Info("hub: tenancy policy", "version", p.Version, "enabled", p.Enabled, "users", len(p.Users), "purge", p.Purge)
	// Disabled and purged users lose what they still have open (attached
	// sessions, followed logs) - their next calls are refused anyway.
	if n := a.srv.Conns.CloseIf(func(c *x509.Certificate) bool {
		id, err := pki.IdentityFromCert(c)
		return err == nil && (id.Role == pki.RoleClient || id.Role == pki.RoleAdmin) && a.policy.Disabled(id.Owner())
	}); n > 0 {
		a.log.Info("closed connections of disabled users", "count", n)
	}
	if p.Version == old || len(p.Purge) == 0 {
		return
	}
	go func() {
		for _, u := range p.Purge {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			err := a.spaces.Purge(ctx, u)
			cancel()
			a.audit.LogAs("hub", "tenancy/purge", map[string]any{"user": u, "error": errText(err)})
			if err != nil {
				a.log.Warn("purge user", "user", u, "err", err)
			}
		}
	}()
}
