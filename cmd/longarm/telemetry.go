package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/telemetry"
	"github.com/lubobill1990/longarm/internal/telemetry/tgrpc"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Telemetry of longarm (docs/telemetry.md). A CLI run is one operation:
// its trace id goes to every worker and hub call (traceparent) and is
// printed with errors ("(trace 4bf92f35)"). The run's items are queued in
// <config>/telemetry and sent at exit only when a hub connection is open
// anyway (tight timeout), otherwise by the next run or longarm daemon.
// longarm daemon and longarm mcp push the queue every 30 s.
var cli struct {
	span  telemetry.Span
	tel   *telemetry.Client
	off   string // why telemetry is off
	cmd   string // command name ("run", "admin user add")
	long  bool   // daemon run / mcp: a long-running process
	start time.Time
	stopC func() // crash watch

	mu      sync.Mutex
	exitHub *hubclient.Conn // kept open by session.close for the exit flush
}

// cliCommandName is the command path without the binary and arguments.
func cliCommandName(cmd *cobra.Command) string {
	return strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()+" ")
}

// noTelemetry: commands that record nothing (looking at telemetry itself,
// help, shell completion).
func noTelemetry(name string) bool {
	first, _, _ := strings.Cut(name, " ")
	switch first {
	case "telemetry", "help", "completion", "__complete", "__completeNoDesc", "longarm":
		return true
	}
	return false
}

// startTelemetry runs before every command (root PersistentPreRun).
func startTelemetry(cmd *cobra.Command) {
	name := cliCommandName(cmd)
	if noTelemetry(name) || cli.tel != nil {
		return
	}
	cfg, off := client.TelemetryConfig(configDir, "longarm")
	cli.cmd, cli.off = name, off
	cli.long = name == "daemon run" || name == "mcp"
	cfg.Detailed = !cli.long
	cli.tel = telemetry.Start(cfg)
	if cli.long {
		// every MCP tool call / daemon request is its own operation
		telemetry.SetFallback(telemetry.Span{})
		cli.stopC = cli.tel.WatchCrashes()
		cli.tel.Event(context.Background(), "process.start", map[string]string{"command": name}, nil)
	}
}

// finishTelemetry records the command's outcome and queues it; with a hub
// connection still open it also sends the queue (at most a second).
func finishTelemetry(code int, err error) {
	tel := cli.tel
	if tel == nil {
		return
	}
	ctx := telemetry.WithSpan(context.Background(), cli.span)
	if cli.long {
		tel.Event(ctx, "process.stop", map[string]string{"command": cli.cmd}, map[string]float64{"uptime_s": tel.Uptime().Seconds()})
		if cli.stopC != nil {
			cli.stopC()
		}
	} else {
		tel.Request(ctx, telemetry.Request{Name: cli.cmd, Span: cli.span, Duration: time.Since(cli.start),
			Code: strconv.Itoa(code), Success: code != exitLongarmError, Always: true})
		if err != nil && code == exitLongarmError {
			tel.Exception(ctx, errType(err), err.Error(), nil, map[string]string{"command": cli.cmd})
		}
	}
	tel.Close()
	cli.mu.Lock()
	h := cli.exitHub
	cli.exitHub = nil
	cli.mu.Unlock()
	if h != nil {
		pctx, cancel := context.WithTimeout(context.Background(), time.Second)
		tgrpc.PushQueue(pctx, tel, pb.NewTelemetryClient(h))
		cancel()
		h.Close()
	}
}

// errType classifies an error of longarm for the exception's type.
func errType(err error) string {
	if _, ok := client.TrustRefusal(err); ok {
		return "trust_refused"
	}
	if s, ok := status.FromError(err); ok {
		return "grpc." + s.Code().String()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "error"
}

// keepHubForExit keeps an open hub connection for finishTelemetry instead
// of closing it (reports whether it did).
func keepHubForExit(c *hubclient.Conn) bool {
	if cli.tel == nil || cli.long || !cli.tel.Enabled() && !cli.tel.RecheckDue() || c.GetState() != connectivity.Ready {
		return false
	}
	cli.mu.Lock()
	defer cli.mu.Unlock()
	if cli.exitHub != nil {
		return false
	}
	cli.exitHub = c
	return true
}

// traceNote is appended to error messages: the id a bug report quotes.
func traceNote() string {
	if !cli.span.Valid() || cli.long {
		return ""
	}
	return " (trace " + cli.span.Short() + ")"
}

// runPusher pushes the queue every 30 s from long-running processes
// (longarm daemon, longarm mcp) over the session's hub connection; wait
// blocks until the last push is done.
func runPusher(ctx context.Context, s *session) (wait func()) {
	tel := cli.tel
	if tel == nil || tel.Config().Disabled {
		return func() {}
	}
	done := make(chan struct{})
	go tel.Run(ctx)
	go func() {
		defer close(done)
		(&tgrpc.Pusher{Tel: tel, Conn: func(context.Context) grpc.ClientConnInterface {
			if h := s.hub(); h != nil {
				return h
			}
			return nil
		}}).Run(ctx)
	}()
	return func() {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
}

// ---------------------------------------------------------------- longarm telemetry

func telemetryCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "telemetry",
		Short: "Telemetry of this machine: status, what is queued, opt out (docs/telemetry.md)",
		Long: `Longarm sends anonymous, scrubbed telemetry (errors, crashes, command names and
durations, connection outcomes) to its hub, which forwards it to the hub
operator's Azure Application Insights - only when the hub collects telemetry.
No command lines, job output, file paths, environment, addresses, host or user
names leave this machine (names become keyed hashes). docs/telemetry.md lists
everything that is sent.

Turn it off on this machine with "longarm telemetry off" or LONGARM_TELEMETRY=0.`,
	}
	dir := func() string { return filepath.Join(configDir, client.TelemetryDir) }
	set := func(disabled bool) error {
		if err := telemetry.SaveSettings(dir(), telemetry.Settings{Disabled: disabled}); err != nil {
			return err
		}
		if disabled {
			fmt.Println("telemetry off on this machine (queue dropped)")
		} else {
			fmt.Println("telemetry on (sent only when the hub collects it)")
		}
		return nil
	}
	off := &cobra.Command{Use: "off", Short: "Turn telemetry off on this machine and drop the queue", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return set(true) }}
	on := &cobra.Command{Use: "on", Short: "Turn telemetry back on (the default)", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return set(false) }}
	st := &cobra.Command{Use: "status", Short: "Show whether telemetry is on, the pseudonyms and the queue", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			cfg, why := client.TelemetryConfig(configDir, "longarm")
			tel := telemetry.New(cfg)
			state := "on"
			switch {
			case why != "":
				state = "off: " + why
			case !tel.Enabled():
				state = "off: the hub does not collect telemetry (asks again later)"
			}
			ctx := tel.Context()
			fmt.Printf("telemetry  %s\n", state)
			if st, err := telemetry.LoadState(cfg.Dir); err == nil && st.Collecting != nil {
				fmt.Printf("hub        collects: %v (checked %s)\n", *st.Collecting, st.Checked.Local().Format(time.RFC3339))
			} else {
				fmt.Printf("hub        not asked yet\n")
			}
			fmt.Printf("fleet      %s\nnode       %s\nuser       %s\n", dash(ctx.Fleet), dash(ctx.Node), dash(ctx.User))
			q := &telemetry.Queue{Dir: cfg.Dir}
			n, size := q.Stat()
			fmt.Printf("queue      %d items, %d bytes (%s)\n", n, size, filepath.Join(cfg.Dir, telemetry.QueueFile))
			crashes, _ := filepath.Glob(filepath.Join(cfg.Dir, "crash-*.txt"))
			if len(crashes) > 0 {
				fmt.Printf("crashes    %d report(s) waiting\n", len(crashes))
			}
			return nil
		}}
	show := &cobra.Command{Use: "show", Short: "Print the queued items: exactly what would be sent (JSON, one per line)", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			items, err := (&telemetry.Queue{Dir: dir()}).Items()
			if err != nil {
				return err
			}
			enc := json.NewEncoder(os.Stdout)
			for _, it := range items {
				enc.Encode(it)
			}
			if len(items) == 0 {
				fmt.Fprintln(os.Stderr, "longarm: the telemetry queue is empty")
			}
			return nil
		}}
	flush := &cobra.Command{Use: "flush", Short: "Send the queue to the hub now", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, why := client.TelemetryConfig(configDir, "longarm")
			if why != "" {
				return fmt.Errorf("telemetry is off: %s", why)
			}
			tel := telemetry.New(cfg)
			s, err := openSession()
			if err != nil {
				return err
			}
			defer s.close()
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
			defer cancel()
			h := s.optionalHub(ctx)
			if h == nil {
				return errors.New("the hub is not reachable")
			}
			n, err := tgrpc.PushQueue(ctx, tel, pb.NewTelemetryClient(h))
			if err != nil {
				return err
			}
			if !tel.Enabled() {
				fmt.Println("the hub does not collect telemetry; queue dropped")
				return nil
			}
			fmt.Printf("sent %d items\n", n)
			return nil
		}}
	c.AddCommand(st, show, on, off, flush)
	return c
}
