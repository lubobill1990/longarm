package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/sched"
	"github.com/lubobill1990/longarm/internal/telemetry"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// The hub is optional everywhere: with a hub (hub.toml / [hub] in
// workers.toml) longarm asks it first - node list, job index, placement - and
// falls back to talking to workers directly when it is unreachable
// (DESIGN §3.3: degrade, never stop). Workers learned from the hub are
// cached in <config>/hub-cache.json for that case; manual workers.toml
// entries always win.

const hubCacheFile = "hub-cache.json"

// hubTimeout bounds a single hub call when the hub is optional.
var hubTimeout = 5 * time.Second

// hubConnectTimeout bounds connecting to the hub when it is optional: a hub
// that isn't there must not hold up commands that can do without it
// (LONGARM_HUB_TIMEOUT overrides).
var hubConnectTimeout = func() time.Duration {
	if v, err := time.ParseDuration(os.Getenv("LONGARM_HUB_TIMEOUT")); err == nil && v > 0 {
		return v
	}
	return 1500 * time.Millisecond
}()

// hubStateFile remembers across longarm invocations that the hub was
// unreachable, so the next commands within hubRetryAfter skip it at once.
const hubStateFile = "hub-state.json"

type hubState struct {
	Address string    `json:"address"`
	Down    time.Time `json:"down"`
	Error   string    `json:"error"`
}

type hubCache struct {
	Updated time.Time         `json:"updated"`
	Nodes   []json.RawMessage `json:"nodes"`
}

// hub returns the hub connection, or nil when no hub is configured.
func (s *session) hub() *hubclient.Conn {
	s.hubMu.Lock()
	defer s.hubMu.Unlock()
	if s.hubConn != nil || !s.cfg.Hub.Enabled() || noHub {
		return s.hubConn
	}
	c, err := hubclient.Dial(s.cfg.Hub.Address, s.dialer.Bundle())
	if err != nil {
		s.hubErr = err
		return nil
	}
	s.hubConn = c
	return c
}

// hubDown remembers that the hub failed: in this process, and for the
// next longarm invocations (hub-state.json) until hubRetryAfter passed.
func (s *session) hubDown(err error) {
	s.loadHubState()
	s.hubMu.Lock()
	defer s.hubMu.Unlock()
	if s.hubErr == nil || time.Since(s.hubErrAt) > hubRetryAfter {
		s.hubErr, s.hubErrAt = err, time.Now()
		b, _ := json.Marshal(hubState{Address: s.cfg.Hub.Address, Down: s.hubErrAt, Error: oneLine(err)})
		tmp := filepath.Join(s.cfg.Dir, fmt.Sprintf("%s.%d.tmp", hubStateFile, os.Getpid()))
		if os.WriteFile(tmp, b, 0o600) == nil && platform.ReplaceFile(tmp, filepath.Join(s.cfg.Dir, hubStateFile)) != nil {
			os.Remove(tmp)
		}
		if !quietHub {
			fmt.Fprintf(os.Stderr, "longarm: hub %s unreachable (%s); talking to workers directly\n", s.cfg.Hub.Address, oneLine(err))
		}
		telemetry.Event(context.Background(), "hub.unreachable", map[string]string{"error": client.ErrClass(err.Error()),
			"grpc_code": status.Code(err).String()}, nil)
	}
}

// hubUp clears a remembered hub failure.
func (s *session) hubUp() {
	s.hubMu.Lock()
	had := s.hubErr != nil
	s.hubErr = nil
	s.hubMu.Unlock()
	if had || s.hubStateSeen {
		os.Remove(filepath.Join(s.cfg.Dir, hubStateFile))
	}
}

// loadHubState applies a hub failure another longarm process recorded less than
// hubRetryAfter ago (once per process).
func (s *session) loadHubState() {
	s.hubStateOnce.Do(func() {
		b, err := os.ReadFile(filepath.Join(s.cfg.Dir, hubStateFile))
		if err != nil {
			return
		}
		s.hubStateSeen = true
		var st hubState
		if json.Unmarshal(b, &st) != nil || st.Address != s.cfg.Hub.Address {
			return
		}
		age := time.Since(st.Down)
		if age < 0 || age >= hubRetryAfter {
			return
		}
		s.hubMu.Lock()
		if s.hubErr == nil {
			s.hubErr, s.hubErrAt = errors.New(st.Error), st.Down
		}
		s.hubMu.Unlock()
		s.hubStateNote = fmt.Sprintf("longarm: hub %s was unreachable %s ago (%s); talking to workers directly (next try in %s)",
			s.cfg.Hub.Address, age.Round(time.Second), st.Error, (hubRetryAfter - age).Round(time.Second))
	})
}

// skipLeg skips longnet legs whose relay is the hub's built-in one while the
// hub is known to be down: they can't connect (signaling goes through the
// relay) and would only hold up the race until their dial timeout.
func (s *session) skipLeg(e client.Endpoint) string {
	if e.Transport != "longnet" || !s.cfg.Hub.Enabled() || noHub {
		return ""
	}
	if hubclient.NormalizeAddr(e.Address) != hubclient.NormalizeAddr(s.cfg.Hub.Address) || !s.hubFailed() {
		return ""
	}
	return "the hub's relay is down"
}

// optionalHub is the hub connection for callers that can do without it:
// nil when there is no hub, it failed recently, or it doesn't connect within
// hubConnectTimeout (which is then remembered).
func (s *session) optionalHub(ctx context.Context) *hubclient.Conn {
	h := s.hub()
	if h == nil {
		return nil
	}
	if s.hubFailed() {
		// Say once why the hub is skipped when the failure came from an
		// earlier longarm (only commands that would have asked the hub say it).
		s.hubNoteOnce.Do(func() {
			if s.hubStateNote != "" && !quietHub {
				fmt.Fprintln(os.Stderr, s.hubStateNote)
			}
		})
		return nil
	}
	if err := h.WaitReady(ctx, hubConnectTimeout); err != nil {
		if ctx.Err() == nil {
			s.hubDown(err)
		}
		return nil
	}
	s.hubUp()
	return h
}

// hubRetryAfter: how long a failed hub is skipped, by long-lived processes
// (longarm daemon, longarm mcp) and across longarm invocations (hub-state.json);
// LONGARM_HUB_RETRY_AFTER overrides.
var hubRetryAfter = func() time.Duration {
	if v, err := time.ParseDuration(os.Getenv("LONGARM_HUB_RETRY_AFTER")); err == nil && v >= 0 {
		return v
	}
	return 30 * time.Second
}()

func (s *session) hubFailed() bool {
	s.loadHubState()
	s.hubMu.Lock()
	defer s.hubMu.Unlock()
	return s.hubErr != nil && time.Since(s.hubErrAt) < hubRetryAfter
}

// hubUnavailable reports whether err means "the hub is not there" (as
// opposed to an answer such as NotFound or PermissionDenied).
func hubUnavailable(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled, codes.Unknown, codes.Internal:
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// hubNodes lists the hub's workers (all = every node). It caches workers
// for offline use and renews this client's certificate when due. ok=false
// when there is no usable hub.
func (s *session) hubNodes(ctx context.Context, all bool) ([]*pb.NodeInfo, bool) {
	h := s.optionalHub(ctx)
	if h == nil {
		return nil, false
	}
	cctx, cancel := context.WithTimeout(ctx, hubTimeout)
	defer cancel()
	resp, err := h.Fleet().ListNodes(cctx, &pb.ListNodesRequest{All: all})
	if err != nil {
		if hubUnavailable(err) {
			s.hubDown(err)
		} else {
			fmt.Fprintf(os.Stderr, "longarm: hub: %s\n", oneLine(err))
		}
		return nil, false
	}
	if !all {
		s.saveCache(resp.Nodes)
	}
	s.renewOnce.Do(func() { s.maybeRenew(ctx, h) })
	return resp.Nodes, true
}

func (s *session) saveCache(nodes []*pb.NodeInfo) {
	c := hubCache{Updated: time.Now()}
	for _, n := range nodes {
		slim := &pb.NodeInfo{Name: n.Name, Endpoints: n.Endpoints, Labels: n.Labels, Os: n.Os, Arch: n.Arch,
			Mirrors: n.Mirrors, WarmRepos: n.WarmRepos, State: n.State}
		b, err := protojson.Marshal(slim)
		if err == nil {
			c.Nodes = append(c.Nodes, b)
		}
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	tmp := filepath.Join(s.cfg.Dir, hubCacheFile+".tmp")
	if os.WriteFile(tmp, b, 0o600) == nil {
		platform.ReplaceFile(tmp, filepath.Join(s.cfg.Dir, hubCacheFile))
	}
}

func (s *session) cachedNodes() []*pb.NodeInfo {
	b, err := os.ReadFile(filepath.Join(s.cfg.Dir, hubCacheFile))
	if err != nil {
		return nil
	}
	var c hubCache
	if json.Unmarshal(b, &c) != nil {
		return nil
	}
	var out []*pb.NodeInfo
	for _, raw := range c.Nodes {
		n := &pb.NodeInfo{}
		if protojson.Unmarshal(raw, n) == nil {
			out = append(out, n)
		}
	}
	return out
}

// workerFromNode turns a hub-known worker into a client.Worker whose legs
// are all its advertised endpoints (raced by the dialer).
func workerFromNode(n *pb.NodeInfo) (client.Worker, bool) {
	return client.WorkerFromEndpoints(n.Name, n.Endpoints)
}

// knownWorkers are workers.toml's entries plus hub-known workers (live list
// if the hub answers, else the cache); manual entries win.
func (s *session) knownWorkers(ctx context.Context) []client.Worker {
	out := append([]client.Worker(nil), s.cfg.Workers...)
	seen := map[string]bool{}
	for _, w := range out {
		seen[w.Name] = true
	}
	nodes, ok := s.hubNodes(ctx, false)
	if !ok {
		nodes = s.cachedNodes()
	}
	for _, n := range nodes {
		if seen[n.Name] || n.State == "revoked" || n.State == "pending" {
			continue
		}
		if w, ok := workerFromNode(n); ok {
			out = append(out, w)
			seen[n.Name] = true
		}
	}
	return out
}

// worker resolves a worker name: workers.toml, then the hub (or its cache),
// then an ad-hoc "certname@host:port".
func (s *session) worker(name string) (client.Worker, error) {
	if s.cfg.Hub.Enabled() && s.revokedWorker(name) {
		return client.Worker{}, fmt.Errorf("worker %s is revoked (longarm admin nodes)", name)
	}
	for _, w := range s.cfg.Workers {
		if w.Name == name {
			return w, nil
		}
	}
	s.hubMu.Lock()
	p := s.placed
	s.hubMu.Unlock()
	if p != nil && p.Name == name {
		if w, ok := workerFromNode(p); ok {
			return w, nil
		}
	}
	if s.cfg.Hub.Enabled() {
		for attempt := 0; attempt < 2; attempt++ {
			s.refreshNodes(attempt > 0)
			s.hubMu.Lock()
			nodes := s.nodes
			s.hubMu.Unlock()
			for _, n := range nodes {
				if n.Name == name {
					if w, ok := workerFromNode(n); ok {
						return w, nil
					}
					return client.Worker{}, fmt.Errorf("worker %s has no usable endpoint (%v)", name, n.Endpoints)
				}
			}
		}
	}
	return s.cfg.Worker(name)
}

// refreshNodes (re)loads the hub's worker list for name lookups: at most
// every 30 s unless forced (an unknown name).
func (s *session) refreshNodes(force bool) {
	s.hubMu.Lock()
	fresh := !s.nodesAt.IsZero() && (time.Since(s.nodesAt) < 30*time.Second || (force && time.Since(s.nodesAt) < 2*time.Second))
	s.hubMu.Unlock()
	if fresh {
		return
	}
	nodes, ok := s.hubNodes(context.Background(), false)
	if !ok {
		nodes = s.cachedNodes()
	}
	s.hubMu.Lock()
	s.nodes, s.nodesAt = nodes, time.Now()
	s.hubMu.Unlock()
}

// maybeRenew renews this client's certificate through the hub when it
// expires within the hub's renew window or a new endorsement waits for it
// (longarm trust init|accept|import-key), and follows the hub's tailnet.
func (s *session) maybeRenew(ctx context.Context, h *hubclient.Conn) {
	b := s.dialer.Bundle()
	cctx, cancel := context.WithTimeout(ctx, hubTimeout)
	defer cancel()
	who, err := h.Fleet().Whoami(cctx, &pb.WhoamiRequest{})
	if err != nil {
		return
	}
	s.syncTailnet(ctx, h, who.Tailnet)
	if !hubclient.NeedsRenewal(b.Leaf(), who.RenewBefore.AsDuration()) && !client.TrustStore(s.cfg.Dir).RenewalRequested() {
		return
	}
	leaf, err := s.renewCert(cctx, h)
	if err != nil {
		fmt.Fprintf(os.Stderr, "longarm: certificate renewal failed: %s\n", oneLine(err))
		return
	}
	fmt.Fprintf(os.Stderr, "longarm: certificate renewed (valid until %s)\n", leaf.NotAfter.Local().Format(time.RFC3339))
}

// renewCert renews this client's certificate through h now. The new
// certificate carries the node key and its endorsement (end-to-end trust),
// which also settles a pending renewal request.
func (s *session) renewCert(ctx context.Context, h *hubclient.Conn) (*x509.Certificate, error) {
	ts := client.TrustStore(s.cfg.Dir)
	requested := ts.RenewalRequested() // before reading the endorsement: a newer one asks again
	t, err := ts.CSRTrust()
	if err != nil {
		return nil, fmt.Errorf("end-to-end trust: %w", err)
	}
	certPEM, keyPEM, caPEM, err := hubclient.Renew(ctx, h, s.dialer.Bundle().Identity.Name, t)
	if err != nil {
		return nil, err
	}
	if err := pki.WriteBundleAtomic(filepath.Join(s.cfg.Dir, client.TLSDir), caPEM, certPEM, keyPEM); err != nil {
		return nil, fmt.Errorf("save renewed certificate: %w", err)
	}
	s.dialer.Reload()
	if requested {
		ts.RenewalDone()
	}
	return pki.ParseCertPEM(certPEM)
}

// findJob locates the worker of a job: the hub's index, else every known
// worker in parallel.
func (s *session) findJob(ctx context.Context, id string) (string, error) {
	if h := s.optionalHub(ctx); h != nil {
		cctx, cancel := context.WithTimeout(ctx, hubTimeout)
		fj, err := h.Fleet().GetJob(cctx, &pb.FleetJobRequest{Id: id})
		cancel()
		if err == nil {
			return fj.Worker, nil
		}
		if hubUnavailable(err) {
			s.hubDown(err)
		}
	}
	ws := s.knownWorkers(ctx)
	found := make(chan string, len(ws))
	var wg sync.WaitGroup
	for _, w := range ws {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := s.dialer.Dial(w)
			if err != nil {
				return
			}
			defer conn.Close()
			cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			if _, err := conn.Jobs().Get(cctx, &pb.GetJobRequest{Id: id}); err == nil {
				found <- w.Name
			}
		}()
	}
	go func() { wg.Wait(); close(found) }()
	if w, ok := <-found; ok {
		return w, nil
	}
	return "", fmt.Errorf("job %s not found on any known worker", id)
}

// ---------------------------------------------------------------- placement

// placeOpts describe a job to place automatically.
type placeOpts struct {
	jobID   string
	repo    string
	memGB   float64
	typ     string
	require map[string]string
	noQueue bool
	exclude []string
}

// place picks a worker: through the hub (with reservation and queueing),
// else client-side over the known workers (same algorithm, live metrics,
// no reservations; it waits and retries when nobody passes).
func (s *session) place(ctx context.Context, o placeOpts) (string, error) {
	if h := s.optionalHub(ctx); h != nil {
		w, err := s.placeViaHub(ctx, h, o)
		if err == nil || !hubUnavailable(err) {
			return w, err
		}
		s.hubDown(err)
	}
	return s.placeLocally(ctx, o)
}

func (s *session) placeViaHub(ctx context.Context, h *hubclient.Conn, o placeOpts) (string, error) {
	st, err := h.Fleet().Place(ctx, &pb.PlaceRequest{JobId: o.jobID, Repo: o.repo, MemGb: o.memGB, Type: o.typ,
		Require: o.require, NoQueue: o.noQueue, Exclude: o.exclude})
	if err != nil {
		return "", err
	}
	for {
		u, err := st.Recv()
		if err != nil {
			return "", err
		}
		switch u.State {
		case "queued":
			fmt.Fprintf(os.Stderr, "longarm: queued at position %d (%s)\n", u.Position, u.Reason)
		case "assigned":
			s.hubMu.Lock()
			s.placed = u.Node
			s.hubMu.Unlock()
			fmt.Fprintf(os.Stderr, "longarm: placed on %s (score %.1f, %.0f GB reserved)\n", u.Worker, u.Score, u.MemGb)
			return u.Worker, nil
		}
	}
}

// candidates probes the known workers' metrics in parallel.
func (s *session) candidates(ctx context.Context, repo string) []sched.Candidate {
	ws := s.knownWorkers(ctx)
	cached := map[string]*pb.NodeInfo{}
	for _, n := range s.cachedNodes() {
		cached[n.Name] = n
	}
	out := make([]sched.Candidate, len(ws))
	var wg sync.WaitGroup
	for i, w := range ws {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := sched.Candidate{Name: w.Name, Eligible: true, Labels: map[string]string{}}
			if n := cached[w.Name]; n != nil {
				for k, v := range n.Labels {
					c.Labels[k] = v
				}
				c.HasMirror = repo != "" && contains(n.Mirrors, repo)
				c.Warm = repo != "" && contains(n.WarmRepos, repo)
			}
			out[i] = c
			conn, err := s.dialer.Dial(w)
			if err != nil {
				return
			}
			defer conn.Close()
			cctx, cancel := context.WithTimeout(ctx, 6*time.Second)
			defer cancel()
			m, err := conn.Metrics().Snapshot(cctx, &pb.MetricsRequest{})
			if err != nil {
				return
			}
			info, err := conn.Node().Info(cctx, &pb.InfoRequest{})
			if err == nil {
				c.Labels["os"], c.Labels["arch"] = info.Os, info.Arch
			}
			c.Online = true
			c.FreeGB = float64(m.MemTotalBytes-min(m.MemUsedBytes, m.MemTotalBytes)) / (1 << 30)
			c.TotalGB = float64(m.MemTotalBytes) / (1 << 30)
			c.CPU, c.Running = m.CpuPercent, int(m.JobsRunning)
			out[i] = c
		}()
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// localQueueInterval is how often the client-side fallback re-probes when
// nobody passes the gate.
var localQueueInterval = 10 * time.Second

func (s *session) placeLocally(ctx context.Context, o placeOpts) (string, error) {
	p := clientParams()
	mem := p.MemFor(o.typ, o.memGB)
	for {
		res := sched.Pick(p, s.candidates(ctx, o.repo), sched.Request{MemGB: mem, Require: o.require, Exclude: o.exclude})
		if res.Worker != "" {
			fmt.Fprintf(os.Stderr, "longarm: picked %s (score %.1f; client-side, no hub)\n", res.Worker, res.Score)
			return res.Worker, nil
		}
		if o.noQueue {
			return "", fmt.Errorf("no worker passes the gate: %s", res.Summary())
		}
		fmt.Fprintf(os.Stderr, "longarm: no worker passes the gate (%s); retrying in %s\n", res.Summary(), localQueueInterval)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(localQueueInterval):
		}
	}
}

// pickNow chooses a worker for a short command (no reservation, no queue).
func (s *session) pickNow(ctx context.Context, require map[string]string) (string, error) {
	p := clientParams()
	var cands []sched.Candidate
	if nodes, ok := s.hubNodes(ctx, false); ok {
		for _, n := range nodes {
			c := sched.Candidate{Name: n.Name, Labels: n.Labels, Online: n.Online && n.Metrics != nil,
				Eligible: n.State == "approved" && !n.Draining, Why: n.DrainReason, ReservedGB: n.ReservedGb, Running: int(n.JobsActive)}
			if m := n.Metrics; m != nil {
				c.FreeGB = float64(m.MemTotalBytes-min(m.MemUsedBytes, m.MemTotalBytes)) / (1 << 30)
				c.TotalGB = float64(m.MemTotalBytes) / (1 << 30)
				c.CPU = m.CpuPercent
			}
			cands = append(cands, c)
		}
	} else {
		cands = s.candidates(ctx, "")
	}
	res := sched.Pick(p, cands, sched.Request{MemGB: 0, Require: require})
	if res.Worker == "" {
		return "", fmt.Errorf("no worker passes the gate: %s", res.Summary())
	}
	return res.Worker, nil
}

// resolveTarget turns a worker argument ("", "auto" or a name) into a
// worker name, placing automatically when needed.
func (s *session) resolveTarget(ctx context.Context, w string, o *placeOpts) (string, error) {
	if w != "" && w != "auto" {
		return w, nil
	}
	if o == nil {
		return s.pickNow(ctx, nil)
	}
	return s.place(ctx, *o)
}

// clientParams are the scheduler parameters of client-side picks (exec
// --on auto, and run without a hub); LONGARM_GATE_FREE_GB / LONGARM_GATE_CPU
// override the gate.
func clientParams() sched.Params {
	p := sched.Defaults()
	if v, err := strconv.ParseFloat(os.Getenv("LONGARM_GATE_FREE_GB"), 64); err == nil {
		p.GateFreeGB = v
	}
	if v, err := strconv.ParseFloat(os.Getenv("LONGARM_GATE_CPU"), 64); err == nil {
		p.GateCPU = v
	}
	return p
}

// revokedWorker reports whether the hub (or its cached list) says name is
// a revoked worker; longarm refuses to dial it even from workers.toml.
func (s *session) revokedWorker(name string) bool {
	s.refreshNodes(false)
	s.hubMu.Lock()
	defer s.hubMu.Unlock()
	for _, n := range s.nodes {
		if n.Name == name {
			return n.State == "revoked"
		}
	}
	return false
}
