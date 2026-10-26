package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/fwcheck"
	"github.com/lubobill1990/longarm/internal/localapi"
	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/pool"
	"github.com/lubobill1990/longarm/internal/version"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// longarm daemon is the client-side session daemon (DESIGN §11.3): it hosts
// long-lived port forwards (they outlive the longarm / MCP process that asked)
// and relays the hub's fleet events (notifications for a tray). It serves
// the local control API (LocalClient) on a channel only this user can
// reach; every operation is also a CLI command:
//
//	longarm daemon start | stop | status | events
//	longarm forward <w> <port> --bg | --list | --stop
const daemonAddrFile = "daemon.addr"

type daemon struct {
	pb.UnimplementedLocalClientServer
	s       *session
	started time.Time
	cancel  context.CancelFunc

	mu       sync.Mutex
	forwards map[string]*daemonForward // by local address
	link     *pb.HubLink
	subs     map[chan *pb.FleetEvent]struct{}

	mon    *pool.Monitor // pool view + latency monitor (daemon_pool.go)
	fw     fwcheck.Cache // Windows Firewall verdict for this longarm.exe
	connMu sync.Mutex
	conns  map[string]*client.Conn // direct job operations, by worker
}

type daemonForward struct {
	info  *pb.ForwardInfo
	ln    net.Listener
	conn  *client.Conn
	count atomic.Int64
	lastE atomic.Value // string
}

func (d *daemon) Status(context.Context, *pb.LocalStatusRequest) (*pb.ClientStatus, error) {
	fw := d.fw.Get() // may read the firewall rules: not under d.mu
	d.mu.Lock()
	defer d.mu.Unlock()
	return &pb.ClientStatus{Identity: d.s.dialer.Identity().String(), Hub: proto.Clone(d.link).(*pb.HubLink),
		Forwards: int32(len(d.forwards)), StartedAt: timestamppb.New(d.started), Pid: int32(os.Getpid()),
		Version: version.Version, ConfigDir: d.s.cfg.Dir, Transport: d.s.dialer.Policy(), Firewall: fw}, nil
}

func (d *daemon) StartForward(ctx context.Context, req *pb.StartForwardRequest) (*pb.ForwardInfo, error) {
	if req.Worker == "" || req.Target == "" {
		return nil, status.Error(codes.InvalidArgument, "worker and target are required")
	}
	bind := req.Bind
	if bind == "" {
		bind = "127.0.0.1"
	}
	conn, err := d.s.conn(req.Worker)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	var ln net.Listener
	port := int(req.LocalPort)
	if port == 0 { // same port as the target if free, else any
		if i := strings.LastIndex(req.Target, ":"); i >= 0 {
			port, _ = strconv.Atoi(req.Target[i+1:])
		} else {
			port, _ = strconv.Atoi(req.Target)
		}
		if ln, err = net.Listen("tcp", net.JoinHostPort(bind, strconv.Itoa(port))); err != nil {
			ln, err = net.Listen("tcp", net.JoinHostPort(bind, "0"))
		}
	} else {
		ln, err = net.Listen("tcp", net.JoinHostPort(bind, strconv.Itoa(port)))
	}
	if err != nil {
		conn.Close()
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	addr := ln.Addr().String()
	f := &daemonForward{ln: ln, conn: conn, info: &pb.ForwardInfo{Worker: req.Worker, Target: req.Target, LocalAddr: addr,
		Url: "http://" + addr + "/", StartedAt: timestamppb.Now()}}
	d.mu.Lock()
	d.forwards[addr] = f
	d.mu.Unlock()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.count.Add(1)
			go func() {
				if err := forwardConn(context.Background(), conn, req.Target, c); err != nil {
					f.lastE.Store(oneLine(err))
				}
			}()
		}
	}()
	fmt.Fprintf(os.Stderr, "longarm daemon: forwarding %s -> %s:%s\n", addr, req.Worker, req.Target)
	return d.forwardInfo(f), nil
}

func (d *daemon) forwardInfo(f *daemonForward) *pb.ForwardInfo {
	info := proto.Clone(f.info).(*pb.ForwardInfo)
	info.Connections = f.count.Load()
	if e, ok := f.lastE.Load().(string); ok {
		info.Error = e
	}
	return info
}

func (d *daemon) ListForwards(context.Context, *pb.ListForwardsRequest) (*pb.ListForwardsResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := &pb.ListForwardsResponse{}
	for _, f := range d.forwards {
		out.Forwards = append(out.Forwards, d.forwardInfo(f))
	}
	sort.Slice(out.Forwards, func(i, j int) bool { return out.Forwards[i].LocalAddr < out.Forwards[j].LocalAddr })
	return out, nil
}

func (d *daemon) StopForward(ctx context.Context, req *pb.StopForwardRequest) (*pb.ListForwardsResponse, error) {
	d.mu.Lock()
	var stopped []*pb.ForwardInfo
	for addr, f := range d.forwards {
		_, port, _ := net.SplitHostPort(addr)
		if req.LocalAddr != "all" && req.LocalAddr != addr && req.LocalAddr != port {
			continue
		}
		f.ln.Close()
		f.conn.Close()
		delete(d.forwards, addr)
		stopped = append(stopped, d.forwardInfo(f))
	}
	d.mu.Unlock()
	if len(stopped) == 0 {
		return nil, status.Errorf(codes.NotFound, "no forward on %s", req.LocalAddr)
	}
	return &pb.ListForwardsResponse{Forwards: stopped}, nil
}

func (d *daemon) Events(req *pb.EventsRequest, st pb.LocalClient_EventsServer) error {
	ch := make(chan *pb.FleetEvent, 256)
	d.mu.Lock()
	d.subs[ch] = struct{}{}
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.subs, ch)
		d.mu.Unlock()
	}()
	for {
		select {
		case <-st.Context().Done():
			return nil
		case ev := <-ch:
			if len(req.Kinds) > 0 && !contains(req.Kinds, ev.Kind) || len(req.Kinds) == 0 && ev.Kind == "metrics" {
				continue
			}
			if req.JobId != "" && ev.Job.GetJob().GetSpec().GetId() != req.JobId {
				continue
			}
			if err := st.Send(ev); err != nil {
				return err
			}
		}
	}
}

func (d *daemon) Shutdown(context.Context, *pb.ShutdownRequest) (*pb.ShutdownResponse, error) {
	go func() {
		time.Sleep(100 * time.Millisecond)
		d.cancel()
	}()
	return &pb.ShutdownResponse{}, nil
}

// relayHubEvents keeps a Fleet.Events subscription and fans it out.
func (d *daemon) relayHubEvents(ctx context.Context) {
	wait := time.Second
	for ctx.Err() == nil {
		h := d.s.hub()
		if h == nil {
			return
		}
		st, err := h.Fleet().Events(ctx, &pb.EventsRequest{Kinds: []string{"job", "node", "queue", "metrics"}})
		if err == nil {
			d.setLink(true, nil)
			if nodes, ok := d.s.hubNodes(ctx, false); ok {
				for _, n := range nodes {
					d.mon.HubNode(n)
				}
			}
			start := time.Now()
			for {
				var ev *pb.FleetEvent
				if ev, err = st.Recv(); err != nil {
					break
				}
				if ev.NodeInfo != nil && (ev.Kind == "node" || ev.Kind == "metrics") {
					d.mon.HubNode(ev.NodeInfo)
				}
				d.mu.Lock()
				for ch := range d.subs {
					select {
					case ch <- ev:
					default:
					}
				}
				d.mu.Unlock()
			}
			if time.Since(start) > 30*time.Second {
				wait = time.Second
			}
		}
		if ctx.Err() != nil {
			return
		}
		d.setLink(false, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if wait *= 2; wait > 30*time.Second {
			wait = 30 * time.Second
		}
	}
}

func (d *daemon) setLink(connected bool, err error) {
	if d.mon != nil {
		d.mon.SetHubUp(connected)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.link.Connected != connected || d.link.Since == nil {
		d.link.Since = timestamppb.Now()
	}
	d.link.Connected = connected
	d.link.Error = ""
	if err != nil {
		d.link.Error = oneLine(err)
	}
}

// maintain renews the client certificate when due and picks up a renewed
// one written by another longarm process.
func (d *daemon) maintain(ctx context.Context) {
	certPath := filepath.Join(d.s.cfg.Dir, client.TLSDir, "node.crt")
	stamp := func() time.Time {
		fi, err := os.Stat(certPath)
		if err != nil {
			return time.Time{}
		}
		return fi.ModTime()
	}
	last := stamp()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if h := d.s.hub(); h != nil {
			d.s.maybeRenew(ctx, h)
		}
		if st := stamp(); !st.Equal(last) {
			last = st
			if err := d.s.dialer.Reload(); err != nil {
				fmt.Fprintf(os.Stderr, "longarm daemon: reload certificate: %v\n", err)
			}
		}
	}
}

func (d *daemon) gateway() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		s, err := d.Status(r.Context(), nil)
		localapi.WriteProto(w, s, err)
	})
	mux.HandleFunc("GET /v1/forwards", func(w http.ResponseWriter, r *http.Request) {
		s, err := d.ListForwards(r.Context(), nil)
		localapi.WriteProto(w, s, err)
	})
	mux.HandleFunc("POST /v1/forwards", func(w http.ResponseWriter, r *http.Request) {
		req := &pb.StartForwardRequest{}
		if err := localapi.ReadProto(r, req); err != nil {
			localapi.WriteProto(w, nil, err)
			return
		}
		s, err := d.StartForward(r.Context(), req)
		localapi.WriteProto(w, s, err)
	})
	mux.HandleFunc("DELETE /v1/forwards/{addr}", func(w http.ResponseWriter, r *http.Request) {
		s, err := d.StopForward(r.Context(), &pb.StopForwardRequest{LocalAddr: r.PathValue("addr")})
		localapi.WriteProto(w, s, err)
	})
	mux.HandleFunc("POST /v1/shutdown", func(w http.ResponseWriter, r *http.Request) {
		s, err := d.Shutdown(r.Context(), nil)
		localapi.WriteProto(w, s, err)
	})
	d.poolGateway(mux)
	// One-shot longarm processes dial their tailscale / longnet legs through the
	// daemon's long-lived nodes.
	mux.Handle("POST /v1/legs/dial", client.LegProxyHandler(d.s.dialer, d.s.worker))
	// ... and ask how the daemon measures the legs before racing them.
	mux.Handle("GET /v1/legs/live", client.LiveLegsHandler(d.liveLegs))
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		ch := make(chan *pb.FleetEvent, 256)
		d.mu.Lock()
		d.subs[ch] = struct{}{}
		d.mu.Unlock()
		defer func() {
			d.mu.Lock()
			delete(d.subs, ch)
			d.mu.Unlock()
		}()
		localapi.SSE(w, r, "fleet", func(ctx context.Context) (proto.Message, error) {
			for {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case ev := <-ch:
					if ev.Kind == "metrics" {
						continue
					}
					return ev, nil
				}
			}
		})
	})
	return mux
}

// runDaemon serves until stopped.
func runDaemon(ctx context.Context) error {
	s, err := openSession()
	if err != nil {
		return err
	}
	defer s.close()
	quietHub = true
	// The daemon is the leg proxy itself and lives long: its own nodes,
	// kept up (the tailscale node starts once the channel is ours).
	s.dialer.Proxy, s.dialer.OneShot = nil, false
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	d := &daemon{s: s, started: time.Now(), cancel: cancel, forwards: map[string]*daemonForward{},
		link: &pb.HubLink{Address: s.cfg.Hub.Address}, subs: map[chan *pb.FleetEvent]struct{}{},
		conns: map[string]*client.Conn{}}
	d.mon = d.newMonitor()
	s.dialer.LiveLegs = func(_ context.Context, worker string) []client.LiveLeg { return d.liveLegs(worker) }
	addrFile := filepath.Join(configDir, daemonAddrFile)
	if c, err := localapi.DialFile(addrFile, "longarm daemon"); err == nil {
		cctx, ccancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := pb.NewLocalClientClient(c).Status(cctx, &pb.LocalStatusRequest{})
		ccancel()
		c.Close()
		if err == nil {
			return errors.New("longarm daemon is already running for " + configDir)
		}
	}
	ln, addr, err := localapi.Listen("longarm", configDir, addrFile)
	if err != nil {
		return err
	}
	defer os.Remove(addrFile)
	s.dialer.WarmTailnet()
	g := grpc.NewServer()
	pb.RegisterLocalClientServer(g, d)
	if s.cfg.Hub.Enabled() {
		go d.relayHubEvents(ctx)
	}
	go d.maintain(ctx)
	go s.dialer.WatchTrust(ctx, nil)
	monDone := make(chan struct{})
	go func() { d.mon.Run(ctx); close(monDone) }()
	go d.poolSideLoops(ctx)
	defer func() { cancel(); <-monDone; d.closeConns() }()
	fmt.Fprintf(os.Stderr, "longarm daemon %s listening on %s (pid %d)\n", version.Version, addr, os.Getpid())
	err = localapi.Serve(ctx, ln, g, d.gateway())
	d.mu.Lock()
	for _, f := range d.forwards {
		f.ln.Close()
		f.conn.Close()
	}
	d.mu.Unlock()
	return err
}

// daemonClient connects to the running daemon (starting it if asked).
func daemonClient(start bool) (pb.LocalClientClient, func(), error) {
	addrFile := filepath.Join(configDir, daemonAddrFile)
	try := func() (pb.LocalClientClient, func(), error) {
		cc, err := localapi.DialFile(addrFile, "longarm daemon")
		if err != nil {
			return nil, nil, err
		}
		c := pb.NewLocalClientClient(cc)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := c.Status(ctx, &pb.LocalStatusRequest{}); err != nil {
			cc.Close()
			return nil, nil, fmt.Errorf("longarm daemon not reachable: %w", err)
		}
		return c, func() { cc.Close() }, nil
	}
	c, closeFn, err := try()
	if err == nil || !start {
		return c, closeFn, err
	}
	if err := startDaemonProcess(); err != nil {
		return nil, nil, err
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if c, closeFn, err = try(); err == nil {
			return c, closeFn, nil
		}
		if time.Now().After(deadline) {
			return nil, nil, fmt.Errorf("longarm daemon did not start: %w (see %s)", err, filepath.Join(configDir, "daemon.log"))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func startDaemonProcess() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logPath := filepath.Join(configDir, "daemon.log")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return err
	}
	_, err = platform.StartDetached(func() *exec.Cmd {
		cmd := exec.Command(exe, "--config-dir", configDir, "daemon", "run", "--log-file", logPath)
		return cmd
	})
	return err
}

func daemonCmd() *cobra.Command {
	c := &cobra.Command{Use: "daemon", Short: "Client session daemon: hosts background port forwards and relays hub events (local control API)"}
	var logFile string
	run := &cobra.Command{
		Use: "run", Short: "Run the daemon in the foreground", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if logFile != "" {
				f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
				if err != nil {
					return err
				}
				defer f.Close()
				os.Stderr = f
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runDaemon(ctx)
		},
	}
	run.Flags().StringVar(&logFile, "log-file", "", "write the log here")
	start := &cobra.Command{
		Use: "start", Short: "Start the daemon in the background (no-op if it runs)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, closeFn, err := daemonClient(true)
			if err != nil {
				return err
			}
			closeFn()
			fmt.Println("longarm daemon running")
			return nil
		},
	}
	stop := &cobra.Command{
		Use: "stop", Short: "Stop the daemon (and its forwards)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, closeFn, err := daemonClient(false)
			if err != nil {
				return err
			}
			defer closeFn()
			_, err = c.Shutdown(cmd.Context(), &pb.ShutdownRequest{})
			return err
		},
	}
	var asJSON bool
	st := &cobra.Command{
		Use: "status", Short: "Show the daemon's state", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, closeFn, err := daemonClient(false)
			if err != nil {
				return err
			}
			defer closeFn()
			s, err := c.Status(cmd.Context(), &pb.LocalStatusRequest{})
			if err != nil {
				return err
			}
			if asJSON {
				b, _ := protojson.MarshalOptions{Multiline: true, Indent: "  ", UseProtoNames: true}.Marshal(s)
				fmt.Println(string(b))
				return nil
			}
			hub := "none"
			if s.Hub.GetAddress() != "" {
				hub = s.Hub.Address + map[bool]string{true: " (subscribed)", false: " (disconnected: " + s.Hub.Error + ")"}[s.Hub.Connected]
			}
			fmt.Printf("longarm daemon %s pid %d since %s\nidentity %s\nhub      %s\nforwards %d\n", s.Version, s.Pid,
				s.StartedAt.AsTime().Local().Format(time.RFC3339), s.Identity, hub, s.Forwards)
			switch fw := s.Firewall; {
			case fw == nil: // not Windows
			case fw.Ok:
				fmt.Printf("firewall ok (inbound allow rule for %s)\n", fw.Program)
			default:
				fmt.Printf("firewall PROBLEM: %s\n         fix (elevated): %s\n", fw.Problem, fw.Fix)
			}
			return nil
		},
	}
	st.Flags().BoolVar(&asJSON, "json", false, "JSON output")
	events := &cobra.Command{
		Use: "events", Short: "Stream the fleet events the daemon relays from the hub", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, closeFn, err := daemonClient(true)
			if err != nil {
				return err
			}
			defer closeFn()
			es, err := c.Events(cmd.Context(), &pb.EventsRequest{})
			if err != nil {
				return err
			}
			for {
				ev, err := es.Recv()
				if err != nil {
					return err
				}
				b, _ := protojson.MarshalOptions{UseProtoNames: true}.Marshal(ev)
				fmt.Println(string(b))
			}
		},
	}
	c.AddCommand(run, start, stop, st, events)
	return c
}

// daemonForwards lists the daemon's forwards (nil if it does not run).
func daemonForwards() []*pb.ForwardInfo {
	c, closeFn, err := daemonClient(false)
	if err != nil {
		return nil
	}
	defer closeFn()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := c.ListForwards(ctx, &pb.ListForwardsRequest{})
	if err != nil {
		return nil
	}
	return r.Forwards
}

func printDaemonForwards(tw *tabwriter.Writer) {
	for _, f := range daemonForwards() {
		fmt.Fprintf(tw, "%s\t%s\t%s\tdaemon\t%s\n", f.LocalAddr, f.Worker, f.Target, f.StartedAt.AsTime().Local().Format("01-02 15:04:05"))
	}
}
