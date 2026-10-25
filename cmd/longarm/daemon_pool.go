package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/localapi"
	"github.com/lubobill1990/longarm/internal/pool"
	"github.com/lubobill1990/longarm/internal/transport"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// The daemon's pool view (longarm pool; the tray's client popup): a latency
// monitor with one persistent connection per worker per advertised leg,
// RTT every 2 s, path tracking, metrics from the hub's events or directly
// from the workers. Also the client tab's jobs, logs, kill and the backend
// setting (longarm transport).

// legProber probes one leg over its own gRPC connection.
type legProber struct {
	conn *client.Conn
}

func (p *legProber) Probe(ctx context.Context) (time.Duration, transport.PathInfo, error) {
	start := time.Now()
	_, err := p.conn.Node().Info(ctx, &pb.InfoRequest{})
	rtt := time.Since(start)
	if err != nil {
		return 0, transport.PathInfo{}, err
	}
	pi := p.conn.Path()
	if pi.Transport == "longnet" && pi.RTT > 0 { // QUIC's smoothed RTT: no gRPC overhead
		rtt = pi.RTT
	}
	return rtt, pi, nil
}

func (p *legProber) Metrics(ctx context.Context, interval time.Duration, fn func(*pb.MetricsSnapshot)) error {
	st, err := p.conn.Metrics().Stream(ctx, &pb.MetricsStreamRequest{Interval: durationpb.New(interval)})
	if err != nil {
		return err
	}
	for {
		m, err := st.Recv()
		if err != nil {
			return err
		}
		fn(m)
	}
}

func (p *legProber) Close() { p.conn.Close() }

// poolTargets are the workers to monitor: workers.toml entries (one leg)
// and the hub's workers (every advertised leg; the cache when the hub is
// down).
func (d *daemon) poolTargets(ctx context.Context) ([]pool.Target, bool) {
	var out []pool.Target
	seen := map[string]bool{}
	for _, w := range d.s.cfg.Workers {
		seen[w.Name] = true
		out = append(out, pool.Target{Name: w.Name, Legs: []client.Worker{w}})
	}
	nodes, ok := d.s.hubNodes(ctx, false)
	if !ok {
		nodes = d.s.cachedNodes()
	}
	for _, n := range nodes {
		if seen[n.Name] || n.State == "revoked" || n.State == "pending" || (n.Role != "" && n.Role != "worker") {
			continue
		}
		seen[n.Name] = true
		out = append(out, pool.Target{Name: n.Name, Legs: client.NodeLegs(n.Name, n.Endpoints)})
		if ok {
			d.mon.HubNode(n)
		}
	}
	return out, ok
}

// newMonitor builds the pool monitor of the daemon.
func (d *daemon) newMonitor() *pool.Monitor {
	return pool.New(pool.Config{
		Interval: poolInterval,
		Targets:  d.poolTargets,
		Dial: func(leg client.Worker) (pool.Prober, error) {
			c, err := d.s.dialer.Dial(leg)
			if err != nil {
				return nil, err
			}
			return &legProber{conn: c}, nil
		},
		Policy: d.s.dialer.Policy,
		Sticky: d.s.dialer.StickyLeg,
	})
}

// liveLegs is the pool monitor's current view of a worker's legs for the
// dialer's race (here and in one-shot longarm processes, GET /v1/legs/live).
func (d *daemon) liveLegs(worker string) []client.LiveLeg {
	return d.mon.LiveLegs(worker, 4*poolInterval)
}

// poolInterval is the probe / metrics interval (LONGARM_POOL_INTERVAL for tests).
var poolInterval = func() time.Duration {
	if v, err := time.ParseDuration(os.Getenv("LONGARM_POOL_INTERVAL")); err == nil && v >= 100*time.Millisecond {
		return v
	}
	return 2 * time.Second
}()

// poolSideLoops reports links to the hub and counts this client's jobs.
func (d *daemon) poolSideLoops(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		d.countMyJobs(ctx)
		if h := d.s.optionalHub(ctx); h != nil {
			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if _, err := h.Fleet().ReportLinks(cctx, d.mon.LinkReport()); err != nil && status.Code(err) != codes.Unimplemented && hubUnavailable(err) {
				d.s.hubDown(err)
			}
			cancel()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (d *daemon) identity() string { return d.s.dialer.Identity().String() }

func activeState(s pb.JobState) bool {
	switch s {
	case pb.JobState_JOB_STATE_QUEUED, pb.JobState_JOB_STATE_ASSIGNED, pb.JobState_JOB_STATE_PREPARING, pb.JobState_JOB_STATE_RUNNING:
		return true
	}
	return false
}

func (d *daemon) countMyJobs(ctx context.Context) {
	r, err := d.ListJobs(ctx, &pb.ClientJobsRequest{Mine: true, Active: true})
	if err != nil {
		return
	}
	counts := map[string]int32{}
	for _, j := range r.Jobs {
		counts[j.Worker]++
	}
	d.mon.SetMyJobs(counts)
}

// conn is a cached connection to a worker for direct job operations.
func (d *daemon) conn(name string) (*client.Conn, error) {
	d.connMu.Lock()
	c := d.conns[name]
	d.connMu.Unlock()
	if c != nil {
		return c, nil
	}
	c, err := d.s.conn(name) // may ask the hub: not under connMu
	if err != nil {
		return nil, err
	}
	d.connMu.Lock()
	defer d.connMu.Unlock()
	if old := d.conns[name]; old != nil { // raced with another caller
		c.Close()
		return old, nil
	}
	d.conns[name] = c
	return c, nil
}

func (d *daemon) closeConns() {
	d.connMu.Lock()
	defer d.connMu.Unlock()
	for n, c := range d.conns {
		c.Close()
		delete(d.conns, n)
	}
}

// ---------------------------------------------------------------- RPCs

func (d *daemon) Pool(_ context.Context, req *pb.PoolRequest) (*pb.PoolView, error) {
	v := d.mon.View(req.History)
	d.mu.Lock()
	v.Hub = proto.Clone(d.link).(*pb.HubLink)
	d.mu.Unlock()
	v.Identity = d.identity()
	v.Firewall = d.fw.Get()
	return v, nil
}

func (d *daemon) WatchPool(req *pb.PoolRequest, st pb.LocalClient_WatchPoolServer) error {
	return d.watchPool(st.Context(), req, st.Send)
}

// watchPool sends the view at once, then at most every interval while it
// changes (and at least every interval).
func (d *daemon) watchPool(ctx context.Context, req *pb.PoolRequest, send func(*pb.PoolView) error) error {
	for {
		changed := d.mon.Changed()
		v, _ := d.Pool(ctx, req)
		if err := send(v); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(poolInterval):
		}
		select { // coalesce: wait for a change, but not longer than another interval
		case <-ctx.Done():
			return nil
		case <-changed:
		case <-time.After(poolInterval):
		}
	}
}

func (d *daemon) ListJobs(ctx context.Context, req *pb.ClientJobsRequest) (*pb.FleetJobsResponse, error) {
	me := d.identity()
	keep := func(j *pb.Job) bool {
		return (!req.Mine || j.GetSpec().GetSubmittedBy() == me) && (!req.Active || activeState(j.GetState()))
	}
	var states []pb.JobState
	if req.Active {
		states = []pb.JobState{pb.JobState_JOB_STATE_PREPARING, pb.JobState_JOB_STATE_RUNNING}
	}
	out := &pb.FleetJobsResponse{}
	if h := d.s.optionalHub(ctx); h != nil {
		cctx, cancel := context.WithTimeout(ctx, hubTimeout)
		r, err := h.Fleet().ListJobs(cctx, &pb.FleetJobsRequest{Worker: req.Worker, States: states})
		cancel()
		if err == nil {
			for _, j := range r.Jobs {
				if keep(j.Job) {
					out.Jobs = append(out.Jobs, j)
				}
			}
			return limitJobs(out, req.Limit), nil
		}
		if hubUnavailable(err) {
			d.s.hubDown(err)
		}
	}
	// Without the hub: ask each worker (in parallel).
	names := d.mon.Workers()
	if req.Worker != "" {
		names = []string{req.Worker}
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := d.conn(name)
			if err != nil {
				return
			}
			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			r, err := c.Jobs().List(cctx, &pb.ListJobsRequest{States: states, Limit: 200})
			if err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, j := range r.Jobs {
				if keep(j) {
					out.Jobs = append(out.Jobs, &pb.FleetJob{Worker: name, Job: j, UpdatedAt: timestamppb.Now(), WorkerOnline: true})
				}
			}
		}()
	}
	wg.Wait()
	sort.Slice(out.Jobs, func(i, j int) bool {
		return out.Jobs[i].Job.GetCreatedAt().AsTime().After(out.Jobs[j].Job.GetCreatedAt().AsTime())
	})
	return limitJobs(out, req.Limit), nil
}

func limitJobs(r *pb.FleetJobsResponse, limit int32) *pb.FleetJobsResponse {
	if limit > 0 && len(r.Jobs) > int(limit) {
		r.Jobs = r.Jobs[:limit]
	}
	return r
}

// jobWorker locates a job's worker (given, the hub's index, or by asking).
func (d *daemon) jobWorker(ctx context.Context, id, worker string) (string, error) {
	if worker != "" {
		return worker, nil
	}
	return d.s.findJob(ctx, id)
}

func (d *daemon) JobLogs(req *pb.ClientLogsRequest, st pb.LocalClient_JobLogsServer) error {
	return d.jobLogs(st.Context(), req, st.Send)
}

func (d *daemon) jobLogs(ctx context.Context, req *pb.ClientLogsRequest, send func(*pb.LogChunk) error) error {
	w, err := d.jobWorker(ctx, req.Id, req.Worker)
	if err != nil {
		return status.Error(codes.NotFound, err.Error())
	}
	c, err := d.conn(w)
	if err != nil {
		return status.Error(codes.NotFound, err.Error())
	}
	st, err := c.Jobs().Logs(ctx, &pb.LogsRequest{Id: req.Id, Offset: req.Offset, Follow: req.Follow})
	if err != nil {
		return err
	}
	for {
		ch, err := st.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := send(ch); err != nil {
			return err
		}
	}
}

func (d *daemon) KillJob(ctx context.Context, req *pb.FleetJobRequest) (*pb.Job, error) {
	w, err := d.jobWorker(ctx, req.Id, req.Worker)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	c, err := d.conn(w)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	return c.Jobs().Kill(ctx, &pb.KillRequest{Id: req.Id})
}

func (d *daemon) GetSettings(context.Context, *pb.GetSettingsRequest) (*pb.ClientSettings, error) {
	return &pb.ClientSettings{Transport: d.s.dialer.Policy()}, nil
}

func (d *daemon) SetSettings(ctx context.Context, req *pb.ClientSettings) (*pb.ClientSettings, error) {
	p, err := client.NormalizePolicy(req.Transport)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := client.SaveSettings(d.s.cfg.Dir, client.Settings{Transport: p}); err != nil {
		return nil, err
	}
	d.s.dialer.SetPolicy(p)
	d.closeConns() // new job connections use the new policy
	fmt.Fprintf(os.Stderr, "longarm daemon: transport policy %s\n", p)
	return d.GetSettings(ctx, nil)
}

// poolGateway adds the pool routes to the daemon's JSON/SSE gateway:
//
//	GET  /v1/pool?history=1
//	GET  /v1/pool/watch?history=1      (SSE, event "pool": PoolView)
//	GET  /v1/jobs?mine=1&active=1&worker=&limit=
//	GET  /v1/jobs/{id}/logs?worker=&offset=&follow=1  (SSE, event "log")
//	POST /v1/jobs/{id}/kill?worker=
//	GET  /v1/settings ; POST /v1/settings {"transport": "race"}
//	POST /v1/workers/{name}/drain {"draining": true}   (longarm admin drain; admin role)
func (d *daemon) poolGateway(mux *http.ServeMux) {
	flag := func(r *http.Request, k string) bool { v := r.URL.Query().Get(k); return v == "1" || v == "true" }
	mux.HandleFunc("GET /v1/pool", func(w http.ResponseWriter, r *http.Request) {
		v, err := d.Pool(r.Context(), &pb.PoolRequest{History: flag(r, "history")})
		localapi.WriteProto(w, v, err)
	})
	mux.HandleFunc("GET /v1/pool/watch", func(w http.ResponseWriter, r *http.Request) {
		req := &pb.PoolRequest{History: flag(r, "history")}
		localapi.SSEStream(w, r, "pool", func(ctx context.Context, send func(proto.Message) error) error {
			return d.watchPool(ctx, req, func(v *pb.PoolView) error { return send(v) })
		})
	})
	mux.HandleFunc("GET /v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		res, err := d.ListJobs(r.Context(), &pb.ClientJobsRequest{Worker: q.Get("worker"), Mine: flag(r, "mine"),
			Active: flag(r, "active"), Limit: int32(limit)})
		localapi.WriteProto(w, res, err)
	})
	mux.HandleFunc("GET /v1/jobs/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		off, _ := strconv.ParseInt(q.Get("offset"), 10, 64)
		req := &pb.ClientLogsRequest{Id: r.PathValue("id"), Worker: q.Get("worker"), Offset: off, Follow: flag(r, "follow")}
		localapi.SSEStream(w, r, "log", func(ctx context.Context, send func(proto.Message) error) error {
			return d.jobLogs(ctx, req, func(c *pb.LogChunk) error { return send(c) })
		})
	})
	mux.HandleFunc("POST /v1/jobs/{id}/kill", func(w http.ResponseWriter, r *http.Request) {
		j, err := d.KillJob(r.Context(), &pb.FleetJobRequest{Id: r.PathValue("id"), Worker: r.URL.Query().Get("worker")})
		localapi.WriteProto(w, j, err)
	})
	mux.HandleFunc("POST /v1/workers/{name}/drain", func(w http.ResponseWriter, r *http.Request) {
		req := &pb.SetDrainRequest{}
		if err := localapi.ReadProto(r, req); err != nil {
			localapi.WriteProto(w, nil, err)
			return
		}
		req.Name = r.PathValue("name")
		n, err := d.DrainWorker(r.Context(), req)
		localapi.WriteProto(w, n, err)
	})
	mux.HandleFunc("GET /v1/settings", func(w http.ResponseWriter, r *http.Request) {
		s, err := d.GetSettings(r.Context(), nil)
		localapi.WriteProto(w, s, err)
	})
	mux.HandleFunc("POST /v1/settings", func(w http.ResponseWriter, r *http.Request) {
		req := &pb.ClientSettings{}
		if err := localapi.ReadProto(r, req); err != nil {
			localapi.WriteProto(w, nil, err)
			return
		}
		s, err := d.SetSettings(r.Context(), req)
		localapi.WriteProto(w, s, err)
	})
}

// ---------------------------------------------------------------- CLI

// poolCmd is `longarm pool`: the daemon's live view (starts the daemon).
func poolCmd() *cobra.Command {
	var asJSON, watch bool
	c := &cobra.Command{
		Use:   "pool",
		Short: "Live pool view from longarm daemon: state, CPU/memory, agents, and this client's latency per leg (path, RTT)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, closeFn, err := daemonClient(true)
			if err != nil {
				return err
			}
			defer closeFn()
			show := func(v *pb.PoolView) {
				if asJSON {
					b, _ := protojson.MarshalOptions{UseProtoNames: true}.Marshal(v)
					fmt.Println(string(b))
					return
				}
				printPool(os.Stdout, v)
			}
			if !watch {
				ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
				defer cancel()
				v, err := c.Pool(ctx, &pb.PoolRequest{})
				if err != nil {
					return err
				}
				show(v)
				return nil
			}
			st, err := c.WatchPool(cmd.Context(), &pb.PoolRequest{})
			if err != nil {
				return err
			}
			for {
				v, err := st.Recv()
				if err != nil {
					return err
				}
				show(v)
			}
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "JSON output")
	c.Flags().BoolVarP(&watch, "watch", "w", false, "keep printing every 2 s")
	return c
}

func printPool(w io.Writer, v *pb.PoolView) {
	hub := "no hub"
	if v.Hub.GetAddress() != "" {
		hub = "hub " + map[bool]string{true: "live", false: "down"}[v.Hub.GetConnected()]
	}
	fmt.Fprintf(w, "%s  source %s  transport %s  %s\n", v.Time.AsTime().Local().Format("15:04:05"), v.Source, v.Transport, hub)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "WORKER\tSTATE\tCPU\tMEM\tCC\tCP\tJOBS\tLINK\tRTT\tLEGS")
	for _, p := range v.Workers {
		cpu, mem, cc, cp := "-", "-", "-", "-"
		if m := p.Metrics; m != nil {
			cpu = fmt.Sprintf("%.0f%%", m.CpuPercent)
			mem = fmt.Sprintf("%.1f/%.0fG", float64(m.MemUsedBytes)/(1<<30), float64(m.MemTotalBytes)/(1<<30))
			cc = fmt.Sprint(m.Agents["claude"].GetSessions())
			cp = fmt.Sprint(m.Agents["copilot"].GetSessions())
		}
		var legs []string
		for _, l := range p.GetLink().GetLegs() {
			s := l.Label
			if l.Up {
				s += " " + fmtRTT(l.RttMs)
			} else {
				s += " down"
			}
			if l.Nat != "" {
				s += " nat " + l.Nat
			}
			legs = append(legs, s)
		}
		state := p.State + " " + humanDuration(time.Since(p.Since.AsTime()))
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n", p.Name, state, cpu, mem, cc, cp, p.MyJobs,
			p.GetLink().GetActive(), fmtRTT(p.GetLink().GetRttMs()), strings.Join(legs, ", "))
	}
	tw.Flush()
	if note := firewallNote(v.Firewall, relayedWorkers(v)); note != "" {
		fmt.Fprintln(w, note)
	}
}

// relayedWorkers lists the workers with a leg that is up over a relay
// ("wa (longnet relay-udp)").
func relayedWorkers(v *pb.PoolView) []string {
	var out []string
	for _, p := range v.Workers {
		var ls []string
		for _, l := range p.GetLink().GetLegs() {
			if l.Up && relayedKind(l.Kind) {
				ls = append(ls, l.Label)
			}
		}
		if len(ls) > 0 {
			out = append(out, p.Name+" ("+strings.Join(ls, ", ")+")")
		}
	}
	return out
}

// transportCmd is `longarm transport [race|tcp|tailscale|longnet]`.
func transportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "transport [" + strings.Join(client.Policies, "|") + "]",
		Short: "Show or set the transport policy: race (all legs, first mTLS handshake wins) or the transport to prefer",
		Long: `The transport policy decides how longarm reaches a worker that has several legs
(tcp, longnet, tailscale):

  race       dial every leg, the first finished mTLS handshake wins (default);
             the winner is sticky (transport-cache.json) and gets a head start
  tcp | tailscale | longnet
             prefer that transport; the other legs start after 2 s or when it fails

It is saved in settings.json in the config dir (a running longarm daemon and the
tray switch at once). LONGARM_TRANSPORT or --transport override it for one command.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				p := client.LoadSettings(configDir).Transport
				over := transportPolicy
				if over == "" {
					over = os.Getenv("LONGARM_TRANSPORT")
				}
				if op, err := client.NormalizePolicy(over); over != "" && err == nil && op != p {
					fmt.Printf("%s (saved: %s)\n", op, p)
					return nil
				}
				fmt.Println(p)
				return nil
			}
			p, err := client.NormalizePolicy(args[0])
			if err != nil {
				return err
			}
			if c, closeFn, err := daemonClient(false); err == nil { // the running daemon switches at once
				defer closeFn()
				s, err := c.SetSettings(cmd.Context(), &pb.ClientSettings{Transport: p})
				if err != nil {
					return err
				}
				fmt.Println(s.Transport)
				return nil
			}
			if err := client.SaveSettings(configDir, client.Settings{Transport: p}); err != nil {
				return err
			}
			fmt.Println(p)
			return nil
		},
	}
}

// DrainWorker is longarm admin drain / undrain through the daemon (admin role).
func (d *daemon) DrainWorker(ctx context.Context, req *pb.SetDrainRequest) (*pb.NodeInfo, error) {
	h := d.s.hub()
	if h == nil {
		return nil, status.Error(codes.FailedPrecondition, "no hub configured")
	}
	cctx, cancel := context.WithTimeout(ctx, hubTimeout)
	defer cancel()
	return h.Admin().SetDrain(cctx, req)
}
