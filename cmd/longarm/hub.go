package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/nodeid"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/sched"
	"github.com/lubobill1990/longarm/internal/telemetry"
	"github.com/lubobill1990/longarm/internal/trust"
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
			fmt.Fprintf(os.Stderr, "longarm: hub %s unavailable (%s); direct connections to workers still work (known workers from the cache; placement, sign-in and new links wait for the hub)\n", s.cfg.Hub.Address, oneLine(err))
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
		s.hubStateNote = fmt.Sprintf("longarm: hub %s was unavailable %s ago (%s); direct connections to workers still work (next try in %s)",
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

// requireHub is the hub connection for callers that need its answer
// (jobs --hub-only): exit 69 when no hub is configured or it doesn't connect
// within hubConnectTimeout. An earlier failure (hub-state.json) is not
// trusted: the hub is asked again.
func (s *session) requireHub(ctx context.Context) (*hubclient.Conn, error) {
	h := s.hub()
	if h == nil {
		why := "no hub configured"
		if noHub {
			why = "--no-hub / LONGARM_NO_HUB=1"
		} else if s.cfg.Hub.Enabled() {
			why = fmt.Sprintf("hub %s: %v", s.cfg.Hub.Address, s.hubErr)
		}
		return nil, &exitError{code: exitHubUnavailable, msg: why}
	}
	if err := h.WaitReady(ctx, hubConnectTimeout); err != nil {
		return nil, &exitError{code: exitHubUnavailable, msg: fmt.Sprintf("hub %s unreachable: %s", s.cfg.Hub.Address, oneLine(err))}
	}
	s.hubUp()
	return h, nil
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
	s.renewOnce.Do(func() {
		if who := s.maybeRenew(ctx, h); who != nil && noticeOK {
			maybeUpdateNotice(os.Stderr, s.cfg.Dir, who, s.cfg.Hub.Address, time.Now())
		}
	})
	return resp.Nodes, true
}

func (s *session) saveCache(nodes []*pb.NodeInfo) {
	c := hubCache{Updated: time.Now()}
	for _, n := range nodes {
		slim := &pb.NodeInfo{Name: n.Name, Endpoints: n.Endpoints, Labels: n.Labels, Os: n.Os, Arch: n.Arch,
			Mirrors: n.Mirrors, WarmRepos: n.WarmRepos, State: n.State, Id: n.Id, CertName: n.CertName, Owner: n.Owner,
			CompanionId: n.CompanionId, AdminWorker: n.AdminWorker, Companion: n.Companion}
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

func (s *session) cachedNodes() []*pb.NodeInfo { return cachedNodesIn(s.cfg.Dir) }

// cachedNodesIn reads the hub's cached worker list of a config dir.
func cachedNodesIn(dir string) []*pb.NodeInfo {
	b, err := os.ReadFile(filepath.Join(dir, hubCacheFile))
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
// are all its advertised endpoints (raced by the dialer), dialed by its
// certificate's name and held to its node ID.
func workerFromNode(n *pb.NodeInfo) (client.Worker, bool) {
	return client.WorkerFromNode(n)
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
		w, ok := workerFromNode(n)
		if !ok { // listed all the same (ls, completion); dialing says why it fails
			all := proto.Clone(n).(*pb.NodeInfo)
			all.Hostname = "" // every endpoint, loopback ones too
			w, ok = client.WorkerFromNode(all)
		}
		if ok {
			w.TrustAny = true // every known worker (listings): no target to hold it to
			out = append(out, w)
			seen[n.Name] = true
		}
	}
	return out
}

// worker resolves a worker target (docs/security.md "Node identity"): a
// name, a node ID, a unique ID prefix (4+ hex digits), id:<prefix> or
// name:<name>. Names: workers.toml, then the hub (or its cache); IDs and
// prefixes: the hub's workers; then an ad-hoc "certname@host:port". A
// target matching several workers is an error listing them.
func (s *session) worker(target string) (client.Worker, error) {
	t, terr := nodeid.ParseTarget(target)
	if terr == nil && t.Mode == nodeid.ByName {
		for _, w := range s.cfg.Workers { // manual entries win
			if w.Name == t.Value {
				return w, nil
			}
		}
	}
	if terr == nil && s.cfg.Hub.Enabled() {
		for attempt := 0; attempt < 2; attempt++ {
			s.refreshNodes(attempt > 0)
			s.hubMu.Lock()
			nodes, live := s.nodes, s.nodesLive
			s.hubMu.Unlock()
			n, err := resolveNode(t, nodes, "worker")
			if errors.Is(err, nodeid.ErrAmbiguous) {
				return client.Worker{}, usagef("%s", err.Error())
			}
			if err != nil {
				continue
			}
			if n.State == "revoked" {
				return client.Worker{}, fmt.Errorf("worker %s (%s) is revoked (longarm admin nodes)", n.Name, nodeid.Short(n.Id))
			}
			now := time.Now()
			if live && !forceDial && workerOffline(n, now) {
				return client.Worker{}, offlineError(n, now)
			}
			w, ok := workerFromNode(n)
			if !ok && len(n.Endpoints) > 0 {
				return client.Worker{}, fmt.Errorf("worker %s listens only on its own machine (%s): unreachable from here; "+
					"an updated longarmd there also listens on the hub's relay", n.Name, strings.Join(n.Endpoints, ", "))
			}
			if !ok {
				return client.Worker{}, fmt.Errorf("worker %s has no usable endpoint (%v)", n.Name, n.Endpoints)
			}
			if live && !n.Online && n.State == "approved" {
				w.Note = offlineNote(n, now) // said when no leg connects
			}
			if t.Mode == nodeid.ByName {
				w.TrustName = t.Value
			} else {
				w.TrustIDPrefix = t.Value
			}
			return w, nil
		}
	}
	if terr == nil && t.Mode != nodeid.ByName {
		return client.Worker{}, &codedError{code: exitNotFound, err: fmt.Errorf("no worker with id %s (the hub knows workers by id)", t.Value)}
	}
	name := target
	if terr == nil {
		name = t.Value
	}
	w, err := s.cfg.Worker(name)
	if err != nil {
		return w, &codedError{code: exitNotFound, err: unknownWorkerError(name, s.cfg.Dir, s.knownNames())}
	}
	return w, nil
}

// resolveNode picks the node target t means among nodes (nodeid.Resolve).
func resolveNode(t nodeid.Target, nodes []*pb.NodeInfo, what string) (*pb.NodeInfo, error) {
	cands := make([]nodeid.Candidate, 0, len(nodes))
	byID := map[string]*pb.NodeInfo{}
	for _, n := range nodes {
		if n.Id == "" { // a cache of a hub before node IDs: names only
			if t.Mode == nodeid.ByName && n.Name == t.Value {
				return n, nil
			}
			continue
		}
		owner := n.Owner
		if n.Role != "" && n.Role != "worker" {
			owner = n.User
		}
		cands = append(cands, nodeid.Candidate{ID: n.Id, Name: n.Name, Owner: owner, State: n.State})
		byID[n.Id] = n
	}
	c, err := t.Resolve(cands, what)
	if err != nil {
		return nil, err
	}
	return byID[c.ID], nil
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
	s.nodes, s.nodesAt, s.nodesLive = nodes, time.Now(), ok
	s.hubMu.Unlock()
}

// maybeRenew renews this client's certificate through the hub when it
// expires within the hub's renew window or a new endorsement waits for it
// (longarm trust init|accept|import-key), and follows the hub's tailnet.
func (s *session) maybeRenew(ctx context.Context, h *hubclient.Conn) *pb.WhoamiResponse {
	b := s.dialer.Bundle()
	cctx, cancel := context.WithTimeout(ctx, hubTimeout)
	defer cancel()
	who, err := h.Fleet().Whoami(cctx, &pb.WhoamiRequest{Os: runtime.GOOS, Arch: runtime.GOARCH})
	if err != nil {
		return nil
	}
	s.syncTailnet(ctx, h, who.Tailnet)
	if !hubclient.NeedsRenewal(b.Leaf(), who.RenewBefore.AsDuration()) && !client.TrustStore(s.cfg.Dir).RenewalRequested() {
		return who
	}
	leaf, err := s.renewCert(cctx, h)
	if err != nil {
		fmt.Fprintln(os.Stderr, renewFailedNote(b.Leaf().NotAfter, time.Now(), oneLine(err)))
		return who
	}
	fmt.Fprintf(os.Stderr, "longarm: certificate renewed (valid until %s)\n", leaf.NotAfter.Local().Format(time.RFC3339))
	return who
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
	leaf, err := pki.ParseCertPEM(certPEM)
	if err == nil {
		migrateSelfEndorsement(ts, leaf)
	}
	return leaf, err
}

// migrateSelfEndorsement moves this device's own endorsement to its node
// ID (docs/security.md "Node identity", "Migration"): a device holding the
// user key that endorsed it re-endorses itself once its certificate
// carries an ID; the next renewal carries the new endorsement. Devices
// without the user key keep the name-bound one (valid while the name is).
func migrateSelfEndorsement(ts trust.Store, leaf *x509.Certificate) {
	id, err := pki.IdentityFromCert(leaf)
	if err != nil || id.ID == "" {
		return
	}
	e, err := ts.Endorsement()
	if err != nil || e == nil || e.ID != "" {
		return
	}
	k, user, err := ts.UserKey()
	if err != nil || user != e.User || user != id.Owner() || !bytes.Equal(k.Public().(ed25519.PublicKey), e.UserKey) {
		return
	}
	if err := endorseSelf(ts, k, user, id); err != nil {
		fmt.Fprintf(os.Stderr, "longarm: endorsing this device under its node id: %s\n", oneLine(err))
		return
	}
	fmt.Fprintf(os.Stderr, "longarm: this device's endorsement now names its node id %s\n", id.ID)
}

// findJob locates the worker of a job: the hub's index, else every known
// worker in parallel. "Not found" (exit 3) only when every worker that may
// have it answered so; a hub that an earlier longarm found down is asked
// again before that, and workers that did not answer make it a longarm
// error (exit 255: the outcome is unknown), never "not found".
func (s *session) findJob(ctx context.Context, id string) (string, error) {
	askHub := func(h *hubclient.Conn) (string, bool) {
		cctx, cancel := context.WithTimeout(ctx, hubTimeout)
		fj, err := h.Fleet().GetJob(cctx, &pb.FleetJobRequest{Id: id})
		cancel()
		if err == nil {
			return fj.Worker, true
		}
		if hubUnavailable(err) {
			s.hubDown(err)
		}
		return "", false
	}
	hubSkipped := s.hub() != nil && s.hubFailed()
	if h := s.optionalHub(ctx); h != nil {
		if w, ok := askHub(h); ok {
			return w, nil
		}
	}
	ws := s.knownWorkers(ctx)
	type answer struct {
		name  string
		found bool
	}
	answers := make(chan answer, len(ws))
	var wg sync.WaitGroup
	for _, w := range ws {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := s.dialer.Dial(w)
			if err != nil {
				answers <- answer{name: w.Name}
				return
			}
			defer conn.Close()
			// as long as `longarm jobs` gives a worker: a first connection
			// through a relay can take a while
			cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			_, err = conn.Jobs().Get(cctx, &pb.GetJobRequest{Id: id})
			switch {
			case err == nil:
				answers <- answer{name: w.Name, found: true}
			case status.Code(err) == codes.NotFound:
				answers <- answer{found: false} // answered: not there
			default:
				answers <- answer{name: w.Name} // no answer
			}
		}()
	}
	go func() { wg.Wait(); close(answers) }()
	var silent []string
	for a := range answers {
		if a.found {
			return a.name, nil
		}
		if a.name != "" {
			silent = append(silent, a.name)
		}
	}
	if hubSkipped {
		// the remembered failure may be over: ask the hub itself once more
		if h := s.hub(); h != nil && h.WaitReady(ctx, hubTimeout) == nil {
			s.hubUp()
			if w, ok := askHub(h); ok {
				return w, nil
			}
		}
	}
	if len(silent) > 0 {
		sort.Strings(silent)
		return "", fmt.Errorf("job %s: not on the workers that answered; %s did not answer (give the worker: longarm <command> <worker> %s)",
			id, strings.Join(silent, ", "), id)
	}
	return "", notFoundf("job %s not found on any known worker", id)
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

// connFor connects to the worker of an operation: the worker the hub
// assigned to the automatic placement po (its job id) - consumed here, for
// this operation only, the one case where the worker's endorsement needn't
// match a name or ID the user gave (TrustAny) - else the target as asked
// for (s.conn).
func (s *session) connFor(target string, po *placeOpts) (*client.Conn, error) {
	if po != nil {
		if w, ok := s.takePlaced(po.jobID, target); ok {
			return s.dialer.Dial(w)
		}
	}
	return s.conn(target)
}

// takePlaced consumes the placement of job jobID if it put the job on
// target: the worker, explicitly unconstrained (TrustAny).
func (s *session) takePlaced(jobID, target string) (client.Worker, bool) {
	s.hubMu.Lock()
	n := s.placements[jobID]
	delete(s.placements, jobID)
	s.hubMu.Unlock()
	if n == nil || n.Name != target {
		return client.Worker{}, false
	}
	w, ok := workerFromNode(n)
	w.TrustAny = true // the hub placed this job: no name or ID the user gave
	return w, ok
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
		if w, ok := runsAlready(err, o.jobID); ok {
			// A retried run --id: the job exists. Submit to its worker,
			// which returns it (same spec) or refuses (another spec).
			fmt.Fprintf(os.Stderr, "longarm: job %s exists on %s\n", o.jobID, w)
			return w, nil
		}
		if err != nil {
			return "", err
		}
		switch u.State {
		case "queued":
			fmt.Fprintf(os.Stderr, "longarm: queued at position %d (%s)\n", u.Position, u.Reason)
		case "assigned":
			if u.Node != nil {
				s.hubMu.Lock()
				if s.placements == nil {
					s.placements = map[string]*pb.NodeInfo{}
				}
				s.placements[o.jobID] = u.Node
				s.hubMu.Unlock()
			}
			fmt.Fprintf(os.Stderr, "longarm: placed on %s (score %.1f, %.0f GB reserved)\n", u.Worker, u.Score, u.MemGb)
			return u.Worker, nil
		}
	}
}

// runsAlready recognizes the hub's answer to placing a job id the caller
// already has on a worker ("job <id> already runs on <worker>").
func runsAlready(err error, id string) (string, bool) {
	s, ok := status.FromError(err)
	if !ok || s.Code() != codes.AlreadyExists {
		return "", false
	}
	w, ok := strings.CutPrefix(s.Message(), "job "+id+" already runs on ")
	return w, ok && w != "" && !strings.ContainsAny(w, " \t")
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
			return "", status.Errorf(codes.ResourceExhausted, "no worker passes the gate: %s", res.Summary())
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
		return "", status.Errorf(codes.ResourceExhausted, "no worker passes the gate: %s", res.Summary())
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
