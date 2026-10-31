package longarmd

import (
	"context"
	"log/slog"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"

	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pii"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/supervisor"
	"github.com/lubobill1990/longarm/internal/telemetry"
	"github.com/lubobill1990/longarm/internal/telemetry/tgrpc"
	"github.com/lubobill1990/longarm/internal/version"
)

// TelemetryConfig is [telemetry] in longarmd.toml: the worker owner's
// choice (docs/telemetry.md). The hub cannot turn it on.
type TelemetryConfig struct {
	// Enabled = false: nothing is collected or sent. Default: on when the
	// hub collects telemetry.
	Enabled *bool `toml:"enabled"`
}

// TelemetryDir is the queue, hub state and crash reports in the data dir.
const TelemetryDir = "telemetry"

// workerTelemetry is longarmd's telemetry: the client, the names its
// scrubber learns from callers, and the pusher to the hub.
type workerTelemetry struct {
	tel   *telemetry.Client
	id    telemetry.Identity
	extra map[string]string
	mu    sync.Mutex
	known map[string]bool
	// prev job states (job lifecycle)
	jobs          map[string]string
	accountPrefix string
}

// startTelemetry sets up the worker's telemetry (nil when off: no hub,
// or [telemetry] enabled = false).
func startTelemetry(cfg Config, bundle *pki.Bundle, hubOn bool) *workerTelemetry {
	disabled := cfg.Telemetry.Enabled != nil && !*cfg.Telemetry.Enabled
	if !hubOn || disabled {
		return nil
	}
	w := &workerTelemetry{
		id: telemetry.Identity{CA: tgrpc.CADER(filepath.Join(cfg.DataDir, TLSDir)), Node: bundle.Identity.Name,
			Role: string(bundle.Identity.Role)},
		extra: map[string]string{}, known: map[string]bool{}, jobs: map[string]string{},
	}
	prefix := cfg.Isolation.AccountPrefix
	if prefix == "" {
		prefix = "la-"
	}
	if a := cfg.Access; a != nil {
		for _, u := range append(append([]string(nil), a.Users...), a.Owners...) {
			w.extra[u], w.extra[prefix+u] = pii.KindUser, pii.KindUser
		}
	}
	w.tel = telemetry.Start(telemetry.Config{
		Component: "longarmd", Version: version.Version, Dir: filepath.Join(cfg.DataDir, TelemetryDir),
		Identity: w.id, Names: w.extra,
	})
	w.accountPrefix = prefix
	return w
}

// learn adds a caller's names to the scrubber (client device and user
// names appear in log lines and errors).
func (w *workerTelemetry) learn(id pki.Identity) {
	if w == nil || id.Name == "" {
		return
	}
	w.mu.Lock()
	if w.known[id.Name] && w.known["user/"+id.Owner()] {
		w.mu.Unlock()
		return
	}
	w.known[id.Name], w.known["user/"+id.Owner()] = true, true
	kind := pii.KindNode
	w.extra[id.Name] = kind
	if u := id.Owner(); u != "" && u != id.Name {
		w.extra[u] = pii.KindUser
		w.extra[w.accountPrefix+u] = pii.KindUser
	}
	names := make(map[string]string, len(w.extra))
	for k, v := range w.extra {
		names[k] = v
	}
	w.mu.Unlock()
	w.tel.SetScrubber(telemetry.NewScrubber(w.id, names))
}

func callerIdentity(ctx context.Context) (pki.Identity, bool) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return pki.Identity{}, false
	}
	ti, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(ti.State.PeerCertificates) == 0 {
		return pki.Identity{}, false
	}
	id, err := pki.IdentityFromCert(ti.State.PeerCertificates[0])
	return id, err == nil
}

// interceptors are the server interceptors: trace context + requests,
// then name learning.
func (w *workerTelemetry) interceptors() ([]grpc.UnaryServerInterceptor, []grpc.StreamServerInterceptor) {
	if w == nil {
		return nil, nil
	}
	u := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		if id, ok := callerIdentity(ctx); ok {
			w.learn(id)
		}
		return h(ctx, req)
	}
	s := func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, h grpc.StreamHandler) error {
		if id, ok := callerIdentity(ss.Context()); ok {
			w.learn(id)
		}
		return h(srv, ss)
	}
	get := func() *telemetry.Client { return w.tel }
	return []grpc.UnaryServerInterceptor{u, tgrpc.UnaryServer(get)}, []grpc.StreamServerInterceptor{s, tgrpc.StreamServer(get)}
}

// run pushes the queue to the hub every 30 s (its own hub connection,
// apart from the agent's session) and ticks the aggregates.
func (w *workerTelemetry) run(ctx context.Context, addr string, live *pki.Bundle, log *slog.Logger, started time.Time) {
	if w == nil {
		return
	}
	var conn *hubclient.Conn
	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()
	go w.tel.Run(ctx)
	p := &tgrpc.Pusher{Tel: w.tel, Log: log, BeforeStop: func() { w.stopEvent(started) }, Conn: func(context.Context) grpc.ClientConnInterface {
		if conn == nil {
			c, err := hubclient.Dial(addr, live)
			if err != nil {
				return nil
			}
			conn = c
		}
		return conn
	}}
	p.Run(ctx)
}

// jobChanged records job lifecycle items: started, ended (state, exit
// code class, duration, reservation). Never the command line.
func (w *workerTelemetry) jobChanged(j supervisor.Job, first bool) {
	if w == nil {
		return
	}
	st := string(j.Status.State)
	w.mu.Lock()
	prev, had := w.jobs[j.Spec.ID]
	w.jobs[j.Spec.ID] = st
	if len(w.jobs) > 5000 {
		for k, v := range w.jobs {
			if v == "done" || v == "failed" || v == "killed" || v == "lost" {
				delete(w.jobs, k)
			}
		}
	}
	w.mu.Unlock()
	if first || had && prev == st {
		return
	}
	ctx := telemetry.Recall(context.Background(), "job/"+j.Spec.ID)
	w.tel.Count("job.state", map[string]string{"state": st}, 1)
	if !j.Status.State.Terminal() {
		return
	}
	props := map[string]string{"state": st, "exit": exitClass(j.Status.ExitCode), "pty": strconv.FormatBool(j.Spec.Pty),
		"workspace": strconv.FormatBool(j.Spec.Workspace != nil), "orphaned": strconv.FormatBool(j.Status.Orphaned)}
	if j.Status.Reason != "" && j.Status.State == "lost" {
		props["reason"] = j.Status.Reason
	}
	meas := map[string]float64{"mem_limit_gb": float64(j.Spec.MemLimitBytes) / (1 << 30), "cpu_limit": j.Spec.CPULimit}
	if !j.Status.StartedAt.IsZero() && !j.Status.EndedAt.IsZero() {
		meas["duration_s"] = j.Status.EndedAt.Sub(j.Status.StartedAt).Seconds()
	}
	if !j.Status.CreatedAt.IsZero() && !j.Status.StartedAt.IsZero() {
		meas["start_delay_s"] = j.Status.StartedAt.Sub(j.Status.CreatedAt).Seconds()
	}
	name := "job.ended"
	if j.Spec.Steer != nil {
		props["steer"] = j.Spec.Steer.Agent // a steerable run: a job on a terminal
	} else if j.Spec.Pty {
		name = "session.ended"
	}
	w.tel.Event(ctx, name, props, meas)
}

// exitClass groups exit codes: ok, error (1-125), not_found (126/127),
// signal (128+), unknown.
func exitClass(code *int) string {
	switch {
	case code == nil:
		return "unknown"
	case *code == 0:
		return "ok"
	case *code == 126 || *code == 127:
		return "not_found"
	case *code > 128 && *code < 256:
		return "signal"
	case *code < 0 || *code > 255:
		return "large" // Windows NTSTATUS and the like
	}
	return "error"
}

// event records an event of the worker.
func (w *workerTelemetry) event(name string, props map[string]string) {
	if w == nil {
		return
	}
	w.tel.Event(context.Background(), name, props, nil)
}

// stop records the stop event; the pusher's last push sends it.
func (w *workerTelemetry) stopEvent(started time.Time) {
	if w == nil {
		return
	}
	w.tel.Event(context.Background(), "process.stop", nil, map[string]float64{"uptime_s": time.Since(started).Seconds()})
}

// client is the telemetry client (nil = off).
func (w *workerTelemetry) client() *telemetry.Client {
	if w == nil {
		return nil
	}
	return w.tel
}
